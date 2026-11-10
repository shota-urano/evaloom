---
status: confirmed
confirmed_rev: 9c588f1
written: 2026-11-10
範囲: 承認の対象（スペシャルハーネスが宣言する操作）、承認を省く設定（Project と、直接振り分けたときのスペシャルハーネスの approval_waivers）、承認待ちの発生、承認と中身の識別子（content_id）の結び付け、出す直前の照合と消費（authorize）、却下・無効・期限切れ・中止
---

# 17. 承認 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend（承認の画面は Frontend） ｜ **範囲**: requirements 8.2 の工程14（必要に応じた承認）、15.3、15.8、16.9

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。承認の対象はスペシャルハーネスが Profile で宣言する操作にし、承認は PR の head SHA ではなく、スペシャルハーネスが報告する中身の識別子（content_id）に結び付ける（本人決定 5）。承認は管理画面だけで受け付ける（本人決定 3）。GitHub のマージ、ベースブランチへの直接 push、GitHub のルールセットとマージ方式は開発の仕事の中身であり、software-engineer へ移した（本人決定 1、3.9）。スペシャルハーネスとの約束はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）7章、テーブルと列挙値は [`02-data-model.md`](./02-data-model.md) 3.4、API は [`01-architecture.md`](./01-architecture.md) に従う。

## 1. 目的

- 外へ出す操作のうち、スペシャルハーネスが本人の承認が要ると宣言したものを、本人の承認の対象にする（条件21、requirements 15.3、vision.md 3章）。
- 承認を、操作の名前と中身の識別子に結び付ける。承認した後に中身が変わったら、その承認では操作をさせない（条件22、requirements 15.8、本人決定 5）。
- 承認は本人だけが、管理画面で、操作の直前に行う。事後承認にしない（requirements 15.3, 15.5、本人決定 3）。
- 承認の照合と消費を1つの書き込みで行い、同じ承認で2回操作させない（requirements 15.8）。
- Project ごとに、スペシャルハーネスが省いてよいと宣言した承認だけを省けるようにする（本人決定 6）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | check / publish の段の結果（契約 6.5 の `needs_approval` と `approvalRequests`）、本人の判断（`POST /api/decisions/{id}/answer`）、publish の段の出す直前の問い合わせ（`POST /api/executions/{id}/authorize`、契約 7.4）、Project の承認を省く設定（projects.approval_waivers）、Profile の承認の宣言（契約 5章 `approvals`） |
| 出力 | approvals、owner_decisions（kind=`approval`）、tasks / executions の状態、execution_events（`approval_requested` / `approval_waived` / `approval_consumed`）、audit_logs、通知（[`12-notification.md`](./12-notification.md)、[`16-linear-feedback.md`](./16-linear-feedback.md)） |

## 3. 処理詳細

### 3.1 承認の対象（requirements 15.3）

- 承認の対象になる操作は、スペシャルハーネスが Profile の `approvals` に宣言した操作だけである（契約 5章、7.1）。操作の名前（operation）は英小文字・数字・`_` で 64 文字以内で、Profile の中で一意である。Evaloom は操作の名前の一覧を持たない。
- 例えば software-engineer は、PR のマージ（`merge_pull_request`）を承認の要る操作として宣言する。X の運用のスペシャルハーネスなら、投稿の公開を宣言する、という形になる。何を宣言するかはスペシャルハーネスが決める（vision.md 3章）。
- 宣言していない操作の承認を求めた結果は、契約に合わない結果として Execution を失敗（`contract_error`）にする（契約 7.1）。
- requirements 15.3 の承認対象操作（本番環境へのデプロイ、外部メール送信、重要ファイルの削除など）を行うスペシャルハーネスは、その操作を `approvals` に宣言する。宣言していない外への操作を行ってよいかは、スペシャルハーネスの責任であり、Evaloom は出す段の宣言した宛先と鍵の範囲（[`01-architecture.md`](./01-architecture.md) 3.1）で外への到達を絞る。
- 認証・権限の変更、ハーネス契約・評価基準・セキュリティポリシーの変更のうち、本人が管理画面で行う設定の変更は、承認の仕組みを通さない（requirements 15.3）。

