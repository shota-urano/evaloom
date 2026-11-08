# 承認の本人用API（一覧・詳細・判断・再承認）

正本は`docs/specs/17-approval.md`の3.4・3.5.1・3.6・3.9・5章・6章と、
`00-overview.md`のQ55（既定24時間）・Q57（却下でfailed）。

- `GET /api/approvals`：`?status=`（approvalsのstatusの値）で絞り込む。
  IDの新しい順に、既存の`limit`・`cursor`のページングで返す。
- `GET /api/approvals/{id}`：approvalsの列に加えて、Executionの`prUrl`、
  `gateResults`（Execution詳細と同じ形）、`diffArtifacts`（diffのArtifactのID・sha256・大きさ。
  本文は既存の`GET /api/executions/{id}/artifacts/{artifactId}`で読む）を返す。
- `POST /api/approvals/{id}/decision`：`decision`・`contentVersion`・`ttlSec`（approveだけ任意）。
  判断と状態遷移・イベント・監査ログを同じ`BEGIN IMMEDIATE`のトランザクションで保存し、
  200で判断後の詳細を返す。
  - approve：approvalsを`approved`、`expires_at`を判断時刻＋`ttlSec`（無ければ24時間）にする。
    Executionは`waiting_approval`→`merging`（3.5.1）。
  - reject：approvalsを`rejected`にする。Executionは`failed`（タスクも`failed`）。
    PRは閉じない。continueの再実行で直せる（Q57）。
- `POST /api/approvals/{id}/renew`：期限切れの承認から、同じExecution・operation・target・
  content_version・params_hashで`pending`の承認を新しく作り、201で詳細を返す。

仕様に具体的な定義がない箇所は、既存APIの慣例に合わせた。

- 判断の順序：承認が`pending`でない（判断済み・無効・期限切れ）か、Executionが最新の
  `waiting_approval`でなければ409。そのうえで`contentVersion`が行の値と違えば422。
  `contentVersion`の欠けと形式不正（40桁または64桁の小文字16進でない）は、resumeの`headSha`と同じく400。
- `ttlSec`は1以上の整数。上限は仕様に無く、`time.Duration`で表せる範囲だけを技術的な上限にした。
  rejectに`ttlSec`を付けると400（`not_allowed`）。
- Q57の「理由 approval_rejected」は、`executions.error_class`に該当する値が無い（02の5.4）ため、
  `error_class`は空のまま`error_message`に`approval_rejected`を入れ、
  `status_changed`イベントの`reason`にも残す。
- 期限切れの判定：status=`expired`、またはstatus=`approved`で`expires_at`を過ぎ未使用のもの。
  後者はrenewの中で`expired`にし、Executionを`merging`から`waiting_approval`へ戻す
  （consumeで期限切れを断ったときと同じ戻り方）。同じExecutionに未使用の承認が別にあれば409。
  一覧の`?status=expired`は、まだ`expired`にしていない期限切れの`approved`を含まない。
- 監査ログ：判断は`owner/approval.decide/approval/<ID>`に`taskId`・`executionId`・`operation`・
  `decision`・`contentVersion`・（approveなら）`expiresAt`。renewは`owner/approval.renew/approval/<元のID>`に
  `previousStatus`・`renewedApprovalId`ほか。
- イベントの`reason`：`approval_approved`・`approval_rejected`・renewでの戻りは`approval_expired`。
- Runnerトークンでの呼び出しは、既存の本人用APIの境界で403と`auth.reject`の監査記録になる（6章）。

## 消費API（Runner用）

`POST /api/approvals/{id}/consume`（17の3.5.2、5章）。Runnerトークンだけで呼べる。
本人のセッションは403と`auth.reject`の監査記録になる（6章）。
承認の Execution の`runner_id`と違う Runner は403（`execution_runner_mismatch`）。

- 入力：Git PublisherがGitHubから読んだPRの状態。`headSha`（40桁または64桁の小文字16進）、
  `baseBranch`、`open`、`merged`（すべて必須）。
- 判定は1つの`BEGIN IMMEDIATE`のトランザクションで、次の順に行う。応答はどれも200で、
  `allowed=true`なら`contentVersion`と`mergeMethod`（Q56の`merge`）、`false`なら`reason`を返す。

