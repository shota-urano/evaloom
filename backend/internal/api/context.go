package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/projectcontext"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// ContextConfig selects Global files relative to the evaloom repository root.
// Every selected path must be below context/. Empty selection reads no files.
type ContextConfig struct {
	RepositoryRoot string
	GlobalPaths    []string
}

type contextInput struct {
	// The authenticated Runner reads the worktree HEAD immediately before
	// submitting Context. On continue this can differ from the pinned base.
	HeadSHA string            `json:"headSha"`
	Project []gitcontext.File `json:"project"`
	Harness []gitcontext.File `json:"harness"`
}

func pinExecutionContext(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, config ContextConfig, adapters *adapter.Registry) http.HandlerFunc {
	config.GlobalPaths = append([]string(nil), config.GlobalPaths...)
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		// Read the HTTP body before taking the database's sole connection.
		var input contextInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteError(w, logger, err)
			return
		}
		if input.Project == nil || input.Harness == nil {
			WriteError(w, logger, invalid("body", "project_and_harness_arrays_required"))
			return
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		execution, err := store.GetContextExecution(r.Context(), tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: NotFound, Message: "Execution not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		runner, _ := r.Context().Value(runnerIdentityKey{}).(string)
		if runner == "" || execution.RunnerID != runner {
			// Release the sole DB connection before recording the denial.
			tx.Rollback()
			auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot access this Execution")
			return
		}
		if !execution.Latest || execution.Status != model.ExecutionPreparing || execution.TaskStatus != model.TaskRunning {
			WriteError(w, logger, &Error{Code: Conflict, Message: "Context can only be pinned while the latest Execution is preparing"})
			return
		}
		if !fullCommitSHA(execution.BaseSHA) || !fullCommitSHA(execution.ProfileCommitSHA) || execution.ProfileID == "" || execution.HarnessID == "" || execution.ProfileVersion == "" {
			WriteError(w, logger, &Error{Code: PreconditionFailed, Message: "Execution is missing its context pins"})
			return
		}
		if !fullCommitSHA(input.HeadSHA) {
			WriteError(w, logger, invalid("headSha", "invalid_format"))
			return
		}
		submitted, err := submittedRevisions(input, execution)
		if err != nil {
			WriteError(w, logger, contextBuildResponse(r.Context(), tx, execution, input.HeadSHA, err))
			return
		}
		existing, err := store.ListContextRevisions(r.Context(), tx, id)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		if len(existing) > 0 {
			if !sameSubmission(existing, submitted) {
				WriteError(w, logger, &Error{Code: Conflict, Message: "Execution Context has already been pinned with different files"})
				return
			}
		}
		builder, err := newContextBuilder(r.Context(), tx, execution, adapters)
		if err != nil {
			WriteError(w, logger, contextBuildResponse(r.Context(), tx, execution, input.HeadSHA, err))
			return
		}
		if len(existing) > 0 {
			result, err := builder.build(existing)
			if err != nil {
				WriteError(w, logger, contextBuildResponse(r.Context(), tx, execution, input.HeadSHA, err))
				return
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		if err := builder.checkSelection(submitted); err != nil {
			WriteError(w, logger, contextBuildResponse(r.Context(), tx, execution, input.HeadSHA, err))
			return
		}
		globals, err := readGlobalContext(r.Context(), config)
		if err != nil {
			WriteError(w, logger, contextBuildResponse(r.Context(), tx, execution, input.HeadSHA, &contextBuildError{field: "global", reason: "read_failed"}))
			return
		}
		taskContent, err := json.Marshal(struct {
			Title              string `json:"title"`
			Body               string `json:"body"`
			AcceptanceCriteria string `json:"acceptanceCriteria"`
		}{execution.Title, execution.Body, execution.AcceptanceCriteria})
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		revisions := append(globals, submitted...)
		// Spec 07 does not define a Task source commit. Use this Execution's
		// base_sha so every layer records a non-empty, Execution-bound SHA.
		revisions = append(revisions, store.ContextRevision{Layer: "task", Kind: "task_input", SourceCommitSHA: execution.BaseSHA, Content: string(taskContent), ContentHash: contextHash(string(taskContent))})
		result, err := builder.build(revisions)
		if err != nil {
			WriteError(w, logger, contextBuildResponse(r.Context(), tx, execution, input.HeadSHA, err))
			return
		}
		now := time.Now().UTC()
		eventRevisions := make([]map[string]string, 0, len(revisions))
		for i := range revisions {
			id, idErr := ulid.New(ulid.Timestamp(now), rand.Reader)
			if idErr != nil {
				err = idErr
				break
			}
			revisions[i].ID, revisions[i].ExecutionID, revisions[i].CreatedAt = id.String(), execution.ID, now.Format(time.RFC3339Nano)
			result.RevisionIDs[i] = revisions[i].ID
			if err = store.InsertContextRevision(r.Context(), tx, revisions[i]); err != nil {
				break
			}
			eventRevisions = append(eventRevisions, map[string]string{"layer": revisions[i].Layer, "path": revisions[i].SourcePath, "hash": revisions[i].ContentHash})
		}
		if err == nil {
			payload, encodeErr := json.Marshal(map[string]any{"revisions": eventRevisions})
			err = encodeErr
			if err == nil {
				_, err = store.AppendExecutionEvent(r.Context(), tx, execution.ID, model.EventContextPinned, payload)
			}
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func fullCommitSHA(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && (len(value) == 40 || len(value) == 64) && value == strings.ToLower(value)
}

func contextHash(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

func relativeContextPath(name string) bool {
	return name != "" && name != "." && name != ".." && !path.IsAbs(name) && path.Clean(name) == name && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\x00\\")
}

func submittedRevisions(input contextInput, execution store.ContextExecution) ([]store.ContextRevision, error) {
	revisions := []store.ContextRevision{}
	var pinError *contextBuildError
	for _, layer := range []struct {
		name, sha string
		files     []gitcontext.File
	}{{"harness", execution.ProfileCommitSHA, input.Harness}, {"project", execution.BaseSHA, input.Project}} {
		seen := map[string]bool{}
		sort.Slice(layer.files, func(i, j int) bool { return layer.files[i].Path < layer.files[j].Path })
		for _, file := range layer.files {
			if !relativeContextPath(file.Path) || seen[file.Path] {
				return nil, invalid(layer.name+".path", "invalid_or_duplicate_path")
			}
			seen[file.Path] = true
			if layer.name == "project" && !projectcontext.IsInstruction(file.Path) {
				return nil, invalid("project.path", "unselected_path")
			}
			if file.ContentHash != contextHash(file.Content) {
				return nil, invalid(layer.name+".contentHash", "hash_mismatch")
			}
			if !fullCommitSHA(file.SourceCommitSHA) {
				return nil, invalid(layer.name+".sourceCommitSha", "invalid_format")
			}
			if file.SourceCommitSHA != layer.sha && pinError == nil {
				pinError = &contextBuildError{field: layer.name + ".sourceCommitSha", reason: "pin_mismatch", conflict: true}
			}
			kind := "harness_config"
			switch path.Base(file.Path) {
			case "AGENTS.md":
				kind = "agents_md"
			case "CLAUDE.md":
				kind = "claude_md"
			}
			revisions = append(revisions, store.ContextRevision{Layer: layer.name, Kind: kind, SourcePath: file.Path, SourceCommitSHA: file.SourceCommitSHA, ContentHash: file.ContentHash, Content: file.Content})
		}
	}
	// Complete request validation before allowing a version conflict to cause
	// a lifecycle change. Malformed snapshots must not block an Execution.
	if pinError != nil {
		return nil, pinError
	}
	return revisions, nil
}

func readGlobalContext(ctx context.Context, config ContextConfig) ([]store.ContextRevision, error) {
	revisions := []store.ContextRevision{}
	if len(config.GlobalPaths) == 0 {
		return revisions, nil
	}
	rootPath := config.RepositoryRoot
	if rootPath == "" {
		rootPath = "."
	}
	seen := map[string]bool{}
	for _, name := range config.GlobalPaths {
		if !relativeContextPath(name) || !strings.HasPrefix(name, "context/") {
			return nil, fmt.Errorf("invalid Global Context path")
		}
		seen[name] = true
	}
	// Resolve the moving ref once, then use only that immutable commit for all
	// selected blobs. Working-tree changes never enter the snapshot.
	sha, err := gitcontext.HeadCommitSHA(ctx, rootPath)
	if err != nil {
		return nil, err
	}
	files, err := gitcontext.Read(ctx, rootPath, sha, func(name string) bool { return seen[name] })
	if err != nil {
		return nil, err
	}
	if len(files) != len(seen) {
		return nil, fmt.Errorf("Global Context file is missing from commit %s", sha)
	}
	for _, file := range files {
		revisions = append(revisions, store.ContextRevision{Layer: "global", Kind: "global_policy", SourcePath: file.Path, SourceCommitSHA: file.SourceCommitSHA, Content: file.Content, ContentHash: file.ContentHash})
	}
	return revisions, nil
}

func sameSubmission(existing, submitted []store.ContextRevision) bool {
	files := []store.ContextRevision{}
	hasTask := false
	for _, revision := range existing {
		if revision.Layer == "task" {
			hasTask = true
		}
		if revision.Layer == "harness" || revision.Layer == "project" {
			files = append(files, revision)
		}
	}
	if !hasTask || len(files) != len(submitted) {
		return false
	}
	for i, file := range files {
		other := submitted[i]
		if file.Layer != other.Layer || file.SourcePath != other.SourcePath || file.SourceCommitSHA != other.SourceCommitSHA || file.ContentHash != other.ContentHash || file.Content != other.Content {
			return false
		}
	}
	return true
}