### 3.2 承認を省く設定（approval_waivers）

旧仕様の承認ポリシー（projects.approval_policy、Q60）を、Project の承認を省く設定（projects.approval_waivers、[`02-data-model.md`](./02-data-model.md) 3.2）に置き換えた。

- 値は、承認を省く操作の名前の配列である。既定は `[]`（すべての承認を求める）。
- 入れられるのは、Project が固定した版（projects.profile_id）の Profile で `waivable: true` と宣言された操作だけである（契約 5.2、7.3）。それ以外の名前は 422（`not_waivable`）で拒否する。
- Project の固定する版を別の版に変えるとき、新しい版で `waivable: true` でない操作が承認を省く設定に残っていれば、その変更を 422 で拒否する。
- スペシャルハーネスへ直接振り分けた仕事では、スペシャルハーネスの承認を省く設定（harnesses.approval_waivers）を使う。既定は `[]` で、宣言したすべての承認を求める。本人が、もう止めなくてよい操作を後から足していく。入れられる操作と拒否の規則は Project と同じで、版は harnesses.default_profile_id と読む（Q113）。変更は `PATCH /api/harnesses/{id}` から行い、audit_logs（`harness.update`）に残す。
- 承認を省く設定を変えられるのは本人だけで、管理画面の `PATCH /api/projects/{id}` から行う。Runner 用 API とスペシャルハーネスには変更の経路を作らない（requirements 15.3）。変更は audit_logs（`project.update`）に変更前後の値を残す。
- 承認を省く操作を足すときは、管理画面で確認ダイアログを出す（Q165）。
- 省いた操作は承認待ちを作らず、execution_events に `approval_waived` を残す。出す直前の問い合わせ（3.5）の時点で振り分け先の承認を省く設定を読み直し、省く設定が外れていれば許可しない（2026-11-06 決定の Q61 と同じ考え方）。

### 3.3 承認待ちの発生

check または publish の段が `needs_approval` を返したとき（check が `succeeded` で `approvalRequests` を書いたときを含む、契約 6.5）、Control Plane は次を1つのトランザクションで行う。

1. `approvalRequests` の各要求の operation が、Execution に固定した Profile の `approvals` に宣言されていることを確かめる。1つでも宣言していなければ、Execution を失敗（`contract_error`）にして終える。
2. 承認を省く操作（振り分け先の approval_waivers にある操作。Project なら projects、直接振り分けたタスクなら harnesses）は、承認待ちを作らず、execution_events に `approval_waived`（operation、contentId）を書く。
3. それ以外の操作ごとに、同じ Execution・同じ operation の未使用の承認（status が `pending` / `approved`）があれば `invalidated`（理由 `superseded`）にし、その判断待ちを `withdrawn`（理由 `superseded`）にする（契約 7.2）。
4. 操作ごとに approvals に `pending` の行を作る。結び付ける値は 3.4 の表のとおり。
5. 操作ごとに owner_decisions に kind=`approval` の行を作る（subject_id=approvals.id、basis=content_id）。
6. 承認待ちが1つ以上あれば、Execution を `waiting_approval`、タスクを `waiting_approval` にする。execution_events に `approval_requested` を書く。
7. 本人に知らせる。macOS 通知と、Linear 起因のタスクなら「管理画面で判断待ち」のコメントと状態の変更（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.2、3.6.1）を linear_outbox に書く。
8. すべての操作を省いたときは、承認待ちを作らない。check の結果なら publish の段へ進み、publish の結果なら publish の段を新しく始める（Execution を `pending`、resume_stage=`publish`）。

