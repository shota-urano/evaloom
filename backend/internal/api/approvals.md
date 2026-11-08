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

## 承認待ちのPRの定期照合（Runner）

正本は17の3.6とQ58。承認待ちの間に本人がGitHubでPRを直接マージ・クローズした場合を、
Runnerが承認待ちのPRを定期的に読んで照合する。

- 承認待ちのPRの一覧：Runnerの既存の定期処理であるheartbeatの応答に`waitingApprovals`を足した。
  要素は`executionId`・`taskId`・`target`（Executionの最新のmerge承認のtarget）・`project`
  （claimの`project`と同じ形）。対象は呼び出し元のRunnerが実行した、最新で`waiting_approval`の
  Execution（タスクも`waiting_approval`）。`merging`は含めない（承認済みの操作がconsumeの前にPRを
  読み、対象が変わっていれば`waiting_approval`へ戻すため、次の照合で拾われる）。
- PRの読み取り：Git Publisherの読み取り専用のinstallation token（contents・pull_requestsはread）で
  `GET /repos/{repo}/pulls/{n}`を読む。マージの権限は使わない。読めないPRは次の照合で読み直す。
  Runnerが処理中のExecution（承認済みの操作を含む）は読まない。
- 照合の結果は既存の`POST /api/executions/{id}/result`で`fromStatus=waiting_approval`から報告する。
  - マージ済み：`status=succeeded`、`mergeCommitSha`（40桁または64桁の小文字16進、必須）。
    Executionは`succeeded`、タスクは`completed`。
  - マージされずに閉じた：`status=failed`、`errorMessage=pull_request_closed`、`errorClass`は送らない。
    Executionとタスクは`failed`。02の5.4に該当する`error_class`が無いため、Q57の`approval_rejected`と
    同じく`error_class`は空のまま`error_message`に入れる。
  - 開いたままなら報告しない。
  - どちらも、未使用（pending / approved）の承認を`invalidated`（target_changed）にし、
    `system/approval.invalidate`の監査記録を残す。状態遷移・通知要求・`status_changed`イベント
    （reason `pull_request_merged` / `pull_request_closed`）・`runner:<id>/execution.result`の監査記録
    （`reason`、マージなら`mergeCommitSha`）を同じ`BEGIN IMMEDIATE`のトランザクションで保存する。
  - 他の項目は400。merge承認の無いExecution、最新でないExecution、`waiting_approval`以外からの報告は409。
- 仕様に無いため、ここで決めた値：照合の間隔（1分。heartbeatのたびではなく、前回の照合から1分経った
  heartbeatの後に1回）、heartbeatの応答の`waitingApprovals`の形、失敗理由`pull_request_closed`、
  イベントの`reason`の`pull_request_merged` / `pull_request_closed`。

## push 直前の確認（publish-check）と直接 push

正本は17の3.7・5章・6章。

- `POST /api/executions/{id}/publish-check`（Runner用）。入力は空のJSON（`{}`）。ほかの項目は400。
  応答は200で `{"cancelRequested": <bool>, "approvalPolicy": {"version": 1, "mergePullRequest": "require_approval" | "direct_push"}}`。
  - `cancelRequested` は Execution の `cancel_requested_at` の有無（heartbeat の中止要求と同じ値）。
  - `approvalPolicy` は呼び出した時点の Project の値。保存値が3.2の形で `direct_push`（version 1）のときだけ
    `direct_push` を返し、ほかの値・読めない値は既定の `require_approval` を返す（直接 push の側へ倒さない）。
  - Execution の `runner_id` と違う Runner は403（`execution_runner_mismatch`、`auth.reject` の監査記録）。
    本人のセッションは403（6章）。Execution が無ければ404。最新の `publishing` でなければ409。
  - 状態・イベント・監査記録は変えない（読むだけ）。
- Runner（`internal/runner/publish_check.go`、`internal/publisher/publish.go`）:
  - claim の `project.approvalPolicy.mergePullRequest` が `direct_push` のときだけ直接 push を試みる。
    claim の値が `require_approval` なら publish-check を呼ばず、従来どおり PR を作る（3.7.1 の2「違えば安全側」）。
  - 直接 push では、ローカルの確認（push 先の ref、HEAD と verified_sha の一致、作業ツリー）の後、
    push の直前の最後の確認として publish-check を1回呼ぶ。再試行しない。
    届かない・200以外・読めない応答なら push しない（`failed`・`publish_failed`）。
    `cancelRequested` なら Execution を中止する。現在値が `direct_push` でなければ PR を作る経路へ切り替える。
  - push 先の ref は、`evaloom/` で始まるブランチに加えて、直接 push のときだけ Project の Base Branch と
    完全に一致する名前を許す（3.7.1 の4）。`git push <remote> <verified_sha>:refs/heads/<base_branch>` を
    強制なしで1回送り、リモートの Base Branch の SHA が verified_sha と一致することを確かめる。
  - 一致したら既存の工程内報告（`publishing` のまま `pushedSha`・`branchName`）で pushed_sha を記録し、
    PR を検索も作成もせずに `succeeded` を報告する。PR が無いため Control Plane は承認待ちを作らず、
    Execution は `succeeded`、タスクは `completed` になる。
- 仕様に無いため、ここで決めた値:
  - publish-check の入力（空のJSON）と応答の形（`cancelRequested`・`approvalPolicy`）。
  - 直接 push の後の `pushed` の報告の `branchName` は Execution の `branch_name`（`evaloom/...`）とした。
    既存の結果API が `branchName` を executions.branch_name と照合する（不一致は422）ため。
    push 先が Base Branch だったことは、PR が無いことと Project の承認ポリシーから分かる。
  - 3.7.2 の4（作業ブランチを push するか）：push しない。作業ブランチは Runner のクローンに残る。
    外部への書き込みを Base Branch への1回に限るため。
  - PR を作る経路（claim の時点で `require_approval`、または publish-check で切り替えた場合）の
    push 直前の中止確認は、従来どおり heartbeat を使う。5章の「heartbeat の応答を使っていた確認を置き換える案」は、
    直接 push の経路だけで適用した。
- fast-forward できないとき（3.7.3）は本タスクの範囲外。現時点では push が拒否されて `publish_failed` になる。
