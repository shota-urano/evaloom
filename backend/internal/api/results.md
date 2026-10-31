# Runnerの結果報告API

正本は`docs/specs/04-task-lifecycle.md`の3.2・3.4・6節、
`02-data-model.md`のexecutions・execution_blocks・5.2〜5.6節、
`05-runner.md`の3.3・3.6節、`06-worktree.md`の3.6節、
`12-notification.md`の3.1・3.4節。

`POST /api/executions/{id}/result`を既存の`protectRunner`で保護する。
本人のCookieでは呼べない。保存するトランザクション内で、認証したRunnerと
Executionの`runner_id`を照合する。不一致と未claimは403にし、既存の
`auth.reject`経路で監査記録を残す。存在しないExecutionは404にする。

仕様に要求JSONと通知要求の保存形式がないため、次の実装判断を採用した。

- 要求には`fromStatus`と`status`を指定する。成功は本文なしの204を返す。
  `fromStatus`が現在値と違う報告、同じ状態への再送、過去のExecutionへの報告、
  未知の状態、既存の遷移表にない報告は409にする。再送を成功扱いにはしない。
- Runnerが報告できるのは、claim済みの最新Executionの工程進行と停止だけとする。
  `pending`への復帰と`blocked`からの操作は本人用API、`interrupted`は
  Control Planeの回収処理に属する。Runnerからは409にする。
  再開点による工程の省略は、既存の遷移関数が許可する場合だけ受け付ける。
- 任意の結果メタデータは`worktreeId`、`branchName`、`baseSha`、
  `harnessHeadSha`、`verifiedSha`、`pushedSha`、`prNumber`、`prUrl`、
  `harnessSessionId`、`errorClass`、`errorMessage`とする。
  省略またはnullの任意文字列・数値は既存値を保持する。
  SHAの形式は既存のContext APIと同じ検証を使う。
  `baseSha`は初回設定と保存済みの値と同じ報告だけを受け付ける。
  保存済みの開始点SHAと異なる値は、Context固定の前後を問わず422にし、
  状態・Context・通知要求を変更しない。仕様06の開始点記録と仕様07の3.2節に従う。
  不一致時の応答コードは、既存のSHA整合性検証に合わせて422とした。
  保存済みの値を含めて`pushedSha`と`verifiedSha`の一致を確認し、不一致は422にする。
  `worktreeId`は同じTask・Project・Runnerに属し、未削除の既存行だけを参照できる。
  worktreeの作成やGate結果の生成は、それぞれの工程の実装に属する。
- `failed`には既定の分類を創作せず、`errorClass`を必須にする。
  `timed_out`と`cancelled`には仕様05の`timeout`と`cancelled`を保存する。
  それらと異なる分類が指定された場合や、進行・成功・Blockedへのエラー分類は400にする。
- `blocked`には`block: {cause, headSha, gateResultId?, detail?}`を必須にする。
  原因は既存の列挙値、HEADは既存のSHA検証を使う。
  `gateResultId`を指定した場合は同じExecutionの行だけを参照できる。
  Context Builderと共通の保存関数で未判断のBlockと`blocked`イベントを記録する。
- 通知要求は追加マイグレーション`0005_notification_requests.sql`の
  `notification_requests`に保存する。列は`execution_id`、`status`、`created_at`。
  `execution_id`はexecutionsへの外部キーとし、`(execution_id, status)`を主キーにする。
  状態は仕様12の`succeeded / failed / interrupted / timed_out / blocked`に限定する。
  `created_at`はUTCのRFC 3339で保存する。
  送信処理は別タスクであり、要求を`notification_sent`として記録しない。
- 通知対象は仕様12を採用し、`cancelled`では要求しない。
  同じExecutionが本人の続行後に再びBlockedになっても、同じ状態の通知要求は
  1件を保つ。DB再接続後も同じ主キーで重複を防ぐ。
- 通知要求、worktreeの保持、開始・終了時刻は既存の遷移関数内で保存する。
  `running`で初回の`started_at`、終端状態で`finished_at`を記録し、
  失敗・中断・タイムアウト・中止・Blockedではactiveのworktreeをretainedにする。
  成功時はworktreeをそのまま保持する。Context Builderもこの処理を共有する。
- `status_changed`イベントの`reason`は`runner_result`とする。
  監査記録は`runner:<id>/execution.result/execution/<Execution ID>`とし、
  Task IDと遷移前後を保存する。自由文や認証情報を監査記録に複製しない。
  Runnerは仕様05に従い、エラー本文とBlockの詳細から秘密情報を除いて送る。

要求の検証、メタデータ・Block保存、既存の状態遷移関数、通知要求、
イベント・監査記録を、claimと同じ`BEGIN IMMEDIATE`トランザクションに含める。
どれかの保存に失敗した場合は全体をロールバックする。
通知の実送信に失敗しても状態を取り消さない処理は、仕様12の送信側で実装する。