- `waiting_approval` の Execution は同時実行の枠に数えない（Q59）。段のプロセスは終了している（契約 7.1）。
- check の段が `blocked` を返し、結果に `approvalRequests` があるときは、本人が続行を選んだ時点で、この手順で承認待ちを作る（[`02-data-model.md`](./02-data-model.md) 5.6）。
- Evaloom は、承認の対象の中身（PR、下書き、投稿の本文など）を読まず、外の状態も照合しない。本人に見せるのは、スペシャルハーネスが報告した `target`、`summary`、content_id と、確かめる段の結果と成果物である。

### 3.4 承認と中身の結び付け（requirements 15.8、本人決定 5）

| 項目 | approvals の列 | 値 |
|---|---|---|
| 操作の名前 | operation | Profile の `approvals[].operation` |
| 対象の表示 | target | 結果 JSON の `target`（`label`、`url`）。表示用の不透明な値 |
| 説明 | summary | 結果 JSON の `summary` |
| 中身の識別子 | content_id | 承認を求めたときにスペシャルハーネスが報告した `contentId`（契約 6.5、Q123）。開発なら commit SHA など |
| 承認を求めた段 | requested_stage | `check` / `publish` |
| 操作の内容のハッシュ | params_hash | operation・target・content_id を正規化した JSON の SHA-256 |
| 使用回数の上限 | max_uses | 1 |
| 有効期限 | expires_at | 承認した時刻 + 回答の `ttlSec`。省略したら全体設定 `approval.ttlSec`（既定 24 時間、Q55、02 の Q200） |

- 本人は管理画面の判断待ちの画面（[`13-management-ui.md`](./13-management-ui.md) 3.4）で、操作の名前と Profile の description、対象の表示、説明、content_id、確かめる段の結果を見て、承認か却下を選ぶ。
- 回答は `{ "action": "approve" | "reject", "contentId": "<basis>" }` である（[`02-data-model.md`](./02-data-model.md) 5.6）。送られた contentId が判断待ちの basis と違えば 422 を返す（Blocked からの続行と同じ考え方）。
- 承認者は本人だけである（requirements 15.5）。判断は audit_logs（actor `owner`、action `decision.answer`）に残す。
- 同じ Execution のすべての承認待ちが承認されたら、Execution を `pending`（resume_stage=`publish`）、タスクを `queued` にする。publish の段の要求 JSON の `approvals` に、承認した操作（`status: "approved"`、`id`、`contentId`）と省いた操作（`status: "waived"`、`id: null`）を入れる（契約 6.1、7.1）。
- 一部だけ承認されたときは、残りの判断を待つ。Execution は `waiting_approval` のままである。
- Evaloom が比べるのは、承認したときの content_id と、出す直前にスペシャルハーネスが送る content_id だけである（3.5）。確かめる段で確かめた中身と出す中身が同じであることは、スペシャルハーネスが見張る（本人決定 5、契約 12章の 3）。

### 3.5 出す直前の照合と消費（authorize）

publish の段は、外へ出す操作を1つ行うたびに、その直前に通話口へ問い合わせる（契約 7.4）。Runner は問い合わせを `POST /api/executions/{id}/authorize` に中継する（[`01-architecture.md`](./01-architecture.md) 5.2）。この API は、旧仕様の `POST /api/approvals/{id}/consume` と `POST /api/executions/{id}/publish-check` を置き換える。

Control Plane は、次の判定と書き込みを1つの書き込みトランザクションの中で行い、`allowed` と `reason` を返す（Fail Closed）。上から順に判定し、最初に当てはまった行で答える。

