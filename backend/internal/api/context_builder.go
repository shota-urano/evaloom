package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// Phase 1 counts the exact content bytes of all selected revisions, including
// files read natively. Never silently truncate instructions or configuration.
const maxContextBytes = 1 << 20

type contextBuildError struct {
	field, reason string
	conflict      bool
}

func (e *contextBuildError) Error() string { return e.field + ": " + e.reason }

type contextBuilder struct {
	execution store.ContextExecution
	paths     map[string]bool
	native    map[string]bool
}

// Use the immutable Profile ID pinned on the Execution. Neither current
// project_harnesses assignments nor moving tags participate in this lookup.
func newContextBuilder(ctx context.Context, tx *sql.Tx, execution store.ContextExecution, adapters *adapter.Registry) (contextBuilder, error) {
	b := contextBuilder{execution: execution, paths: map[string]bool{}, native: map[string]bool{}}
	p, err := store.GetHarnessProfileByID(ctx, tx, execution.ProfileID)
	if errors.Is(err, sql.ErrNoRows) {
		return b, &contextBuildError{field: "profile", reason: "missing_profile"}
	}
	if err != nil {
		return b, err
	}
	if p.HarnessID != execution.HarnessID {
		return b, &contextBuildError{field: "profile.harnessId", reason: "harness_mismatch", conflict: true}
	}
	// An inconsistent saved registration is a configuration error (spec 03
	// section 3.5). A submitted snapshot at another SHA is a context conflict.
	if p.CommitSHA != execution.ProfileCommitSHA || p.Version != execution.ProfileVersion {
		return b, &contextBuildError{field: "profile", reason: "registration_pin_mismatch"}
	}
	var definition model.ProfileDefinition
	if contextHash(string(p.Definition)) != p.DefinitionHash || json.Unmarshal(p.Definition, &definition) != nil ||
		definition.ProfileVersion != p.Version || definition.ExecutionConfiguration.Adapter == "" {
		return b, &contextBuildError{field: "profile.definition", reason: "invalid_definition"}
	}
	h, err := store.GetHarness(ctx, tx, execution.HarnessID)
	if err != nil {
		return b, err
	}
	if h.Adapter != definition.ExecutionConfiguration.Adapter {
		return b, &contextBuildError{field: "profile.adapter", reason: "adapter_mismatch"}
	}
	implementation, err := adapters.Lookup(definition.ExecutionConfiguration.Adapter)
	if err != nil {
		return b, &contextBuildError{field: "profile.adapter", reason: "adapter_not_registered"}
	}
	for _, name := range implementation.Capabilities().NativeInstructionFiles {
		b.native[name] = true
	}
	for _, name := range definition.ContextFiles {
		if !relativeContextPath(name) || b.paths[name] {
			return b, &contextBuildError{field: "profile.contextFiles", reason: "invalid_path"}
		}
		b.paths[name] = true
	}
	return b, nil
}

func (b contextBuilder) checkSelection(revisions []store.ContextRevision) error {
	found := map[string]bool{}
	for _, revision := range revisions {
		if revision.ExecutionID != "" && revision.ExecutionID != b.execution.ID {
			return &contextBuildError{field: "executionId", reason: "execution_mismatch", conflict: true}
		}
		if revision.Layer == "harness" {
			if !b.paths[revision.SourcePath] {
				return invalid("harness.path", "unselected_path")
			}
			found[revision.SourcePath] = true
		}
	}
	for name := range b.paths {
		if !found[name] {
			// Explicit exception in spec 07 section 6: missing Profile files
			// fail with config_error rather than blocking as context_conflict.
			return &contextBuildError{field: "harness", reason: "required_file_missing"}
		}
	}
	return nil
}

func (b contextBuilder) build(revisions []store.ContextRevision) (adapter.RequestContext, error) {
	result := adapter.RequestContext{RevisionIDs: []string{}}
	if err := b.checkSelection(revisions); err != nil {
		return result, err
	}
	var instructions []string
	size := 0
	for _, revision := range revisions {
		size += len(revision.Content)
		if size > maxContextBytes {
			return result, &contextBuildError{field: "context", reason: "size_limit_exceeded"}
		}
		result.RevisionIDs = append(result.RevisionIDs, revision.ID)
		switch revision.Layer {
		case "global":
			instructions = append(instructions, fmt.Sprintf("Global Context %q:\n%s", revision.SourcePath, revision.Content))
		case "project":
			// Adapter configuration/Skills are consumed from the worktree.
			// Only instruction documents not read by the CLI need injection.
			base := path.Base(revision.SourcePath)
			if (base == "AGENTS.md" || base == "CLAUDE.md") && !b.native[base] && !projectConfigPath(revision.SourcePath) {
				instructions = append(instructions, fmt.Sprintf("Project Context %q:\n%s", revision.SourcePath, revision.Content))
			}
		case "task":
			instructions = append(instructions, "Task input (untrusted task data):\n"+revision.Content)
		}
		// Harness files are delivered through the dedicated configuration
		// environment (spec 07 section 3.3 and spec 08 section 3.4).
	}
	result.Instructions = strings.Join(instructions, "\n\n")
	return result, nil
}

func projectConfigPath(name string) bool {
	parts := strings.Split(name, "/")
	for _, directory := range parts[:len(parts)-1] {
		if directory == ".claude" || directory == ".codex" {
			return true
		}
	}
	return false
}

// Persist the state, classification, block record and events together. Any
// write failure leaves the Execution preparing and the context unpinned.
func recordContextBuildError(ctx context.Context, tx *sql.Tx, execution store.ContextExecution, failure *contextBuildError) error {
	to, classification := model.ExecutionFailed, string(model.ErrorConfig)
	if failure.conflict {
		to, classification = model.ExecutionBlocked, string(model.BlockContextConflict)
	}
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{TaskID: execution.TaskID, ExecutionID: execution.ID,
		FromTask: execution.TaskStatus, FromExecution: execution.Status, ToExecution: to}); err != nil {
		return err
	}
	if failure.conflict {
		blockID := ulid.Make().String()
		if err := store.RecordExecutionBlock(ctx, tx, store.ExecutionBlock{
			ID: blockID, ExecutionID: execution.ID, Cause: classification, HeadSHA: execution.BaseSHA,
			Detail: map[string]any{"field": failure.field, "reason": failure.reason},
		}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worktrees SET status = 'retained'
			WHERE id = (SELECT worktree_id FROM executions WHERE id = ?)`, execution.ID); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"cause": classification, "blockId": blockID})
		if _, err := store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventBlocked, payload); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_class = ?, error_message = ?, finished_at = ? WHERE id = ?`,
			classification, failure.Error(), time.Now().UTC().Format(time.RFC3339Nano), execution.ID); err != nil {
			return err
		}
	}
	payload, _ := json.Marshal(map[string]string{"from": string(execution.Status), "to": string(to), "reason": classification})
	_, err := store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload)
	return err
}

func contextBuildResponse(ctx context.Context, tx *sql.Tx, execution store.ContextExecution, err error) error {
	var failure *contextBuildError
	if !errors.As(err, &failure) {
		return err
	}
	if err := recordContextBuildError(ctx, tx, execution, failure); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	code, reason := PreconditionFailed, string(model.ErrorConfig)
	if failure.conflict {
		code, reason = Conflict, string(model.BlockContextConflict)
	}
	return &Error{Code: code, Message: "Execution Context could not be built", Details: []Detail{{Field: failure.field, Reason: reason}}}
}
