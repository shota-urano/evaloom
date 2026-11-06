---
status: draft
範囲: 承認の対象と承認ポリシー、承認と操作内容の結び付け（head SHA が変わったら無効）、承認の原子的な消費、承認後の PR マージ、PR マージを承認対象から外した Project のベースブランチへの直接 push（Q35）
---

# 17. 承認 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend（承認画面は Frontend） ｜ **範囲**: requirements 8.2 の工程14（必要に応じた承認）、15.3、15.8、16.9、Q35

この文書は Phase 2 の下書きである。requirements で確定していない点は「案」とし、00-overview.md の要確認リスト（Q53〜Q63）に載せている。

## 1. 目的

- 重要操作を人間の承認対象にできるようにする（条件21、requirements 15.3）。
- 承認を、操作・対象・内容の版に結び付ける。承認した後に対象の内容（PR の head commit SHA など）が変わったら、その承認では操作を実行しない（条件22、requirements 15.8）。
- 承認は本人だけが、操作の直前に行う。事後承認にしない（requirements 15.3, 15.5）。
- 承認ポリシーで PR マージを承認対象から外した Project では、検証を通った commit を PR を経ずにベースブランチへ push する（requirements 8.5, 15.3、Q35）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | PR の作成（[`11-git-publisher.md`](./11-git-publisher.md)）、本人の承認・却下（管理画面）、Runner が GitHub から読んだ PR の現在の状態、Project の承認ポリシー |
| 出力 | approvals、tasks / executions の状態、PR のマージ、ベースブランチへの push、audit_logs、通知（[`12-notification.md`](./12-notification.md)、[`16-linear-feedback.md`](./16-linear-feedback.md)） |

## 3. 処理詳細

### 3.1 承認の対象（requirements 15.3、Q53）

requirements 15.3 の承認対象操作のうち、Evaloom が Phase 2 で実際に行う操作は PR マージだけとする（案）。ほかの操作は Phase 2 でも Evaloom とハーネスが行わず、Phase 1 と同じく Runner の実行権限で拒否する（[`09-sandbox.md`](./09-sandbox.md) 3.7）。

| 操作 | operation の値 | Phase 2 の扱い |
|---|---|---|
| PR マージ | `merge_pull_request` | 承認の後に Evaloom が行う（3.4）。承認ポリシーで外した Project では行わず、ベースブランチへ直接 push する（3.7） |
| 本番環境へのデプロイ | `deploy_production` | 行わない（権限で拒否） |
| 外部メール送信 | `send_external_email` | 行わない（権限で拒否） |
| 重要ファイル削除 | `delete_important_file` | 行わない（権限で拒否） |
| 認証・権限変更 | `change_auth` | 行わない（権限で拒否）。本人が管理画面で行う設定変更は承認の仕組みを通さない（requirements 15.3） |
| 共通契約・評価基準・中央セキュリティポリシーの変更 | `change_contract` / `change_evaluation_criteria` / `change_security_policy` | 行わない（権限で拒否） |

operation の値は、将来ほかの操作を足すときにも approvals を作り直さずに済むよう、列挙値として先に決めておく。Phase 2 で approvals に書くのは `merge_pull_request` だけである。

requirements 8.2 の工程14「実行結果に対する承認」は、Phase 2 では PR マージの承認として実現する（案）。

### 3.2 承認ポリシー（requirements 15.3、Q60）

projects.approval_policy（Phase 1 は保存と表示だけ、Q25）を、Phase 2 で次の形にする（案）。

```json
{ "version": 1, "mergePullRequest": "require_approval" }
```

| 値 | 意味 |
|---|---|
| `require_approval`（既定） | PR を作り、本人の承認の後に Evaloom がマージする（3.3〜3.5） |
| `direct_push` | PR を作らず、Gate 2・3 を通った commit をベースブランチへ fast-forward で push する（3.7） |