| 状況 | 応答 | approvals の status |
|---|---|---|
| タスクの中止要求がある | `allowed: false`、`cancelled` | 未使用の承認を `invalidated`（理由 `cancelled`） |
| 要求の Runner が claim した Runner でない、または Execution が `publishing` でない | 403 / 409（[`01-architecture.md`](./01-architecture.md) 5.3）。Runner は段に `allowed: false`（`unavailable`）を返す | 変えない |
| operation が `publish`（承認の要らない操作） | `allowed: true` | 行は無い |
| operation が Profile の `approvals` に無い（`publish` 以外） | `allowed: false`、`not_approved` | 行は無い |
| approvalId が null で、operation が振り分け先（Project かスペシャルハーネス）の現在の approval_waivers にある（3.2） | `allowed: true` | 行は無い。execution_events に `approval_consumed`（approvalId null）を書く |
| approvalId が null で、上に当てはまらない（省く設定が外された場合を含む） | `allowed: false`、`not_approved` | 行は無い |
| approvalId の行が、この Execution・この operation のものでない、または status が `approved` でない（`pending` / `rejected` / `invalidated`） | `allowed: false`、`not_approved` | 変えない |
| 行の status が `consumed`、または use_count が max_uses に達している | `allowed: false`、`already_used` | 変えない |
| expires_at を過ぎている | `allowed: false`、`expired` | `expired` |
| 送られた contentId が行の content_id と違う | `allowed: false`、`content_changed` | `invalidated`（理由 `content_changed`） |
| 上のどれにも当てはまらない | `allowed: true` | `consumed`（use_count=1、consumed_at） |

- 消費は `UPDATE approvals SET status='consumed', use_count=use_count+1, consumed_at=? WHERE id=? AND execution_id=? AND operation=? AND status='approved' AND use_count < max_uses AND expires_at > ? AND content_id=?` のように、期待する現在値を条件にした1つの更新で行う。更新できた行が0件なら許可しない（requirements 15.8「承認の消費は DB で原子的に管理する」）。
- 判定の結果は execution_events に `approval_consumed`（approvalId、operation、allowed、reason）として残し、audit_logs（actor `runner:<id>`、action `approval.consume`）にも残す。
- Control Plane の DB の書き込みに失敗したら `allowed: false` を返す。Runner が Control Plane に届かないときは、Runner が段に `allowed: false`（`unavailable`）を返す（[`01-architecture.md`](./01-architecture.md) 3.7）。
- スペシャルハーネスは `allowed: true` を受け取ったときだけ、その操作を1回行う。許可されなかったときは操作せず、`publish_failed` を返す（契約 7.4）。この Execution は失敗になる。
- 承認の要る操作を `publish` と名乗って問い合わせることを Evaloom は見分けられない。宣言した操作は必ずその名前で問い合わせることは、スペシャルハーネスの義務である（契約 12章の 4）。

### 3.6 却下・無効・期限切れ・中止

| 契機 | approvals | 判断待ち | タスク・Execution |
|---|---|---|---|
| 本人が却下 | `rejected` | `answered` | Execution を `failed`（`approval_rejected`）、タスクを `failed` にする（Q57）。同じ Execution のほかの未使用の承認は `invalidated`（理由 `cancelled`）にし、その判断待ちを `withdrawn`（理由 `cancelled`）にする。外に出したもの（開いた PR など）は、スペシャルハーネスが出したまま残る。本人は `continue` の再実行で直せる |
| 同じ Execution・同じ operation の承認を新しく求めた | 前の行を `invalidated`（`superseded`） | 前の行を `withdrawn`（`superseded`） | 3.3 で新しい承認待ちを作る |
| 出す直前の問い合わせで content_id が違った | `invalidated`（`content_changed`） | 変えない（判断済み） | publish の段が `publish_failed` を返し、Execution は `failed`。本人に通知する |
| 判断待ちのまま時間がたった | 変えない | 変えない | 有効期限は承認した時刻から数えるため、判断待ちのままでは期限切れにならない |
| 承認した後、出す直前の問い合わせまでに期限が切れた | `expired` | 変えない（判断済み） | Q166 |
| 本人が期限切れの承認から承認待ちを作り直す（`POST /api/approvals/{id}/renew`） | 新しい `pending` の行（同じ operation・target・content_id） | 新しい kind=`approval` の行 | Execution とタスクを `waiting_approval` にする |
| タスクの中止（本人、または Linear 起因、[`15-linear-intake.md`](./15-linear-intake.md) 3.7） | 未使用なら `invalidated`（`cancelled`） | 開いていれば `withdrawn`（`cancelled`） | `cancelled`。外に出したものを Evaloom は取り消さない |
| 承認待ちの間に、外の対象が本人の手で変わった（GitHub で直接マージした、など） | 変えない | 変えない | Evaloom は外を照合しない。Q167 |

