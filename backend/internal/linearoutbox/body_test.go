package linearoutbox_test

import (
	"context"
	"database/sql"
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
// harness h at Profile 1.0.0 and Runner "runner" (worktree root /worktrees).
// The work_summary artifact holds summary (none when empty),
// read through the Control Plane's ArtifactStore as evaloomd does.
func claimedLinearExecution(t *testing.T, db *sql.DB, summary string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO harnesses (id, name, repo_path, repo_url, created_at, updated_at) VALUES ('h', 'Harness', '/repos/h', '', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, definition, definition_hash, model_id, operation_status, registered_at, contract_version) VALUES ('p', 'h', '1.0.0', ?, 'v1', '{}', 'hash', 'model', 'provisional', ?, '1')`, sha40, testTime)
	mustExec(t, db, `UPDATE executions SET runner_id = 'runner', harness_id = 'h', profile_id = 'p',
		profile_version = '1.0.0', profile_commit_sha = ? WHERE id = 'linear-1-e1'`, sha40)
	files, err := store.NewArtifactStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	t.Cleanup(linearoutbox.SetSummarySource(files.HarnessSummaryTx))
	if summary != "" {
		if _, err := files.Save(context.Background(), store.Artifact{ID: "out-1", ExecutionID: "linear-1-e1", Stage: "work", Kind: "work_summary", Name: "summary.md", Redacted: true}, strings.NewReader(summary)); err != nil {
			t.Fatal(err)
		}
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
	t.Run("succeeded", func(t *testing.T) {
		db := outboxDB(t)
		claimedLinearExecution(t, db, "テストを1件足した")
		setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing)
		transition(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing, model.ExecutionSucceeded)
		checkBody(t, onlyBody(t, db), []string{
			"管理画面のタスク ID: linear-1\n",
			"- 実行ステータス: Completed\n",
			"- 実行担当ハーネス: h（Profile 1.0.0）\n",
			"\n作業概要:\nテストを1件足した\n",
		}, []string{"エラー情報", "承認", "PR URL", "検証結果", "Gate"})
	})
	t.Run("failed", func(t *testing.T) {
		db := outboxDB(t)
		claimedLinearExecution(t, db, "途中まで")
		mustExec(t, db, `UPDATE executions SET error_class = 'check_failed', error_message = 'テストが失敗した' WHERE id = 'linear-1-e1'`)
		setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionVerifying)
		transition(t, db, "linear-1", model.TaskRunning, model.ExecutionVerifying, model.ExecutionFailed)
		checkBody(t, onlyBody(t, db), []string{
			"- 実行ステータス: Failed\n",
			"- 実行担当ハーネス: h（Profile 1.0.0）\n",
			"- エラー情報: check_failed（テストが失敗した）\n",
			"\n作業概要:\n途中まで\n",
		}, []string{"PR URL", "検証結果", "Gate"})
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
		setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionVerifying)
		transition(t, db, "linear-1", model.TaskRunning, model.ExecutionVerifying, model.ExecutionBlocked)
		checkBody(t, onlyBody(t, db), []string{
			"- 実行ステータス: Blocked\n",
			"- 続行・中止は管理画面で本人が選ぶ\n",
			"\n作業概要:\nテストを消した\n",
		}, []string{"PR URL", "検証結果", "Block 原因"})
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
	mustExec(t, db, `UPDATE executions SET error_class = 'work_failed',
		error_message = 'Proxy-Authorization: Basic hdr-secret-3' WHERE id = 'linear-1-e1'`)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning)
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning, model.ExecutionFailed)
	checkBody(t, onlyBody(t, db), []string{
		"curl -H 'Authorization: [REDACTED]\n[REDACTED]\n\n\n終わり",
		"- エラー情報: work_failed（Proxy-Authorization: [REDACTED]）\n",
	}, []string{"hdr-secret-1", "pem-secret-2", "hdr-secret-3"})
}

// 受け入れ基準: ログの本文と Runner のパスが本文に入らない。
func TestCommentBodyHasNoLogTextOrRunnerPath(t *testing.T) {
	db := outboxDB(t)
	claimedLinearExecution(t, db, "変更は "+worktreePath+"/main.go と /worktrees/other にある\n"+
		"path=/worktrees/private/main.go 失敗（/worktrees/private/main.go） [/worktrees/private/main.go]\n"+
		"URL=https://example.com/worktrees/private/main.go")
	mustExec(t, db, `UPDATE executions SET error_class = 'work_failed',
		error_message = 'cannot open /worktrees/linear-1-r1/.git path=/worktrees/private/main.go 失敗（/worktrees/private/main.go） [/worktrees/private/main.go] URL=https://example.com/worktrees/private/main.go' WHERE id = 'linear-1-e1'`)
	mustExec(t, db, `INSERT INTO execution_events (execution_id, type, payload, created_at)
		VALUES ('linear-1-e1', 'log', '{"sourceId":"s1","stream":"harness","text":"LOG-BODY-CANARY"}', ?)`, testTime)
	mustExec(t, db, `INSERT INTO artifacts (id, execution_id, stage, kind, name, storage_path, sha256, size_bytes, redacted, created_at)
		VALUES ('log-1', 'linear-1-e1', 'work', 'log', 'work.log', 'artifacts/LOG-PATH-CANARY.log', 'x', 1, 0, ?)`, testTime)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning)
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning, model.ExecutionFailed)
	checkBody(t, onlyBody(t, db), []string{
		"変更は [REDACTED] と [REDACTED] にある\n" +
			"path=[REDACTED] 失敗（[REDACTED]） [[REDACTED]]\n" +
			"URL=https://example.com/worktrees/private/main.go\n",
		"- エラー情報: work_failed（cannot open [REDACTED] path=[REDACTED] 失敗（[REDACTED]） [[REDACTED]] URL=https://example.com/worktrees/private/main.go）\n",
	}, []string{"LOG-BODY-CANARY", "LOG-PATH-CANARY", worktreePath, "path=/worktrees", "（/worktrees", "[/worktrees", "artifacts/"})

	for _, tc := range []struct{ name, before, after string }{
		{"space", "変更は ", ""},
		{"equals", "path=", ""},
		{"colon", "path:", ""},
		{"backtick", "変更は `", "`"},
		{"parenthesis", "(", ")"},
		{"closing parenthesis", ")", ""},
		{"fullwidth parenthesis", "失敗（", "）"},
		{"closing fullwidth parenthesis", "）", ""},
		{"bracket", "[", "]"},
		{"closing bracket", "]", ""},
		{"single quote", "'", "'"},
		{"double quote", "\"", "\""},
		{"line start", "", ""},
		{"new line", "変更は\n", ""},
		{"letter", "path", ""},
		{"Japanese text", "変更は", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const localPath = "/worktrees/private/main.go"
			const urls = "http://example.com/worktrees/public/main.go https://example.com/worktrees/public/main.go"
			text := tc.before + localPath + tc.after + " " + urls
			redacted := tc.before + "[REDACTED]" + tc.after + " " + urls
			db := outboxDB(t)
			claimedLinearExecution(t, db, text)
			mustExec(t, db, `UPDATE executions SET error_class = 'work_failed', error_message = ? WHERE id = 'linear-1-e1'`, text)
			setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning)
			transition(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning, model.ExecutionFailed)
			checkBody(t, onlyBody(t, db), []string{
				"\n作業概要:\n" + redacted + "\n",
				"- エラー情報: work_failed（" + strings.Join(strings.Fields(redacted), " ") + "）\n",
			}, []string{localPath})
		})
	}
}

// Without the artifact reader (evaloomd without -data-dir) a succeeded body
// says the summary is unavailable instead of failing the transition.
func TestCommentBodyWithoutSummarySource(t *testing.T) {
	db := outboxDB(t)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing)
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing, model.ExecutionSucceeded)
	checkBody(t, onlyBody(t, db), []string{"- 実行ステータス: Completed\n", "\n作業概要: 取得できなかった\n"}, nil)
}
