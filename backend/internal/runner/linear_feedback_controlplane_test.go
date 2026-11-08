package runner

import (
	"strings"
	"testing"
)

// spec 16 3.3 through the production pipeline: a harness writes the Runner
// token, the GitHub token and its worktree path into the summary, and logs
// its worktree path. The Linear comment written when the Execution succeeds
// carries the 6.4 items with the secrets as [REDACTED], and contains neither
// the log text nor a Runner path.
func TestProductionPipelineLinearCommentBody(t *testing.T) {
	f := newProductionPipelineFixture(t, "linear_feedback")
	if err := f.execute(t); err != nil {
		t.Fatal(err)
	}
	f.assertSucceeded(t)
	var kind, body, worktreePath, worktreeRoot string
	if err := f.db.QueryRow(`SELECT kind, body FROM linear_outbox WHERE task_id='task-001'`).Scan(&kind, &body); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT w.path, r.worktree_root FROM executions e JOIN worktrees w ON w.id=e.worktree_id
		JOIN runners r ON r.id=e.runner_id WHERE e.id=?`, logExecution).Scan(&worktreePath, &worktreeRoot); err != nil {
		t.Fatal(err)
	}
	if kind != "succeeded" {
		t.Fatalf("kind %s", kind)
	}
	var pushed string
	if err := f.db.QueryRow(`SELECT pushed_sha FROM executions WHERE id=?`, logExecution).Scan(&pushed); err != nil {
		t.Fatal(err)
	}
	// The scenario's Project is direct_push (spec 17 3.7): no PR is created.
	for _, want := range []string{
		"- 実行ステータス: Completed\n",
		"- 実行担当ハーネス: h（Profile 1.0.0）\n",
		"- PR URL: なし（ベースブランチへ push した commit: " + pushed + "）\n",
		"- Gate 2: pass（実行数: 1、skip 数: 0、確認方法: ID による照合）\n",
		"- Gate 3: pass\n",
		"作業概要:\n作業した [REDACTED] [REDACTED] [REDACTED]",
		"管理画面のタスク ID: task-001\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	for _, leaked := range []string{f.runnerToken, readCanary, worktreePath, worktreeRoot, f.config.DataDir, "dummy ran in", "エラー情報"} {
		if leaked == "" || strings.Contains(body, leaked) {
			t.Errorf("body contains %q:\n%s", leaked, body)
		}
	}
}