- 外せるのは PR マージだけで、ほかの操作の項目は持たせない（requirements 15.3）。未知の項目や値は 400 で拒否する。
- 既定は `require_approval` とする（requirements 15.3）。Phase 1 で保存した値がこの形でなければ、マイグレーションで `require_approval` にする（案）。
- `direct_push` にする変更は、管理画面で本人が確認ダイアログを経て行う。確認ダイアログでは、PR のレビューに任せていた範囲（既存テストの書き換え、テストの実行設定・CI の定義の変更など）を確認する人がいなくなることを示す（requirements 15.3）。変更は audit_logs に残す。
- Runner とハーネスは承認ポリシーを変更できない（requirements 15.3）。Runner 用 API に変更の経路を作らない。

### 3.3 承認待ちの発生

承認ポリシーが `require_approval` の Project で、Git Publisher が PR を作成または更新したら（[`11-git-publisher.md`](./11-git-publisher.md) 3.3）、Control Plane は次を1つのトランザクションで行う。

1. その PR を対象にした未使用の承認（status が `pending` / `approved`）があれば、`invalidated`（理由 `superseded`）にする。PR に新しい commit が足されたためである。
2. approvals に `pending` の行を作る。結び付ける値は 3.4 の表のとおり。
3. Execution を `waiting_approval`、タスクを `waiting_approval` にする。
4. 承認待ちを通知する（macOS 通知、Linear 起因のタスクなら Linear コメント）。

`waiting_approval` の Execution は同時実行の枠に数えない（案、Q59）。プロセスは動いていないためである。

### 3.4 承認と操作内容の結び付け（requirements 15.8）

| 項目 | approvals の列 | PR マージでの値 |
|---|---|---|
| 操作の種類 | operation | `merge_pull_request` |
| 対象 | target | `{ "repository": "<owner>/<repo>", "prNumber": 12, "baseBranch": "main", "headBranch": "evaloom/task-001-..." }` |
| 内容の版 | content_version | PR の head commit SHA（executions.pushed_sha） |
| 操作パラメータのハッシュ | params_hash | operation・target・content_version・マージ方式（Q56）を正規化した JSON の SHA-256 |
| 承認者 | decided_by | `owner`（本人だけ、requirements 15.5） |
| 使用回数の上限 | max_uses | 1 |
| 有効期限 | expires_at | 承認した時刻 + 有効時間。有効時間の既定値は Q55 |

本人は管理画面で、承認待ちの対象（PR の URL、head SHA、差分、Gate の結果）を見て、承認か却下を選ぶ。送信時に、画面に表示していた content_version を一緒に送る。Control Plane は、送られた値が行の content_version と一致しなければ 422 を返す（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.8 の Blocked からの続行と同じ考え方）。

### 3.5 承認の後のマージ（requirements 15.8）

#### 3.5.1 実行する主体（Q54）

Control Plane は GitHub の書き込み権限を持たない（[`01-architecture.md`](./01-architecture.md) 3.1）。そのためマージは Runner の Git Publisher が行う案とする。

