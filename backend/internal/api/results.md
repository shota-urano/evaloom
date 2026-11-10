# Runnerの結果報告API

正本は`docs/specs/04-task-lifecycle.md`の3.2・3.4・6節、
`02-data-model.md`のexecutions・5.2〜5.5節、`05-runner.md`、`12-notification.md`の3.1・3.4節。

`POST /api/executions/{id}/result`を既存の`protectRunner`で保護する。
本人のCookieでは呼べない。保存するトランザクション内で、認証したRunnerと
Executionの`runner_id`を照合する。不一致と未claimは403にし、既存の
`auth.reject`経路で監査記録を残す。存在しないExecutionは404にする。

worktree、Gate 2・3、Git Publisher、setup と基準値の測定、承認待ちとマージの報告は
software-engineer へ移したため受け付けない（`docs/specs/19-routing-and-stages.md` 3.11）。
それらの項目（`worktreeId`、`baseSha`、`verifiedSha`、`pushedSha`、`prNumber`、`prUrl`、
`gateResults`、`block`、`baseline`、`testRuns`、`setupStatus`、`worktree`、`audit`、`mergeCommitSha` など）は
未知の項目として400になる。

- 要求には`fromStatus`と`status`を指定する。成功は本文なしの204を返す。
  `fromStatus`が現在値と違う報告、同じ状態への再送、過去のExecutionへの報告、
  未知の状態、既存の遷移表にない報告は409にする。状態遷移の再送を成功扱いにはしない。
- Runnerが報告できるのは、claim済みの最新Executionの工程進行と停止
  （`preparing`・`running`・`verifying`・`publishing`・`succeeded`・`failed`・`cancelled`・`timed_out`）だけとする。
  `blocked`の原因は開発の工程（Gate・Context・Git）だけだったため、Runnerからは409にする。
  `pending`への復帰と`blocked`からの操作は本人用API、`interrupted`は
  Control Planeの回収処理に属する。
- 任意の結果メタデータは`harnessSessionId`、`errorClass`、`errorMessage`とする。
  省略またはnullの任意文字列は既存値を保持する。
- `failed`には既定の分類を創作せず、`errorClass`を必須にする。
  `timed_out`と`cancelled`には仕様05の`timeout`と`cancelled`を保存する。
  それらと異なる分類が指定された場合や、進行・成功へのエラー分類は400にする。
- `succeeded`には`usage.wallTimeMs`を必須にし、`usage_records`に保存する。
- 通知要求は`notification_requests`に保存する。列は`execution_id`、`status`、`created_at`。
  `(execution_id, status)`を主キーにする。
  通知対象は仕様12を採用し、`cancelled`では要求しない。
- 通知要求、開始・終了時刻は既存の遷移関数内で保存する。
  `running`で初回の`started_at`、終端状態で`finished_at`を記録する。
- `status_changed`イベントの`reason`は`runner_result`とする。
  監査記録は`runner:<id>/execution.result/execution/<Execution ID>`とし、
  Task IDと遷移前後を保存する。自由文や認証情報を監査記録に複製しない。

要求の検証、メタデータ保存、既存の状態遷移関数、通知要求、
イベント・監査記録を、claimと同じ`BEGIN IMMEDIATE`トランザクションに含める。
どれかの保存に失敗した場合は全体をロールバックする。