- 承認の判断、消費、無効化は、すべて audit_logs に残す（requirements 15.2 監査ログ）。actor は判断と作り直しが `owner`、消費が `runner:<id>`、無効化と期限切れが `system`。

### 3.7 出す操作の結果がわからないとき

出す操作を送った後に結果がわからなくなったときの扱いは、契約 7.5 に従う。

- スペシャルハーネスは、同じ承認で同じ操作をもう一度送らない。承認は問い合わせの時点で使用済みになっている。
- 外の状態を読み直せないとき、publish の段は `unknown`（`publish_unknown`）を返し、Execution は `blocked`（cause=`publish_unknown`）になる（[`02-data-model.md`](./02-data-model.md) 5.6）。本人が続行を選ぶと publish の段を新しく始める。スペシャルハーネスは始めに外の状態を確かめ、まだ行われていない承認の要る操作があれば、新しい `contentId` で承認を求め直す（`needs_approval`）。

### 3.8 データモデル

approvals の列は [`02-data-model.md`](./02-data-model.md) 3.4 の approvals を正本とする。この文書で使う点は次のとおり。

- status は `pending` / `approved` / `rejected` / `consumed` / `invalidated` / `expired`。invalidated_reason は `content_changed` / `superseded` / `cancelled`。
- 同じ execution_id と operation で、status が `pending` / `approved` の行は1件までとする（部分一意インデックス）。
- 承認を省いた操作は行を作らない（3.2）。
- 旧仕様の content_version は content_id に、decided_by は audit_logs の actor に、result は execution_events と executions.outputs に置き換えた。Execution 状態の `merging` は持たない。承認の後は Execution を `pending`（resume_stage=`publish`）に戻し、publish の段で操作する（[`02-data-model.md`](./02-data-model.md) 5.2）。

### 3.9 software-engineer へ移したもの

次は開発の仕事の中身であり、software-engineer の文書（`~/workspace/harnesses/software-engineer/docs/design.md`）へ移した（本人決定 1）。Evaloom の仕様からは消した。

| 旧仕様の内容 | 旧 Q | 移した後の扱い |
|---|---|---|
| PR マージを承認の対象にすること、マージの実行（GitHub のマージ API の `sha` パラメータ） | Q53、Q54 | software-engineer が `merge_pull_request` を `approvals` に宣言し、publish の段で `authorize` の後にマージする |
| PR のマージ方式 | Q56 | software-engineer の設定 |
| 承認待ちの間に GitHub 上で直接マージ・クローズされた場合 | Q58 | software-engineer の publish の段が始めに外の状態を確かめる（契約 7.5、12章の 9）。Evaloom 側は Q167 |
| 承認ポリシーの `direct_push` と、ベースブランチへの直接 push、fast-forward できないときの切り替え | Q35、Q60〜Q62 | software-engineer の Profile が、例えば直接 push の操作を `waivable: true` で宣言し、本人が Project の承認を省く設定で選ぶ形にする。push の手順は software-engineer が決める |
| GitHub のルールセットと Bypass の設定 | Q63 | software-engineer の導入の手順 |
| マージの結果がわからないときの PR の読み直し | - | 契約 7.5 に従い software-engineer が行う |