1. 本人が承認すると、approvals は `approved`、Execution は `merging` の待ち（pending のマージ）になる。
2. Runner は claim の応答で、通常の Execution とは別の種類の仕事「承認済みの操作」を受け取る（claim の応答に `operation` を足す。[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.3.1 の拡張）。
3. Runner の Git Publisher が 3.5.2 の手順でマージする。

GitHub 側の権限は 3.8。

#### 3.5.2 操作の直前の確認と消費

1. Git Publisher は GitHub から PR の現在の状態（head SHA、base ブランチ、開いているか、マージ済みか）を読む。
2. Runner は読んだ値を添えて `POST /api/approvals/{id}/consume` を呼ぶ。Control Plane は1つの書き込みトランザクションの中で、次の表のとおり判定する（requirements 15.8）。

| 状況 | 扱い | approvals の status |
|---|---|---|
| 内容が一致する（head SHA、対象、params_hash） | 消費して、操作してよいと返す | `consumed`（use_count=1） |
| head SHA が変わった | 操作しない。承認は無効 | `invalidated`（理由 `content_changed`） |
| 対象が変わった（base ブランチ、PR が閉じた） | 操作しない。承認は無効 | `invalidated`（理由 `target_changed`） |
| 使用済み | 操作しない | 変えない |
| 有効期限切れ | 操作しない。再承認を求める | `expired` |
| タスクの中止要求がある | 操作しない | `invalidated`（理由 `cancelled`） |

   消費は `UPDATE approvals SET status='consumed', use_count=use_count+1, consumed_at=? WHERE id=? AND status='approved' AND use_count < max_uses AND expires_at > ?` のように、期待する現在値を条件にした更新で行う。更新できた行が0件なら操作しない（requirements 15.8「承認の消費は DB で原子的に管理する」）。

3. 消費できたら、Git Publisher は GitHub のマージ API に `sha` パラメータとして content_version を渡してマージする。PR の head がその SHA と一致しなければ GitHub が拒否する（requirements 15.8）。
4. マージの結果（マージ commit の SHA）を報告する。Execution は `succeeded`、タスクは `completed` になる。マージ完了を通知する。

`sha` パラメータではベースブランチの変更までは防げない（requirements 15.8）。ベースブランチ側は、GitHub のルールセット（PR と CI を必須にする、[`11-git-publisher.md`](./11-git-publisher.md) 3.5.1 のルールセット A）で守る。

#### 3.5.3 結果が分からないとき（requirements 15.8）

マージの要求を送った後に応答が無いなど、結果が分からなくなったら、同じ承認で再びマージを要求しない。Git Publisher は GitHub から PR を読み直し、次のとおり扱う。

| PR の状態 | 扱い |
|---|---|
| マージ済みで、マージされた head が content_version と一致 | 成功として報告する |
| 開いたまま | 失敗（`publish_failed`、理由 `merge_unknown`）として報告する。承認は消費済みのため、本人が再承認する |
| 読めない | 同上。本人が GitHub で状態を確かめる |

### 3.6 却下・無効・中止

| 契機 | approvals | タスク・Execution |
|---|---|---|
| 本人が却下 | `rejected` | タスクを `failed`（理由 `approval_rejected`）にする案（Q57）。PR は開いたまま残し、本人は continue の再実行で直せる |
| PR に新しい commit が足された（再実行の continue など） | `invalidated`（`superseded`） | 3.3 で新しい承認待ちを作る |
| 有効期限切れ | `expired` | `waiting_approval` のまま。本人は再承認を求める操作（`POST /api/approvals/{id}/renew`、追加案）で新しい承認待ちを作る |
| タスクの中止（本人、または Linear 起因、[`15-linear-intake.md`](./15-linear-intake.md) 3.7） | 未使用なら `invalidated`（`cancelled`） | `cancelled`。PR は閉じない |
| 承認待ちの間に、本人が GitHub 上で直接マージ・クローズした | `invalidated`（`target_changed`） | マージなら `completed`、クローズなら `failed` とする案（Q58）。Runner が承認待ちの PR の状態を定期的に読んで照合する |

### 3.7 ベースブランチへの直接 push（requirements 8.5, 15.3、Q35）

承認ポリシーが `direct_push` の Project に限り、Git Publisher は PR を作らず、Gate 2・3 を通った commit（verified_sha）をベースブランチへ fast-forward で push する。

#### 3.7.1 push 直前の確認（[`11-git-publisher.md`](./11-git-publisher.md) 3.1 の拡張）

1. 中止要求の有無を Control Plane に問い合わせる（11 の 3.1 と同じ）。
2. 承認ポリシーの現在値を Control Plane から読み直す（Q61）。claim の時点の値と違えば、安全側（PR を作る）を選ぶ。`direct_push` から `require_approval` へ変えた直後に直接 push しないためである。問い合わせできなければ push しない（Fail Closed）。
3. HEAD が verified_sha と一致し、作業ツリーに未コミットの変更が無いことを確かめる（11 の 3.1 と同じ）。
4. push 先の ref の確認を次のように変える。`evaloom/` で始まるブランチに加えて、承認ポリシーが `direct_push` のときに限り、Project の Base Branch と完全に一致する ref だけを許す。それ以外の ref には push しない。

#### 3.7.2 push

1. `git push <remote> <verified_sha>:refs/heads/<base_branch>` で push する。強制 push はしない（11 の 3.2 と同じ）。
2. push 後にリモートのベースブランチの SHA を読み、verified_sha と一致することを確かめる。
3. pushed_sha を記録する。Execution は `succeeded`、タスクは `completed` になる。PR は作らない。
4. ブランチ `evaloom/<task-id>-<短い名前>` は作業の記録として残す（案。push するかは実装時に確定）。

#### 3.7.3 fast-forward できないとき（Q62）

開始点 SHA（base_sha）の後に、ベースブランチが先へ進んでいると fast-forward できない。テストした commit は古い開始点の上にあるため、そのまま押し込まない。案は「PR を作る経路へ切り替え、本人のレビューに回す」とする。PR を作ったら承認ポリシーにかかわらず PR のマージは本人が GitHub で行う（Evaloom はこの PR をマージしない）。

### 3.8 GitHub 側の権限（Q54, Q63）

Phase 1 の設定（[`11-git-publisher.md`](./11-git-publisher.md) 3.5.1）では、ルールセット B（ベースブランチの更新は Repository admin だけ）によって App はマージも push もできない。Phase 2 では次のように変える案とする。本人の判断が要る。

| 対象 | ルールセット A（PR 必須・CI 必須・force push と削除の禁止） | ルールセット B（更新は admin だけ） |
|---|---|---|
| `require_approval` の Project のリポジトリ | Bypass は空のまま | Bypass に evaloom-publisher を足す。App は PR と CI を経たマージでだけ main を更新できる |
| `direct_push` の Project のリポジトリ | force push と削除の禁止を別のルールセット C（Bypass 空）に分け、A の Bypass に App を足す | Bypass に App を足す |

- A の Bypass を空のままにすれば、App が持つ書き込み権限でも、PR と必須ステータスチェック `ci` を経ずに main を更新できない。マージの承認を Evaloom で行っても、GitHub 側の CI の条件は残る。
- `direct_push` の Project では PR を経ないため、GitHub の CI は push の後に main で動く。push の前の確認は Gate 2・3 だけになる（requirements 15.3 が許す範囲）。
- App の権限（contents と pull_requests の write、metadata の read）は変えない。マージに必要な権限はこの範囲に含まれる（実装時に GitHub の公式文書で確かめる）。
- ルールセットの Bypass の種類（常に許可か、PR のときだけか）は、実機で確かめて決める。

### 3.9 データモデル（Phase 2 で追加）

#### approvals

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| task_id | TEXT FK→tasks | |
| execution_id | TEXT FK→executions | 承認待ちを作った Execution |
| operation | TEXT | 3.1 の値 |
| target | TEXT (JSON) | 3.4 の対象 |
| content_version | TEXT | PR の head commit SHA |
| params_hash | TEXT | 3.4 |
| status | TEXT | `pending` / `approved` / `rejected` / `consumed` / `invalidated` / `expired` |
| invalidated_reason | TEXT NULL | `content_changed` / `target_changed` / `superseded` / `cancelled` |
| decided_by | TEXT NULL | `owner` |
| decided_at | TEXT NULL | |
| expires_at | TEXT NULL | 承認した時点で決める |
| max_uses | INTEGER | 1 |
| use_count | INTEGER | 0 から始める |
| consumed_at | TEXT NULL | |
| result | TEXT (JSON) NULL | マージ commit の SHA、失敗の理由 |
| created_at | TEXT | |

- 同じ execution_id と operation で、status が `pending` / `approved` の行は1件までとする（部分一意インデックス）。
- 承認の判断、消費、無効化はすべて audit_logs に残す（requirements 15.2 監査ログ）。actor は判断が `owner`、消費が `runner:<id>`、無効化が `system`。

#### 状態の値の追加

| 対象 | 追加する値 | 対応するタスク状態 | 終端か |
|---|---|---|---|
| executions.status | `waiting_approval` | `waiting_approval` | 終端ではない。同時実行の枠に数えない（Q59） |
| executions.status | `merging` | `waiting_approval` | 終端ではない。同時実行の枠に数えない（Q59） |
| tasks.status | `waiting_approval`（02 の 5.1 で Phase 1 は使わないとした値） | - | - |

`succeeded` は「PR を作成した」から「マージした、またはベースブランチへ push した」に意味が変わる。`require_approval` の Project では、PR を作った時点で `waiting_approval` になる。

### 3.10 既存の仕様への影響

Phase 2 の実装に入るときに、次の確定済みの仕様を書き換える必要がある。書き換えたら、その仕様の status を draft に戻す。

| 仕様 | 書き換える箇所 |
|---|---|
| [`02-data-model.md`](./02-data-model.md) | 5.1 の `waiting_approval` を「使う」に。5.2 に `waiting_approval` / `merging` を追加。3.3 から approvals を外し、本書 3.9 を参照 |
| [`03-registry.md`](./03-registry.md) | 3.7 の承認ポリシーを「保存と表示のみ」から 3.2 の形と効果に |
| [`04-task-lifecycle.md`](./04-task-lifecycle.md) | 3.2 の遷移表に承認待ち・マージ・却下を追加。3.3.1 の claim の応答に `operation` を追加 |
| [`05-runner.md`](./05-runner.md) | 3.8「Runner が行わない操作」から PR マージを外し、承認済みのマージだけを行うと書く |
| [`09-sandbox.md`](./09-sandbox.md) | 3.7 の PR マージの行を、承認済みのときだけ Git Publisher が行うに |
| [`11-git-publisher.md`](./11-git-publisher.md) | 3.1 のブランチ名の接頭辞の確認、3.5 の「マージをしない」とルールセットの設定値、4 の「Evaloom はマージしない」 |
| [`12-notification.md`](./12-notification.md) | 3.1 に承認待ちとマージ完了の通知を追加 |
| [`13-management-ui.md`](./13-management-ui.md) | Approvals 画面と Dashboard の承認待ちを追加 |

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 承認対象操作 | 本番デプロイ、PR マージ、外部メール送信、重要ファイル削除、認証・権限変更、共通契約・評価基準・中央セキュリティポリシーの変更 | requirements 15.3 |
| 承認の時点 | 操作の直前。事後承認にしない | requirements 15.3 |
| 権限と承認状態の確認 | 実行開始時と、副作用のある操作の直前 | requirements 15.3 |
| 承認者 | 本人だけ | requirements 15.5 |
| 承認の使用回数 | 原則1回 | requirements 15.8 |
| head SHA が変わった | 承認は無効 | requirements 15.8、条件22 |
| 対象が変わった | 承認は無効 | requirements 15.8 |
| 使用済み | 実行しない | requirements 15.8 |
| 有効期限切れ | 再承認を求める | requirements 15.8 |
| 承認の消費 | DB で原子的に管理する | requirements 15.8 |
| PR マージ | GitHub のマージ API の `sha` パラメータで PR の HEAD との一致を求め、ブランチ保護と組み合わせる | requirements 15.8 |
| 承認ポリシーで外せる操作 | PR マージだけ。既定は承認対象のまま | requirements 15.3 |
| 外した Project の push | Gate 2・3 を通った commit をベースブランチへ fast-forward で push。PR を作らない | requirements 8.5, 15.3、Q35 |
| 権限設定の変更 | 本人だけ。Runner とハーネスには許可しない | requirements 15.3 |
| Fail Closed | 認証・承認・検証のどれかに失敗したら操作しない | requirements 15.1 |

## 5. インターフェース

| Method | Endpoint | 認証区分 | 内容 | 出典 |
|---|---|---|---|---|
| GET | /api/approvals | 本人用 | 承認の一覧。`?status=` で承認待ち・履歴を絞り込む | 18章 |
| GET | /api/approvals/{id} | 本人用 | 承認の詳細（対象、content_version、Gate の結果、差分の成果物への参照） | 追加案 |
| POST | /api/approvals/{id}/decision | 本人用 | 承認・却下。入力は `decision`（`approve` / `reject`）、`contentVersion`、`ttlSec`（任意、Q55） | 18章 |
| POST | /api/approvals/{id}/renew | 本人用 | 期限切れの承認から新しい承認待ちを作る | 追加案 |
| POST | /api/approvals/{id}/consume | Runner 用 | 操作の直前の照合と消費（3.5.2） | 追加案 |
| POST | /api/executions/{id}/publish-check | Runner 用 | push 直前の確認（中止要求、承認ポリシーの現在値）。11 の 3.1 で heartbeat の応答を使っていた確認を置き換える案 | 追加案 |
| POST | /api/runners/{id}/claim | Runner 用 | 応答に「承認済みの操作」の仕事を足す | 18章（拡張） |

`POST /api/approvals/{id}/decision` の応答:

| HTTP | code | 場面 |
|---|---|---|
| 200 | - | 判断を記録した |
| 409 | `conflict` | すでに判断済み、無効、期限切れ |
| 422 | `precondition_failed` | `contentVersion` が行の content_version と一致しない |

`POST /api/approvals/{id}/consume` の応答（案）:

```json
{ "allowed": true, "contentVersion": "<40桁>", "mergeMethod": "merge" }
// または
{ "allowed": false, "reason": "content_changed" }
```

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| 承認の送信で content_version が違う | 422。画面を読み直させる |
| 消費の時点で head SHA・対象が違う | 操作しない。承認を invalidated にし、本人に通知する |
| 消費の更新が0件（同時に2回呼ばれた等） | 操作しない |
| マージ API が SHA の不一致で拒否 | 操作しない。承認は消費済みのため invalidated（`content_changed`）にし、本人に通知する |
| マージの結果が分からない | 3.5.3 |
| 直接 push で fast-forward できない | 3.7.3 |
| push 直前に承認ポリシーを読めない | push しない（Fail Closed） |
| Runner トークンで decision を呼んだ | 403。audit_logs に記録する |
| 本人のセッションで consume を呼んだ | 403。audit_logs に記録する |

## 7. スコープ外

- PR マージ以外の承認対象操作の実行（Q53 案。Phase 2 でも権限で拒否する）
- Context の更新候補の承認（requirements 7.4）
- 評価結果に基づく承認（Phase 3 以降）
- Linear・Slack・スマートフォンからの承認（requirements 15.5。承認は管理画面で本人だけが行う）
- マージキューの利用（requirements 15.8 で組み合わせの例に挙がっている。Phase 2 ではルールセットで守る案）
- 複数の承認者・利用者管理（requirements 15.5）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) 認証区分と Control Plane の権限
- [`02-data-model.md`](./02-data-model.md) 状態の値
- [`03-registry.md`](./03-registry.md) 承認ポリシーの保存
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 状態遷移、claim、Blocked からの続行（SHA に結び付ける考え方）
- [`05-runner.md`](./05-runner.md) Runner が行わない操作
- [`09-sandbox.md`](./09-sandbox.md) 承認対象操作の拒否
- [`10-verification-gates.md`](./10-verification-gates.md) Gate 2・3
- [`11-git-publisher.md`](./11-git-publisher.md) push・PR 作成と GitHub の権限
- [`12-notification.md`](./12-notification.md) 通知
- [`13-management-ui.md`](./13-management-ui.md) 管理画面
- [`15-linear-intake.md`](./15-linear-intake.md) Linear 起因の中止
- [`16-linear-feedback.md`](./16-linear-feedback.md) 必要な承認事項の返却