| 順 | 状況 | reason | approvals | Execution |
|---|---|---|---|---|
| 1 | 使用済み（consumed、またはuse_count≥max_uses） | `already_used` | 変えない | 変えない |
| 2 | expired / invalidated / pending / rejected | `expired` / 無効の理由 / `not_approved` | 変えない | 変えない |
| 3 | Executionが最新の`merging`でない | 409 | 変えない | 変えない |
| 4 | 中止要求がある | `cancelled` | `invalidated`（cancelled） | `cancelled`（タスクも） |
| 5 | 有効期限切れ | `expired` | `expired` | `merging`→`waiting_approval` |
| 6 | 対象が変わった（`merged`、`open=false`、`baseBranch`が違う） | `target_changed` | `invalidated`（target_changed） | `merging`→`waiting_approval` |
| 7 | head SHAが変わった | `content_changed` | `invalidated`（content_changed） | `merging`→`waiting_approval` |
| 8 | 保存したtarget・content_versionから作ったparams_hashが行の値と違う | `content_changed` | `invalidated`（content_changed） | `merging`→`waiting_approval` |
| 9 | 一致 | - | `consumed`（use_count+1、consumed_at） | `merging`のまま（マージ結果の報告を待つ） |

- 9の消費は`status='approved' AND use_count < max_uses AND expires_at > now`を条件にした更新で行い、
  更新が0件なら`already_used`で断る。
- 監査ログ：呼び出しごとに`runner:<ID>/approval.consume/approval/<ID>`（`allowed`・`reason`・`headSha`ほか）。
  無効化は既存と同じく`system/approval.invalidate`。
- イベントの`reason`：戻りは`approval_expired`・`approval_target_changed`・`approval_content_changed`、
  中止は既存の`task_cancelled`。
- reasonの値のうち`already_used`・`expired`・`not_approved`は仕様に無いため、ここで決めた。

## 承認済みの操作（claim）とマージの結果の報告

正本は17の3.5.1・3.5.2・6章。

- claim（`POST /api/runners/{id}/claim`）は、通常の Execution より先に「承認済みの操作」を探す。
  条件は、approvals が `approved`・未使用・`merge_pull_request`、Execution が最新の `merging`、
  タスクが `waiting_approval`、Execution と Project の Runner が呼び出し元であること。
  `decided_at`・ID の順で1件返す。merging は同時実行の枠に数えない（Q59）ため、空き枠の判定より前に探す。
  応答は `{"execution":{"id","taskId"},"project":{...},"operation":{"approvalId","operation","target","contentVersion"}}`。
  claim は状態を変えない。使用は consume だけが行うため、同じ操作を2回渡してもマージは1回だけになる。
  通常の Execution の claim の応答には `"operation": null` を付ける。
- マージの結果は既存の `POST /api/executions/{id}/result` で `fromStatus=merging` から報告する。
  - 成功：`status=succeeded`、`mergeCommitSha`（40桁または64桁の小文字16進、必須）。
  - 失敗：`status=failed`、`errorClass=publish_failed`、`errorMessage` は
    `content_changed`（GitHub が sha の不一致で拒否、6章）・`merge_rejected`（その他の拒否）・
    `merge_unknown`（結果が分からない、3.5.3）のどれか。
  - 他の項目（usage、Gate 結果、PR の項目など）は 400。`mergeCommitSha` を merging 以外から送ると 400。
  - 同じ Execution に、consume 済みで結果未記録の承認が無ければ 409（マージは許されていない）。
  - approvals.result に `{"mergeCommitSha":...}` または `{"error":<理由>}` を保存する。
    `content_changed` のときは承認を `invalidated`（content_changed）にし、`system` の監査記録を残す。
  - 状態遷移・通知要求（succeeded / failed）・`status_changed`（reason `runner_result`）・
    `runner:<id>/execution.result` の監査記録は既存の遷移関数で同じトランザクションに保存する。
- 仕様に無いため、ここで決めた値：claim の応答の `operation` の形、`mergeCommitSha` の項目名、
  失敗理由 `merge_rejected`、GitHub の拒否時に Execution を `failed` にすること。

## マージの結果が分からないときの照合（Runner）

正本は17の3.5.3。マージ要求に応答が無い・5xx・読めない応答のとき、Git Publisherは
マージを再び要求せず、PRを1回だけ読み直す（`GET /repos/{repo}/pulls/{n}`）。

- マージ済みで、headのSHAがcontent_versionと一致し、`merge_commit_sha`が40桁または64桁なら、
  その値を`mergeCommitSha`として`succeeded`を報告する。
- 開いたまま・読めないときは`failed`・`publish_failed`・`merge_unknown`を報告する。
- 仕様の表に無い状態（マージ済みだがheadがcontent_versionと違う、閉じていてマージされていない、
  マージ済みだが`merge_commit_sha`が読めない）も、仕様に無いため安全側の`merge_unknown`にした。
  `succeeded`の報告には`mergeCommitSha`が必須のため、マージ commit を読めないときは成功として報告できない。
- 読み直しは再試行しない（1回だけ）。
