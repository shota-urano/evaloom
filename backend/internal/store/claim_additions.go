package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// All database-derived additions share NextClaim's acquisition snapshot.
func loadClaimAdditions(ctx context.Context, tx *sql.Tx, c *Claim) error {
	if c.Execution.Mode == "fresh" {
		var number int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM executions
			WHERE task_id=? AND mode='fresh' AND seq<=?`, c.Execution.TaskID, c.Execution.Seq).Scan(&number); err != nil {
			return err
		}
		c.Execution.FreshNumber = &number
	}
	c.PullRequests = []ClaimPullRequest{}
	rows, err := tx.QueryContext(ctx, `SELECT e.pr_number,e.branch_name,e.seq FROM executions e
		WHERE e.task_id=? AND e.seq<? AND e.pr_number IS NOT NULL
		AND NOT EXISTS(SELECT 1 FROM executions newer WHERE newer.task_id=e.task_id
			AND newer.seq<? AND newer.seq>e.seq AND newer.pr_number=e.pr_number)
		ORDER BY e.seq DESC`, c.Execution.TaskID, c.Execution.Seq, c.Execution.Seq)
	if err != nil {
		return err
	}
	for rows.Next() {
		var pr ClaimPullRequest
		if err := rows.Scan(&pr.Number, &pr.BranchName, &pr.Seq); err != nil {
			rows.Close()
			return err
		}
		c.PullRequests = append(c.PullRequests, pr)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	c.Execution.ResumeGateResults = []ClaimGateResult{}
	if c.Execution.ResumePoint == nil {
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT harness_head_sha,verified_sha FROM executions WHERE id=?`, c.Execution.ID).
		Scan(&c.Execution.HarnessHeadSHA, &c.Execution.VerifiedSHA); err != nil {
		return err
	}
	block := new(ClaimResumeBlock)
	var detail string
	err = tx.QueryRowContext(ctx, `SELECT id,cause,head_sha,gate_result_id,detail,decided_at
		FROM execution_blocks WHERE execution_id=? AND decision='continue'
		ORDER BY decided_at DESC,rowid DESC LIMIT 1`, c.Execution.ID).
		Scan(&block.ID, &block.Cause, &block.HeadSHA, &block.GateResultID, &detail, &block.DecidedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if !json.Valid([]byte(detail)) {
			return errors.New("store: invalid resume block detail")
		}
		block.Detail = json.RawMessage(detail)
		c.Execution.ResumeBlock = block
		c.Execution.ResumeHeadSHA = &block.HeadSHA
	}
	c.Execution.VerifyRun, err = findVerifyRun(ctx, tx, c.Execution.ID)
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,gate,result,base_sha,target_sha,findings
		FROM gate_results WHERE execution_id=? ORDER BY created_at,rowid`, c.Execution.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var gate ClaimGateResult
		var findings string
		if err := rows.Scan(&gate.ID, &gate.Gate, &gate.Result, &gate.BaseSHA, &gate.TargetSHA, &findings); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(findings), &gate.Findings); err != nil {
			rows.Close()
			return err
		}
		c.Execution.ResumeGateResults = append(c.Execution.ResumeGateResults, gate)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	// Q74 specifies one HarnessResult per Execution. If several exist, the
	// spec does not select a winner; treat the summary as unavailable.
	return tx.QueryRowContext(ctx, `SELECT CASE WHEN count(*)=1 THEN min(id) END
		FROM artifacts WHERE execution_id=? AND kind='harness_output'`, c.Execution.ID).Scan(&c.HarnessOutputArtifactID)
}