## 9. 実装単位

- [ ] （人間） Q54・Q63 の GitHub のルールセットの変更を、対象リポジトリで行う（Bypass に evaloom-publisher を足す。direct_push の Project ではルールセット C を分ける）
- [ ] [Backend] approvals のマイグレーションと、Execution の状態 `waiting_approval` / `merging` の追加、遷移表の更新
  - 受け入れ基準: 同じ Execution・同じ operation で pending / approved の行を2つ作れない; waiting_approval と merging の Execution が claim の同時実行の数に入らない; 遷移表に無い遷移が 409 になる
- [ ] [Backend] 承認ポリシーの形の検証とマイグレーション、変更の監査記録
  - 受け入れ基準: `mergePullRequest` 以外の項目や未知の値が 400; 既存の Project の値が require_approval になる; 変更が audit_logs に残る; Runner トークンでは変更できない
- [ ] [Backend] PR 作成後の承認待ちの発生（3.3）と、新しい commit による前の承認の無効化
  - 受け入れ基準: require_approval の Project で PR を作るとタスクが waiting_approval になり approvals に pending の行ができる; 同じ PR に continue で commit を足すと前の承認が invalidated（superseded）になり新しい pending ができる; 承認待ちの通知が1回出る
- [ ] [Backend] 承認・却下・再承認の本人用 API（decision、renew、一覧、詳細）
  - 受け入れ基準: contentVersion が違うと 422; 判断済みの承認への decision が 409; 却下でタスクが Q57 の状態になる; 期限切れの承認から renew で新しい pending ができる; 判断が audit_logs に残る
