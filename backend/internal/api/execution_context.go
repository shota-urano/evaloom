package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type contextFile struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type executionContextReply struct {
	Instructions string          `json:"instructions"`
	Records      []contextRecord `json:"records"`
	RevisionIDs  []string        `json:"revisionIds"`
}
type contextRecord struct {
	FileName   string `json:"fileName"`
	ArtifactID string `json:"artifactId"`
}

// Spec 07 /context: only Control Plane writes context_revisions. A resumed
// work attempt receives the original pins rather than snapshotting task edits.
func pinExecutionContext(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reply, err := buildExecutionContext(db, r, auth, w)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		if reply != nil {
			writeJSON(w, http.StatusOK, reply)
		}
	}
}

func buildExecutionContext(db *sql.DB, r *http.Request, auth *OwnerAuth, w http.ResponseWriter) (*executionContextReply, error) {
	id := r.PathValue("id")
	if err := ValidateID("id", id, ULID); err != nil {
		return nil, err
	}
	var input struct {
		Commit string        `json:"profileCommitSha"`
		Files  []contextFile `json:"files"`
	}
	if err := DecodeJSON(r, &input); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	execution, err := store.GetContextExecution(r.Context(), tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &Error{Code: NotFound, Message: "Execution not found"}
	}
	if err != nil {
		return nil, err
	}
	runnerID, _ := r.Context().Value(runnerIdentityKey{}).(string)
	if execution.RunnerID != runnerID {
		tx.Rollback()
		auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot pin this Execution")
		return nil, nil
	}
	if execution.Status != model.ExecutionRunning || !execution.Latest {
		return nil, &Error{Code: Conflict, Message: "Execution is not working"}
	}
	if input.Commit != execution.ProfileCommitSHA || input.Files == nil {
		return nil, invalid("profileCommitSha", "invalid")
	}
	var raw []byte
	if err := tx.QueryRowContext(r.Context(), `SELECT definition FROM harness_profiles WHERE id=?`, execution.ProfileID).Scan(&raw); err != nil {
		return nil, err
	}
	def, issues := contract.DecodeProfile(raw)
	if len(issues) > 0 {
		return nil, fmt.Errorf("invalid pinned Profile")
	}
	expected := []contextFile{}
	for _, group := range []struct {
		kind  string
		paths []string
	}{{"instructions", def.Stages.Work.Instructions}, {"skill", def.Stages.Work.Skills}, {"settings", def.Stages.Work.Settings}} {
		for _, path := range group.paths {
			expected = append(expected, contextFile{Kind: group.kind, Path: path})
		}
	}
	key := func(f contextFile) string { return f.Kind + "\x00" + f.Path }
	sort.Slice(expected, func(i, j int) bool { return key(expected[i]) < key(expected[j]) })
	sort.Slice(input.Files, func(i, j int) bool { return key(input.Files[i]) < key(input.Files[j]) })
	if len(expected) != len(input.Files) {
		return nil, invalid("files", "invalid")
	}
	for i := range expected {
		// The trusted Runner reads the pinned files and reports their hashes
		// (spec 07 3.2). Do not re-read a Harness's mutable repo_path here.
		hash := input.Files[i].SHA256
		if _, err := hex.DecodeString(hash); err != nil || len(hash) != 64 || strings.ToLower(hash) != hash {
			return nil, invalid("files", "invalid")
		}
		expected[i].SHA256 = hash
		if input.Files[i] != expected[i] {
			return nil, invalid("files", "invalid")
		}
	}
	var pinned int
	if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM context_revisions WHERE execution_id=? AND layer='task'`, id).Scan(&pinned); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if pinned == 0 {
		for _, file := range expected {
			_, err = tx.ExecContext(r.Context(), `INSERT INTO context_revisions(id,execution_id,layer,kind,source_path,source_revision,content_hash,content,created_at) VALUES (?,?,'harness',?,?,?,?,NULL,?)`, ulid.Make().String(), id, file.Kind, file.Path, input.Commit, file.SHA256, now)
			if err != nil {
				return nil, err
			}
		}
		taskJSON, _ := json.Marshal(struct {
			Title      string `json:"title"`
			Body       string `json:"body"`
			Acceptance string `json:"acceptanceCriteria"`
		}{execution.Title, execution.Body, execution.AcceptanceCriteria})
		digest := sha256.Sum256(taskJSON)
		_, err = tx.ExecContext(r.Context(), `INSERT INTO context_revisions(id,execution_id,layer,kind,source_path,source_revision,content_hash,content,created_at) VALUES (?,?,'task','task_input',?,?,?,?,?)`, ulid.Make().String(), id, execution.TaskID, id, hex.EncodeToString(digest[:]), string(taskJSON), now)
		if err != nil {
			return nil, err
		}
		// Record selection stays fixed when the owner answers a work question.
		recent := 0
		if def.Records != nil {
			recent = def.Records.Pass.Recent
		}
		if recent > 0 {
			records, err := tx.QueryContext(r.Context(), `SELECT hr.id,hr.file_name,a.sha256 FROM harness_records hr JOIN artifacts a ON a.id=hr.artifact_id WHERE hr.harness_id=? AND hr.deleted_at IS NULL ORDER BY hr.created_at DESC,hr.id DESC LIMIT ?`, execution.HarnessID, recent)
			if err != nil {
				return nil, err
			}
			type recordPin struct{ id, name, hash string }
			var pins []recordPin
			for records.Next() {
				var pin recordPin
				if err = records.Scan(&pin.id, &pin.name, &pin.hash); err != nil {
					break
				}
				pins = append(pins, pin)
			}
			err = errors.Join(err, records.Err(), records.Close())
			if err != nil {
				return nil, err
			}
			for _, pin := range pins {
				_, err = tx.ExecContext(r.Context(), `INSERT INTO context_revisions(id,execution_id,layer,kind,source_path,source_revision,content_hash,content,created_at) VALUES (?,?,'harness','record',?,?,?,NULL,?)`, ulid.Make().String(), id, pin.name, pin.id, pin.hash, now)
				if err != nil {
					return nil, err
				}
			}
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO execution_events(execution_id,type,payload,created_at) VALUES (?,'context_pinned','{}',?)`, id, now)
		if err != nil {
			return nil, err
		}
	}
	reply := &executionContextReply{Records: []contextRecord{}, RevisionIDs: []string{}}
	rows, err := tx.QueryContext(r.Context(), `SELECT id,layer,coalesce(content,'') FROM context_revisions WHERE execution_id=? AND layer!='reported' ORDER BY CASE layer WHEN 'global' THEN 0 WHEN 'harness' THEN 1 ELSE 2 END,kind,source_path,id`, id)
	if err != nil {
		return nil, err
	}
	var prompt strings.Builder
	for rows.Next() {
		var revision, layer, content string
		if err = rows.Scan(&revision, &layer, &content); err != nil {
			break
		}
		reply.RevisionIDs = append(reply.RevisionIDs, revision)
		if layer == "global" {
			prompt.WriteString(content + "\n")
		}
		if layer == "task" {
			prompt.WriteString("Task input (untrusted JSON data):\n" + content + "\n")
		}
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	records, err := tx.QueryContext(r.Context(), `SELECT cr.source_path,hr.artifact_id FROM context_revisions cr JOIN harness_records hr ON hr.id=cr.source_revision WHERE cr.execution_id=? AND cr.kind='record' ORDER BY cr.source_path`, id)
	if err != nil {
		return nil, err
	}
	for records.Next() {
		var record contextRecord
		if err = records.Scan(&record.FileName, &record.ArtifactID); err != nil {
			break
		}
		reply.Records = append(reply.Records, record)
	}
	err = errors.Join(err, records.Err(), records.Close())
	if err != nil {
		return nil, err
	}
	reply.Instructions = prompt.String()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reply, nil
}