Q55（有効期限の既定 24 時間）、Q57（却下で failed）、Q59（承認待ちを同時実行の枠に数えない）は Evaloom に残す。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 承認の対象 | スペシャルハーネスが Profile の `approvals` に宣言した操作だけ | vision.md 3章、契約 7.1 |
| 承認の場所 | 管理画面だけ。Linear・Slack・スマートフォンの通知からの操作を承認として扱わない | 本人決定 3、requirements 15.5 |
| 承認の時点 | 操作の直前。事後承認にしない | requirements 15.3 |
| 権限と承認状態の確認 | 段の開始時と、外へ出す操作の直前（authorize） | requirements 15.3 |
| 承認者 | 本人だけ | requirements 15.5 |
| 承認と中身の結び付け | 承認を求めたときの content_id と、出す直前の content_id を Evaloom が完全一致で比べる。違えば承認は無効 | 本人決定 5、requirements 15.8、条件22 |
| 確かめたもの＝出すもの | スペシャルハーネスが見張る。Evaloom は照合しない | 本人決定 5 |
| 承認の使用回数 | 1回 | requirements 15.8 |
| 使用済み | 許可しない | requirements 15.8 |
| 有効期限 | 承認した時刻から全体設定 `approval.ttlSec`（既定 24 時間）。切れたら許可せず、再承認を求める | requirements 15.8、Q55 |
| 承認の消費 | DB の1つの更新で原子的に行う | requirements 15.8 |
| 承認を省ける操作 | Project が固定した版で `waivable: true` と宣言された操作だけ。既定は省かない | 本人決定 6、契約 7.3 |
| 承認を省く設定の変更 | 本人だけ。Runner とスペシャルハーネスには許可しない。出す直前に読み直す | requirements 15.3、Q61 |
| 承認待ちと同時実行 | 同時実行の枠に数えない | Q59 |
| 却下 | Execution とタスクを failed（`approval_rejected`） | Q57 |
| Fail Closed | 認証・承認・照合のどれかに失敗したら許可しない | requirements 15.1 |

## 5. インターフェース

| Method | Endpoint | 認証区分 | 内容 |
|---|---|---|---|
| GET | /api/decisions | 本人用 | 判断待ちの一覧。`?kind=approval` で承認待ちに絞る |
| GET | /api/decisions/{id} | 本人用 | 承認待ちの詳細（操作、Profile の description、対象の表示、説明、content_id、確かめた結果、成果物への参照） |
| POST | /api/decisions/{id}/answer | 本人用 | 承認・却下。入力は `{ "action": "approve" \| "reject", "contentId": "<basis>", "ttlSec": <整数> }`。`ttlSec` は approve のときだけ書ける任意の項目（[`02-data-model.md`](./02-data-model.md) 5.6、Q200） |
| GET | /api/approvals | 本人用 | 承認の履歴。`?status=` で絞り込む |
| GET | /api/approvals/{id} | 本人用 | 承認の詳細（使用・無効の記録を含む） |
| POST | /api/approvals/{id}/renew | 本人用 | 期限切れの承認から新しい承認待ちを作る |
| PATCH | /api/projects/{id} | 本人用 | `approvalWaivers` の変更（3.2） |
| PATCH | /api/harnesses/{id} | 本人用 | 直接振り分けたタスク用の `approvalWaivers` の変更（3.2、Q113） |
| POST | /api/executions/{id}/authorize | Runner 用 | 出す直前の照合と消費（3.5、契約 7.4） |

`POST /api/decisions/{id}/answer`（kind=`approval`）の応答は次のとおりとする。

| HTTP | code | 場面 |
|---|---|---|
| 200 | - | 判断を記録した |
| 400 | `validation_failed` | 回答の形が違う |
| 409 | `conflict` | すでに判断済み、取り下げ済み |
| 422 | `precondition_failed` | `contentId` が判断待ちの basis と一致しない |

`POST /api/executions/{id}/authorize` の要求と応答は次のとおりとする。形は契約 7.4 の通話口と同じで、Runner は段から受け取った要求を中継する。

```json
{ "operation": "merge_pull_request", "approvalId": "01J...", "contentId": "<識別子>" }
```

```json
{ "allowed": true }
{ "allowed": false, "reason": "content_changed" }
```

`reason` は `cancelled` / `content_changed` / `not_approved` / `already_used` / `expired`（契約 7.4）。`unavailable` は Runner が Control Plane に届かないときに Runner が返す値で、この API は返さない。