- [ ] [Backend] 消費 API（consume）と照合の表（3.5.2）
  - 受け入れ基準: 表の各行に対応するテストで allowed と approvals の status が期待どおりになる; 同じ承認に consume を並行に2回呼ぶと allowed=true が1回だけ; 承認後に head SHA を変えると allowed=false（content_changed）になる（条件22）
- [ ] [Backend] claim の応答への「承認済みの操作」の追加と、Git Publisher のマージ（sha パラメータ付き）
  - 受け入れ基準: テスト用の GitHub API に、マージ要求が content_version を sha に入れて1回だけ送られる; consume が allowed=false のときマージ要求が送られない; マージ成功でタスクが completed になる
- [ ] [Backend] マージの結果が分からないときの照合（3.5.3）
  - 受け入れ基準: マージ要求の応答を切るテストで、再びマージを要求せず PR を読み直す; マージ済みなら成功、開いたままなら publish_failed（merge_unknown）になる
- [ ] [Backend] 承認待ちの PR の定期照合（GitHub で直接マージ・クローズされた場合、3.6）
  - 受け入れ基準: 承認待ちの PR を GitHub 側でマージすると承認が invalidated になりタスクが completed になる; クローズするとタスクが Q58 の状態になる
- [ ] [Backend] push 直前の確認 API（publish-check）と、直接 push（3.7）
  - 受け入れ基準: direct_push の Project で Gate を通った commit がベースブランチへ push され PR が作られない; push 後のリモートのベースブランチの SHA が verified_sha と一致する; push の直前に承認ポリシーを require_approval に変えると PR を作る経路になる; Control Plane に届かないと push しない; Base Branch 以外で `evaloom/` で始まらない ref には push しない
- [ ] [Backend] 直接 push で fast-forward できないときの切り替え（3.7.3）
  - 受け入れ基準: テスト用のリモートでベースブランチを先に進めておくと、強制 push せず PR を作る経路になる
- [ ] [Backend] 承認の受け入れテスト（条件21, 22）を 14 のテスト群に足す
  - 受け入れ基準: PR マージを承認対象にした Project で、承認なしにマージされない; 承認後に head SHA を変えるとマージされない; 使用済みの承認で2回目のマージが行われない; 有効期限切れの承認でマージされない
- [ ] [Frontend] Approvals 画面（承認待ち一覧、詳細、承認・却下、履歴）と Dashboard の承認待ち
  - 受け入れ基準: 承認待ちの対象（PR の URL、head SHA、Gate の結果、差分）が表示される; 送信に表示中の contentVersion が含まれる; 422 で読み直しを促す; 承認待ちの件数が Dashboard に出る
- [ ] [Frontend] Project 編集の承認ポリシーの選択と、direct_push にするときの確認ダイアログ
  - 受け入れ基準: require_approval / direct_push を選べる; direct_push を選ぶと、PR のレビューに任せていた範囲を確認する人がいなくなることを示すダイアログが出て、確認しないと保存されない
