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
