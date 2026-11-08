package linearoutbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const (
	worktreePath = "/worktrees/linear-1-r1"
	sha40        = "0123456789abcdef0123456789abcdef01234567"
)

// claimedLinearExecution gives linear-1-e1 the state a claimed Execution has:
// harness h at Profile 1.0.0, Runner "runner" (worktree root /worktrees) and
// its worktree. The harness_output artifact holds summary (none when empty),
// read through the Control Plane's ArtifactStore as evaloomd does.
func claimedLinearExecution(t *testing.T, db *sql.DB, summary string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO harnesses (id, name, specialty, supported_tasks, adapter, repo_path, repo_url,
		supported_models, available_tools, tool_permissions, contract_version, created_at, updated_at)
		VALUES ('h', 'Harness', 'developer', '["development"]', 'codex', '/repos/h', '', '[]', '[]', '{}', '1', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, definition,
		definition_hash, model_id, operation_status, registered_at)
		VALUES ('p', 'h', '1.0.0', ?, 'v1', '{}', 'hash', 'model', 'provisional', ?)`, sha40, testTime)
	mustExec(t, db, `INSERT INTO worktrees (id, project_id, task_id, runner_id, path, branch_name, base_sha, status,
		setup_status, copied_files, created_at) VALUES ('wt-1', 'project', 'linear-1', 'runner', ?, 'b', ?, 'active',
		'succeeded', '[]', ?)`, worktreePath, sha40, testTime)
	mustExec(t, db, `UPDATE executions SET runner_id = 'runner', harness_id = 'h', profile_id = 'p',
		profile_version = '1.0.0', profile_commit_sha = ?, worktree_id = 'wt-1' WHERE id = 'linear-1-e1'`, sha40)
	files, err := store.NewArtifactStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	t.Cleanup(linearoutbox.SetSummarySource(files.HarnessSummaryTx))
	if summary != "" {
		data, _ := json.Marshal(map[string]string{"summary": summary})
		if _, err := files.Save(context.Background(), "out-1", "linear-1-e1", "harness_output", true, strings.NewReader(string(data))); err != nil {
			t.Fatal(err)
		}
	}
}

func gateResults(t *testing.T, db *sql.DB, gate2, gate3 string, total, skipped int, findings string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO test_runs (id, execution_id, kind, target_sha, command, exit_code, total, passed,
		failed, skipped, timed_out, started_at, finished_at) VALUES ('run-1', 'linear-1-e1', 'verify', ?, 'make test',
		0, ?, ?, 0, ?, 0, ?, ?)`, sha40, total, total-skipped, skipped, testTime, testTime)
	mustExec(t, db, `INSERT INTO gate_results (id, execution_id, gate, result, base_sha, target_sha, findings, created_at)
		VALUES ('g2', 'linear-1-e1', 2, ?, ?, ?, ?, ?)`, gate2, sha40, sha40, findings, testTime)
	if gate3 != "" {
		mustExec(t, db, `INSERT INTO gate_results (id, execution_id, gate, result, base_sha, target_sha, findings, created_at)
			VALUES ('g3', 'linear-1-e1', 3, ?, ?, ?, '[]', ?)`, gate3, sha40, sha40, testTime)
	}
}

func onlyBody(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows := outboxRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %+v; want one", rows)
	}
	if !strings.HasSuffix(rows[0].body, rows[0].dedupeKey) {
		t.Fatalf("body does not end with dedupe_key: %q", rows[0].body)
	}
	return rows[0].body
}

func checkBody(t *testing.T, body string, want, absent []string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("body lacks %q:\n%s", s, body)
		}
	}
	for _, s := range absent {
		if strings.Contains(body, s) {
			t.Errorf("body contains %q:\n%s", s, body)
		}
	}
}

