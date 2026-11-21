package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

const proposalArtifactQuery = "?artifactId=" + artifactID + "&stage=proposal&kind=log&name=proposal.log&redacted=1"

// proposalArtifactFixture is artifactFixture plus a route proposal handed to
// the fixture's Runner (spec 05 3.13, 15 3.6.4).
func proposalArtifactFixture(t *testing.T) (*authFixture, string, string, string) {
	t.Helper()
	f, token, dir := artifactFixture(t)
	proposalID := ulid.Make().String()
	insertRouteProposal(t, f, proposalID, ulid.Make().String(), "2026-11-01T00:00:00Z")
	stageExec(t, f, `UPDATE route_proposals SET status='running', runner_id='mac-mini' WHERE id=?`, proposalID)
	return f, token, dir, proposalID
}

func proposalArtifactPath(id string) string { return "/api/route-proposals/" + id + "/artifacts" }

func TestProposalArtifactRegistersRowForProposal(t *testing.T) {
	for _, kind := range []string{"log", "proposal_result"} {
		t.Run(kind, func(t *testing.T) {
			f, token, dir, proposalID := proposalArtifactFixture(t)
			query := strings.ReplaceAll(proposalArtifactQuery, "kind=log", "kind="+kind)
			status, body := postArtifact(t, f, token, proposalArtifactPath(proposalID)+query, "[REDACTED]\n")
			if status != http.StatusNoContent {
				t.Fatalf("upload status=%d body=%s", status, body)
			}
			assertArtifactCount(t, f, dir, 1)
			var routeProposalID, stage, storedKind string
			var executionIsNull, redacted int
			if err := f.db.QueryRow(`SELECT route_proposal_id, execution_id IS NULL, stage, kind, redacted FROM artifacts WHERE id=?`, artifactID).
				Scan(&routeProposalID, &executionIsNull, &stage, &storedKind, &redacted); err != nil {
				t.Fatal(err)
			}
			if routeProposalID != proposalID || executionIsNull != 1 || stage != "proposal" || storedKind != kind || redacted != 1 {
				t.Fatalf("row route_proposal_id=%s execution_id_null=%d stage=%s kind=%s redacted=%d", routeProposalID, executionIsNull, stage, storedKind, redacted)
			}
		})
	}
}

func TestProposalArtifactRejectsNonProposalStageOrKindWithoutWrites(t *testing.T) {
	f, token, dir, proposalID := proposalArtifactFixture(t)
	for _, query := range []string{
		strings.ReplaceAll(proposalArtifactQuery, "stage=proposal", "stage=work"),
		strings.ReplaceAll(proposalArtifactQuery, "kind=log", "kind=out_file"),
	} {
		status, body := postArtifact(t, f, token, proposalArtifactPath(proposalID)+query, "content")
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	assertArtifactCount(t, f, dir, 0)
}

func TestProposalArtifactRejectsNonRunnerAuthWithoutWrites(t *testing.T) {
	f, _, dir, proposalID := proposalArtifactFixture(t)
	path := proposalArtifactPath(proposalID)
	t.Run("owner login", func(t *testing.T) {
		status, body := f.request(t, "POST", path+proposalArtifactQuery, true, f.csrf, func(r *http.Request) {
			r.Header.Set("Content-Type", "application/octet-stream")
		})
		assertAuthError(t, status, body, 403, Forbidden)
		f.assertAudit(t, "POST", path, "wrong_auth_class", 403, Forbidden)
		assertArtifactCount(t, f, dir, 0)
	})
	t.Run("no credentials", func(t *testing.T) {
		status, body := f.request(t, "POST", path+proposalArtifactQuery, false, "", func(r *http.Request) {
			r.Header.Del("Origin")
			r.Header.Set("Content-Type", "application/octet-stream")
		})
		assertAuthError(t, status, body, 401, Unauthenticated)
		assertArtifactCount(t, f, dir, 0)
	})
}

func TestProposalArtifactUnknownProposalCreatesNoRow(t *testing.T) {
	f, token, dir, _ := proposalArtifactFixture(t)
	status, body := postArtifact(t, f, token, proposalArtifactPath(ulid.Make().String())+proposalArtifactQuery, "content")
	assertAuthError(t, status, body, 404, NotFound)
	assertArtifactCount(t, f, dir, 0)
}
