package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestProposalLogRetrySurvivesExecutionDeadline(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		for _, mode := range []string{"success", "missing_cli", "cli_failure"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				db, config, controlPlane := controlPlaneFixture(t, nil)
				var recovered atomic.Bool
				var failures atomic.Int32
				attempted := make(chan struct{}, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if strings.HasSuffix(req.URL.Path, "/artifacts") && req.URL.Query().Get("kind") == "log" && !recovered.Load() {
						failures.Add(1)
						select {
						case attempted <- struct{}{}:
						default:
						}
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					req.Host = controlPlane.Listener.Addr().String()
					controlPlane.Config.Handler.ServeHTTP(w, req)
				}))
				defer server.Close()
				config.ControlPlaneURL = server.URL
				config, _ = proposalRuntimeConfig(t, config, mode)
				r, err := New(config)
				if err != nil {
					t.Fatal(err)
				}
				claim := proposalClaim(ulid.Make().String(), 2)
				claim.Proposer = json.RawMessage(fmt.Sprintf(`{"microHarness":%q,"model":"m"}`, name))
				insertRoutingProposal(t, db, claim.ID, ulid.Make().String(), config.RunnerID)
				services := ProposalServicesFromConfig(config)
				services.Adapters = proposalAdapters
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- r.ExecuteProposal(ctx, claim, services) }()
				select {
				case <-attempted:
				case err := <-done:
					t.Fatalf("proposal stopped before log delivery: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("proposal did not attempt log delivery")
				}
				// The CLI has finished. Keep the API unavailable longer than the
				// entire execution timeout, then recover without cancelling the job.
				select {
				case err := <-done:
					t.Fatalf("execution deadline stopped log retry: %v", err)
				case <-time.After(time.Duration(claim.TimeoutSec)*time.Second + 100*time.Millisecond):
				}
				if failures.Load() < 2 {
					t.Fatal("log delivery did not retry the unavailable API")
				}
				recovered.Store(true)
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("proposal did not finish after log API recovery")
				}
				var status, label, reason, class string
				if err := db.QueryRow(`SELECT status,coalesce(proposed_label,''),coalesce(reason,''),coalesce(error_class,'') FROM route_proposals WHERE id=?`, claim.ID).Scan(&status, &label, &reason, &class); err != nil {
					t.Fatal(err)
				}
				wantStatus, wantClass := "proposed", ""
				switch mode {
				case "missing_cli":
					wantStatus, wantClass = "failed", "config_error"
				case "cli_failure":
					wantStatus, wantClass = "failed", "work_failed"
				}
				if status != wantStatus || class != wantClass {
					t.Fatalf("result=%s/%s; want %s/%s", status, class, wantStatus, wantClass)
				}
				var logs, proposals int
				if err := db.QueryRow(`SELECT count(*) FROM artifacts WHERE route_proposal_id=? AND kind='log'`, claim.ID).Scan(&logs); err != nil || logs != 1 {
					t.Fatalf("saved logs=%d err=%v", logs, err)
				}
				if err := db.QueryRow(`SELECT count(*) FROM artifacts WHERE route_proposal_id=? AND kind='proposal_result'`, claim.ID).Scan(&proposals); err != nil {
					t.Fatal(err)
				}
				if mode != "success" {
					if proposals != 0 || label != "" || reason != "" {
						t.Fatal("failed CLI produced a proposal result")
					}
					return
				}
				if proposals != 1 || label != "route:project" || !strings.HasPrefix(reason, "fits ") || strings.Contains(reason, "proposal-refreshed") {
					t.Fatalf("out proposal lost: artifacts=%d label=%q reason=%q", proposals, label, reason)
				}
				var artifactID string
				if err := db.QueryRow(`SELECT id FROM artifacts WHERE route_proposal_id=? AND kind='proposal_result'`, claim.ID).Scan(&artifactID); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(filepath.Join(config.DataDir, "artifacts", artifactID))
				if err != nil {
					t.Fatal(err)
				}
				outLabel, outReason, err := readProposalOutcomeBytes(body)
				if err != nil || outLabel != label || outReason != reason {
					t.Fatalf("saved out differs from reported proposal: %q %q %v", outLabel, outReason, err)
				}
			})
		}
	}
}
