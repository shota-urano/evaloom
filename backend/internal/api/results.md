# Runnerの結果報告API

正本は`docs/specs/04-task-lifecycle.md`の3.4・3.4.1・3.5、`02-data-model.md`の
stage_runs・check_results・owner_decisions・approvals・5.2・5.3・5.6、
`17-approval.md`の3.3・3.4、契約`harness-contract-v1.md`の6.5・8章・9章。

`POST /api/executions/{id}/result`を既存の`protectRunner`で保護する。
本人のCookieでは呼べない。保存するトランザクション内で、認証したRunnerと
Executionの`runner_id`を照合する。不一致と未claimは403にし、既存の
`auth.reject`経路で監査記録を残す。存在しないExecutionは404にする。

- 送れるのは`type`が`stage_started`と`stage_finished`の2種類だけ。ほかの`type`、
  未知の項目（旧来の`fromStatus`や開発の項目を含む）は400。
- `stage_started`は`{"attempt": n}`を200で返す。同じ`requestArtifactId`の再送は保存済みの
  attemptを返し、何も書かない。始めてよい段は、claimedならresume_stage、無ければ
  Profileにprepareがあればprepare、無ければwork。段の途中なら前の段が終わった後の次の段だけ。
  それ以外は409。`requestArtifactId`は同じExecution・同じ段の`request_json`の成果物でなければ400。
- `stage_finished`は`{"next": ...}`を200で返す。`next`が`publish`のときは
  `checkedContentId`と`approvals`（省いた操作だけ）を添える。表の各行の処理は
  `internal/store/stage_reports.go`。全項目を必須とし、nullableな項目もnullで送らせる。
- 結果JSONが契約に合わない（未知の項目、段で書けないstatus、スペシャルハーネスが書けない
  errorClass、宣言していない操作の承認要求、必須のcontentIdの欠け、question.jsonの形）ときは
  `contract_error`でExecutionを失敗にし、200（`next: stop`）を返す。
- 同じ内容の再送（キーの順と空白は問わない）は、監査記録に残した応答をそのまま返し、
  行もイベントも増やさない。同じ(stage, attempt)への違う内容は409。
- 通知要求とLinearへの書き込みは`task.TransitionExecutionTx`の遷移が行う。
- 監査記録は`runner:<id>/execution.result/execution/<Execution ID>`。detailに
  type、stage、attempt、遷移前後、報告の内容のSHA-256、応答を残す。

要求の検証の後、すべての書き込みを1つの`BEGIN IMMEDIATE`トランザクションで行い、
どれかが失敗したら全体をロールバックする。