// 受け入れ基準: 本文に 6.4 の7項目のうちその場面に該当するものが入る。
// Each spec 16 3.2 trigger that the state transitions produce gets the 3.3
// items that apply to it; the others are left out.
func TestCommentBodyHasTheItemsOfItsTrigger(t *testing.T) {
	ctx := context.Background()
	t.Run("succeeded with a PR", func(t *testing.T) {
		db := outboxDB(t)
		claimedLinearExecution(t, db, "テストを1件足した")
		gateResults(t, db, "pass", "pass", 5, 1, `[]`)
		mustExec(t, db, `UPDATE executions SET pr_url = 'https://github.com/o/r/pull/3', pushed_sha = ? WHERE id = 'linear-1-e1'`, sha40)
		setStates(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing)
		transition(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing, model.ExecutionSucceeded)
		checkBody(t, onlyBody(t, db), []string{
			"管理画面のタスク ID: linear-1\n",
			"- 実行ステータス: Completed\n",
			"- 実行担当ハーネス: h（Profile 1.0.0）\n",
			"\n作業概要:\nテストを1件足した\n",
			"\n検証結果:\n- Gate 2: pass（実行数: 4、skip 数: 1、確認方法: ID による照合）\n- Gate 3: pass\n",
			"- PR URL: https://github.com/o/r/pull/3\n",
		}, []string{"エラー情報", "承認", "push した commit"})
	})
	t.Run("succeeded by a direct push", func(t *testing.T) {
		db := outboxDB(t)
		claimedLinearExecution(t, db, "直接 push した")
		gateResults(t, db, "pass", "pass", 2, 0, `[{"rule":"count_only_check","path":"","line":0,"message":"件数"}]`)
		mustExec(t, db, `UPDATE executions SET pushed_sha = ? WHERE id = 'linear-1-e1'`, sha40)
		setStates(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing)
		transition(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing, model.ExecutionSucceeded)
		checkBody(t, onlyBody(t, db), []string{
			"- PR URL: なし（ベースブランチへ push した commit: " + sha40 + "）\n",
			"確認方法: 件数による確認",
		}, []string{"エラー情報"})
	})
	t.Run("failed", func(t *testing.T) {
		db := outboxDB(t)
		claimedLinearExecution(t, db, "途中まで")
		gateResults(t, db, "failed", "", 3, 0, `[{"rule":"test_failed","path":"a_test.go","line":0,"message":"FAIL TestA"}]`)
		mustExec(t, db, `UPDATE executions SET error_class = 'test_failed', error_message = 'テストが失敗した' WHERE id = 'linear-1-e1'`)
		setStates(t, db, "linear-1", model.TaskVerifying, model.ExecutionVerifying)
		transition(t, db, "linear-1", model.TaskVerifying, model.ExecutionVerifying, model.ExecutionFailed)
		checkBody(t, onlyBody(t, db), []string{
			"- 実行ステータス: Failed\n",
			"- 実行担当ハーネス: h（Profile 1.0.0）\n",
			"- エラー情報: test_failed（テストが失敗した）\n",
			"- Gate 2: failed（実行数: 3、skip 数: 0、確認方法: ID による照合）\n",
			"\n作業概要:\n途中まで\n",
		}, []string{"PR URL", "Gate 3", "FAIL TestA", "a_test.go"})
	})
	t.Run("interrupted by a restart, through store recovery", func(t *testing.T) {
		db := outboxDB(t)
		claimedLinearExecution(t, db, "")
		setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning)
		started := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
		if err := store.RecoverStaleRunners(ctx, db, started, started.Add(store.StartupHeartbeatGrace)); err != nil {
			t.Fatal(err)
		}
		checkBody(t, onlyBody(t, db), []string{
			"- 実行ステータス: Failed\n",
			"- エラー情報: interrupted（runner_heartbeat_lost）\n",
		}, []string{"作業概要", "検証結果", "PR URL"})
	})
	t.Run("blocked", func(t *testing.T) {
		db := outboxDB(t)
		claimedLinearExecution(t, db, "テストを消した")
		gateResults(t, db, "blocked", "", 1, 0, `[{"rule":"existing_test_not_run","path":"","line":0,"message":"x"}]`)
		mustExec(t, db, `INSERT INTO execution_blocks (id, execution_id, cause, gate_result_id, head_sha, detail, created_at)
			VALUES ('b1', 'linear-1-e1', 'gate2_missing_tests', 'g2', ?, '{}', ?)`, sha40, testTime)
		setStates(t, db, "linear-1", model.TaskVerifying, model.ExecutionVerifying)
		transition(t, db, "linear-1", model.TaskVerifying, model.ExecutionVerifying, model.ExecutionBlocked)
		checkBody(t, onlyBody(t, db), []string{
			"- 実行ステータス: Blocked\n",
			"- エラー情報: Block 原因 gate2_missing_tests\n",
			"- 続行・中止は管理画面で本人が選ぶ\n",
			"- Gate 2: blocked（実行数: 1、skip 数: 0",
			"\n作業概要:\nテストを消した\n",
		}, []string{"PR URL"})
	})
	t.Run("cancelled", func(t *testing.T) {
		db := outboxDB(t)
		claimedLinearExecution(t, db, "途中")
		mustExec(t, db, `UPDATE executions SET error_class = 'cancelled' WHERE id = 'linear-1-e1'`)
		setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning)
		transition(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning, model.ExecutionCancelled)
		checkBody(t, onlyBody(t, db), []string{
			"- 実行ステータス: Cancelled\n",
			"- 実行担当ハーネス: h（Profile 1.0.0）\n",
			"- エラー情報: cancelled\n",
		}, []string{"作業概要", "検証結果", "PR URL"})
	})
}