使わなくなったエンドポイント: `POST /api/approvals/{id}/decision`（`POST /api/decisions/{id}/answer` に置き換えた）、`POST /api/approvals/{id}/consume` と `POST /api/executions/{id}/publish-check`（`POST /api/executions/{id}/authorize` に置き換えた）。claim の応答に「承認済みの操作」の仕事を足す案は取りやめた（承認の後は同じ Execution の publish の段として再開するため）。

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| 承認の回答で contentId が違う | 422。画面を読み直させる |
| 判断済み・取り下げ済みの判断待ちへの回答 | 409 |
| 宣言していない操作の承認を求めた | Execution を失敗（`contract_error`）にする（3.3） |
| 省けない操作を承認を省く設定に入れた | 422（`not_waivable`） |
| 出す直前の問い合わせで content_id が違う | 許可しない。承認を invalidated（`content_changed`）にし、本人に通知する |
| 消費の更新が0件（同時に2回呼ばれた等） | 許可しない |
| 出す直前の問い合わせで DB に書き込めない | 許可しない（Fail Closed） |
| Runner が Control Plane に届かない | Runner が段に `allowed: false`（`unavailable`）を返す |
| Runner トークンで判断の回答を呼んだ | 403。audit_logs に記録する |
| 本人のセッションで authorize を呼んだ | 403。audit_logs に記録する |

## 7. スコープ外

- GitHub のマージ、ベースブランチへの直接 push、GitHub のルールセット、マージ方式、マージキュー（software-engineer へ移した、3.9）
- Linear・Slack・スマートフォンの通知からの承認（本人決定 3、requirements 15.5。承認は管理画面で本人だけが行う）
- 承認の対象の中身や外の状態を Evaloom が読んで照合すること（本人決定 5）
- Context の更新候補の承認（requirements 7.4）
- 評価結果に基づく承認（評価と改善の Phase）
- 複数の承認者・利用者管理（requirements 15.5）

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約 7章（承認、通話口）
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) 認証区分、authorize、Fail Closed
- [`02-data-model.md`](./02-data-model.md) approvals、owner_decisions、状態の値
- [`03-registry.md`](./03-registry.md) Project の承認を省く設定の保存と検証
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 状態遷移、本人の判断の後の再開
- [`05-runner.md`](./05-runner.md) 通話口の中継
- [`12-notification.md`](./12-notification.md) 通知
- [`13-management-ui.md`](./13-management-ui.md) 本人の判断待ちの画面
- [`14-security-tests.md`](./14-security-tests.md) 承認の受け入れテスト
- [`15-linear-intake.md`](./15-linear-intake.md) Linear 起因の中止
- [`16-linear-feedback.md`](./16-linear-feedback.md) 「管理画面で判断待ち」の知らせ

## 9. 実装単位

- [ ] [Backend] 承認待ちの発生（3.3）
  - 受け入れ基準: check が needs_approval を返すと、省かない操作ごとに approvals の pending の行と kind=approval の判断待ちができ、Execution とタスクが waiting_approval になる; check が succeeded で approvalRequests を書いたときも同じになる; 宣言していない操作の承認要求で Execution が contract_error になる; 省く操作では行ができず approval_waived が記録される; すべて省いたとき check の後に publishing へ進む; 同じ Execution・同じ operation で新しく求めると前の承認が invalidated（superseded）になる; waiting_approval の Execution が claim の同時実行の数に入らない; 承認待ちの通知が1回出て、Linear 起因のタスクには decision_open の行ができる
- [ ] [Backend] 承認・却下・作り直しの本人用 API（decisions の answer、approvals の一覧・詳細・renew）
  - 受け入れ基準: contentId が違うと 422; 判断済みの判断待ちへの回答が 409; すべて承認すると Execution が pending（resume_stage=publish）になる; 一部だけ承認すると waiting_approval のまま; 却下でタスクと Execution が failed（approval_rejected）になり、ほかの未使用の承認が invalidated（cancelled）になる; 期限切れの承認から renew で新しい pending と判断待ちができる; 判断が audit_logs に残る; Runner トークンで回答すると 403
