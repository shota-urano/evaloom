package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

func claimOnce(t *testing.T, db *sql.DB) *ExecutionJob {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	response, err := Claim(ctx, tx, ClaimInput{RunnerID: "mac-mini", WorkspaceRoot: "/workspaces"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	job, ok := response.Job.(*ExecutionJob)
	if !ok {
		var s, m sql.NullString
		db.QueryRow(`SELECT status, error_message FROM executions`).Scan(&s, &m)
		t.Fatalf("job=%#v execution status=%v error=%v", response.Job, s, m)
	}
	return job
}

// spec 04 3.3 step 4: the claim pins Profile, contract version and params
// without losing number precision, and later Project changes do not alter them.
func TestClaimPinsContractAndParamsWithoutLosingNumbers(t *testing.T) {
	db := openTaskTables(t)
	const params = `{"number":9007199254740993}`
	mustTaskExec(t, db, `UPDATE projects SET params=?`, params)
	var definition string
	if err := db.QueryRow(`SELECT definition FROM harness_profiles WHERE id='profile-1'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	job := claimOnce(t, db)
	if string(job.Params) != params || string(job.Profile.Definition) != definition || job.Profile.ID != "profile-1" {
		t.Fatalf("job params=%s profile=%+v", job.Params, job.Profile)
	}
	encoded, err := json.Marshal(job)
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("encode=%s err=%v", encoded, err)
	}
	mustTaskExec(t, db, `UPDATE projects SET params='{"changed":true}'`)
	var route, project, contractVersion, pinnedParams, workspace, path string
	if err := db.QueryRow(`SELECT e.route_kind, e.project_id, e.contract_version, e.params, e.workspace_id, w.path
		FROM executions e JOIN task_workspaces w ON w.id = e.workspace_id`).
		Scan(&route, &project, &contractVersion, &pinnedParams, &workspace, &path); err != nil {
		t.Fatal(err)
	}
	if route != "project" || project != "project-a" || contractVersion != "1" || pinnedParams != params ||
		workspace != job.Workspace.ID || path != "/workspaces/"+workspace || !job.Workspace.Created {
		t.Fatal(route, project, contractVersion, pinnedParams, workspace, path)
	}
}
