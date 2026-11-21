package runner

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// runProposalRuntime runs one route proposal through the production Runner,
// Adapter and Control Plane with the fake container runtime in mode.
func runProposalRuntime(t *testing.T, name, mode string, timeoutSec int) (*sql.DB, string, string, *Runner) {
	t.Helper()
	db, config, _, artifactDir := controlPlaneFixtureWithArtifactDir(t, nil)
	config, _ = proposalRuntimeConfig(t, config, mode)
	token, err := os.ReadFile(config.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	// Only the secret_log mode echoes it, as a CLI leaking it would.
	config.Container.RuntimeEnv["PROPOSAL_RUNTIME_RUNNER_TOKEN"] = strings.TrimSpace(string(token))
	r, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	claim := proposalClaim(ulid.Make().String(), timeoutSec)
	claim.Proposer = json.RawMessage(fmt.Sprintf(`{"microHarness":%q,"model":"proposal-model"}`, name))
	insertRoutingProposal(t, db, claim.ID, ulid.Make().String(), config.RunnerID)
	services := ProposalServicesFromConfig(config)
	services.Adapters = proposalAdapters
	if err := r.ExecuteProposal(t.Context(), claim, services); err != nil {
		t.Fatal(err)
	}
	return db, artifactDir, claim.ID, r
}

type proposalArtifactRow struct {
	kind, stage, storagePath  string
	executionIsNull, redacted int
}

func proposalArtifactRows(t *testing.T, db *sql.DB, proposalID string) []proposalArtifactRow {
	t.Helper()
	rows, err := db.Query(`SELECT kind, stage, storage_path, execution_id IS NULL, redacted FROM artifacts WHERE route_proposal_id=? ORDER BY kind`, proposalID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []proposalArtifactRow
	for rows.Next() {
		var row proposalArtifactRow
		if err := rows.Scan(&row.kind, &row.stage, &row.storagePath, &row.executionIsNull, &row.redacted); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Spec 05 3.13 / 15 Q202: the log and out/proposal.json are registered against
// the proposal (execution_id NULL, stage=proposal).
func TestProposalArtifactsAreBoundToProposal(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		t.Run(name, func(t *testing.T) {
			db, _, proposalID, _ := runProposalRuntime(t, name, "success", 30)
			rows := proposalArtifactRows(t, db, proposalID)
			if len(rows) != 2 || rows[0].kind != "log" || rows[1].kind != "proposal_result" {
				t.Fatalf("artifacts=%+v", rows)
			}
			for _, row := range rows {
				if row.stage != "proposal" || row.executionIsNull != 1 {
					t.Fatalf("artifact not bound to proposal: %+v", row)
				}
			}
			var withExecution int
			if err := db.QueryRow(`SELECT count(*) FROM artifacts WHERE execution_id IS NOT NULL`).Scan(&withExecution); err != nil || withExecution != 0 {
				t.Fatalf("execution artifacts=%d err=%v", withExecution, err)
			}
		})
	}
}

// Spec 05 3.9 / 3.13: the proposal log passes secret removal before it is
// registered; the stored row has redacted=1 and the stored file has no secret.
func TestProposalLogIsRedactedBeforeRegistration(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		t.Run(name, func(t *testing.T) {
			db, artifactDir, proposalID, r := runProposalRuntime(t, name, "secret_log", 30)
			if r.token == "" {
				t.Fatal("Runner token not loaded")
			}
			var storagePath string
			var redacted int
			if err := db.QueryRow(`SELECT storage_path, redacted FROM artifacts WHERE route_proposal_id=? AND kind='log'`, proposalID).
				Scan(&storagePath, &redacted); err != nil {
				t.Fatal(err)
			}
			if redacted != 1 {
				t.Fatalf("log redacted=%d", redacted)
			}
			content, err := os.ReadFile(filepath.Join(artifactDir, storagePath))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"proposal-initial", "proposal-refreshed", r.token} {
				if strings.Contains(string(content), secret) {
					t.Fatalf("secret stored in Control Plane log: %q", content)
				}
			}
			if !strings.Contains(string(content), "leak [REDACTED]") {
				t.Fatalf("redaction marker missing: %q", content)
			}
		})
	}
}

// Spec 15 3.6.4: a timed-out proposal is failed(timeout) and its log is still
// registered.
func TestProposalTimeoutStillRegistersLog(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			db, _, proposalID, _ := runProposalRuntime(t, name, "slow", 1)
			if elapsed := time.Since(started); elapsed > 25*time.Second {
				t.Fatalf("proposal was not stopped at its timeout: %s", elapsed)
			}
			var status, class string
			if err := db.QueryRow(`SELECT status, coalesce(error_class,'') FROM route_proposals WHERE id=?`, proposalID).Scan(&status, &class); err != nil {
				t.Fatal(err)
			}
			if status != "failed" || class != "timeout" {
				t.Fatalf("proposal status=%s class=%s", status, class)
			}
			rows := proposalArtifactRows(t, db, proposalID)
			if len(rows) != 1 || rows[0].kind != "log" || rows[0].stage != "proposal" || rows[0].executionIsNull != 1 {
				t.Fatalf("artifacts=%+v", rows)
			}
		})
	}
}