- [ ] [Backend] 承認を省く設定の検証と変更の監査記録（3.2）
  - 受け入れ基準: waivable でない操作、Profile に無い操作を approvalWaivers に入れると 422; 版を変えて waivable でない操作が残ると 422; 変更が audit_logs に変更前後の値で残る; Runner トークンでは変更できない; スペシャルハーネスへ直接振り分けたタスクでは、スペシャルハーネスの approvalWaivers が `[]` なら宣言したすべての操作で承認待ちができ、足した操作だけ承認待ちを作らない; スペシャルハーネスの approvalWaivers の検証と audit_logs も Project と同じ
- [ ] [Backend] 出す直前の照合と消費（`POST /api/executions/{id}/authorize`、3.5）
  - 受け入れ基準: 3.5 の表の各行に対応するテストで allowed・reason と approvals の status が期待どおりになる; 同じ承認に並行に2回問い合わせると allowed=true が1回だけ; 承認の後に contentId を変えて問い合わせると allowed=false（content_changed）で承認が invalidated になる（条件22）; 承認を省いた後に Project の省く設定から外すと、approvalId=null の問い合わせが not_approved になる; operation=publish は中止要求が無ければ許可される; 中止要求があると cancelled; 有効期限を過ぎると expired; DB の書き込みを失敗させると allowed=false; 結果が execution_events と audit_logs に残る
- [ ] [Backend] 承認の受け入れテスト（条件21、22）を 14 のテスト群に足す
  - 受け入れ基準: 承認の要る操作を宣言したテスト用のスペシャルハーネスで、承認なしに authorize が許可されない; 承認の後に contentId を変えると許可されない; 使用済みの承認で2回目が許可されない; 有効期限切れの承認で許可されない; 管理画面以外の経路（Linear のコメント、Runner 用 API）で承認できない
- [ ] [Frontend] 判断待ちの画面の承認の表示と操作（[`13-management-ui.md`](./13-management-ui.md) 3.4）と Dashboard の承認待ちの件数
  - 受け入れ基準: 承認待ちの対象（操作、description、target の label と url、summary、content_id、確かめた結果）が表示される; 送信に表示中の contentId が含まれる; 422 で読み直しを促す; 承認待ちの件数が Dashboard に出る
- [ ] [Frontend] Project 編集の承認を省く設定と確認ダイアログ（3.2、Q165）
  - 受け入れ基準: 固定した版で waivable の操作だけが選べる; 操作を足すと確認ダイアログが出て、確認しないと保存されない; 外すときはダイアログが出ない

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q165 | 承認を省く操作を足すとき、管理画面で何を確かめさせるか（旧仕様の `direct_push` の確認ダイアログを一般化） | 決定（2026-11-10 本人、案を採用）: 確認ダイアログを出し、省く操作の名前と Profile の description と、「この Project では、この操作を本人の承認なしにスペシャルハーネスが行う」ことを示す。パスワードの入れ直しは求めない（18 の 3.7 の決定どおり） |
| Q166 | 承認してから publish の段が問い合わせるまでに有効期限が切れたとき（Runner が止まっていて、承認から 24 時間たって claim された、など） | 決定（2026-11-10 本人、案を採用）: claim の時点で Execution の承認の有効期限を確かめ、切れた承認があれば publish の段を始めない。その承認を `expired` にし、Execution とタスクを `waiting_approval` に戻して、期限切れを示す判断待ちを作る（本人は renew で承認し直す）。段が始まった後の問い合わせで切れていたときは、契約どおり `expired` で許可せず、Execution は失敗になる |
| Q167 | 承認待ちの間に、外の対象が本人の手で変わったとき（PR を GitHub で直接マージした、投稿を手で公開した、など）、Evaloom は気付くべきか | 決定（2026-11-10 本人、案を採用）: Evaloom は外を読まず、何もしない（本人決定 5、vision.md 4章）。本人は判断待ちの画面で却下するか、タスクを中止する。承認した場合は、publish の段が始めに外の状態を確かめて、すでに行われた操作を繰り返さない（契約 7.5、12章の 9） |