// 受け入れ基準: Runner が保持する秘密の値を summary に含めたテストで本文が
// [REDACTED] になる (the Runner-side half is TestProductionPipelineLinearCommentBody
// in package runner). Here the Control Plane applies the same well-known
// patterns of spec 05 3.5 to the stored summary and the error message.
func TestCommentBodyRedactsSecretFormsInFreeText(t *testing.T) {
	db := outboxDB(t)
	claimedLinearExecution(t, db, "curl -H 'Authorization: Bearer hdr-secret-1'\n-----BEGIN PRIVATE KEY-----\npem-secret-2\n-----END PRIVATE KEY-----\n終わり")
	mustExec(t, db, `UPDATE executions SET error_class = 'execution_failed',
		error_message = 'Proxy-Authorization: Basic hdr-secret-3' WHERE id = 'linear-1-e1'`)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning)
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning, model.ExecutionFailed)
	checkBody(t, onlyBody(t, db), []string{
		"curl -H 'Authorization: [REDACTED]\n[REDACTED]\n\n\n終わり",
		"- エラー情報: execution_failed（Proxy-Authorization: [REDACTED]）\n",
	}, []string{"hdr-secret-1", "pem-secret-2", "hdr-secret-3"})
}

// 受け入れ基準: ログの本文と Runner のパスが本文に入らない。
func TestCommentBodyHasNoLogTextOrRunnerPath(t *testing.T) {
	db := outboxDB(t)
	claimedLinearExecution(t, db, "変更は "+worktreePath+"/main.go と /worktrees/other にある")
	mustExec(t, db, `UPDATE executions SET error_class = 'execution_failed',
		error_message = 'cannot open /worktrees/linear-1-r1/.git' WHERE id = 'linear-1-e1'`)
	mustExec(t, db, `INSERT INTO execution_events (execution_id, type, payload, created_at)
		VALUES ('linear-1-e1', 'log', '{"sourceId":"s1","stream":"harness","text":"LOG-BODY-CANARY"}', ?)`, testTime)
	mustExec(t, db, `INSERT INTO artifacts (id, execution_id, kind, storage_path, sha256, size_bytes, redacted, created_at)
		VALUES ('log-1', 'linear-1-e1', 'harness_log', 'artifacts/LOG-PATH-CANARY.log', 'x', 1, 0, ?)`, testTime)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning)
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning, model.ExecutionFailed)
	checkBody(t, onlyBody(t, db), []string{
		"変更は [REDACTED]/main.go と [REDACTED]/other にある",
		"- エラー情報: execution_failed（cannot open [REDACTED]/.git）\n",
	}, []string{"LOG-BODY-CANARY", "LOG-PATH-CANARY", "/worktrees", "artifacts/"})
}

// Without the artifact reader (evaloomd without -data-dir) a succeeded body
// says the summary is unavailable instead of failing the transition.
func TestCommentBodyWithoutSummarySource(t *testing.T) {
	db := outboxDB(t)
	setStates(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing)
	transition(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing, model.ExecutionSucceeded)
	checkBody(t, onlyBody(t, db), []string{"- 実行ステータス: Completed\n", "\n作業概要: 取得できなかった\n"}, nil)
}
