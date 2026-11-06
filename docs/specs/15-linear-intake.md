---
status: confirmed
confirmed_rev: 57117d5
範囲: Linear Webhook の受信（公開 HTTPS 経路・署名検証・重複排除）、Team + Label ルーティング、Linear からの自動タスク作成、ai-ready の取り外しと Issue のキャンセルによる中止
---

# 15. Linear 連携：受信とタスク作成 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend（設定画面は Frontend） ｜ **範囲**: requirements 8.2 の工程1〜4（イベント受信、署名確認、重複排除、プロジェクト特定）を Linear について行い、タスクを作る。結果の返却は [`16-linear-feedback.md`](./16-linear-feedback.md)

この文書は Phase 2 の下書きである。requirements で確定していない点は「案」とし、00-overview.md の要確認リスト（Q42〜Q50）に載せている。

## 1. 目的

- Linear の Issue に `ai-ready` が付いた時点で、Evaloom が自動でタスクを作り、対象プロジェクトで実行できるようにする（requirements 6.2, 19章 Phase 2）。
- 実行先は Linear Team と Issue Label で決める。Linear Project を使わなくても自動実行できるようにする（条件19, 20、requirements 6.1）。
- Mac mini のポートを直接公開せず、署名を検証した Webhook だけを受け付ける（requirements 2.2, 6.6, 15.2）。
- 外部から来る Issue 本文を信頼済みの命令として扱わない（requirements 15.1）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | Linear Webhook（Issue の作成・更新・削除）、Linear GraphQL API から取り直した Issue の現在の内容 |
| 出力 | event_inbox、tasks（`source=linear`）、executions、linear_issue_links、audit_logs、[`16-linear-feedback.md`](./16-linear-feedback.md) へのコメント要求 |

テーブルの定義は本書 3.8 に書き、[`02-data-model.md`](./02-data-model.md) 3.3 から参照する。

## 3. 処理詳細

### 3.1 受信の経路（requirements 6.6、Q42）

requirements 21章で「Linear Webhook を受ける公開 HTTPS エンドポイントの構成」は詳細設計事項とされている。決まっているのは次の3点である。

- Mac mini のローカル HTTP ポートを直接公開しない。
- 公開 HTTPS エンドポイントを経由する。
- 署名を検証する。検証は Evaloom（Control Plane）自身が行い、経路の途中の仕組みに任せない（Fail Closed のため、途中の仕組みが検証を省いても Evaloom が拒否する）。

経路の候補（Q42）:

| 案 | 内容 | 利点 | 弱点 |
|---|---|---|---|
| A（推奨） | Cloudflare Tunnel 等の外向き接続のトンネル。Mac mini から外へ接続し、公開ホスト名への要求を Webhook 専用の待ち受け（3.2）にだけ転送する | 受信ポートを開けない。Evaloom の外に状態を持たない | Mac mini が止まっている間の Webhook は Linear の再送に頼る（3.7 の補完で補う） |
| B | Tailscale Funnel | A と同じく受信ポートを開けない。将来の遠隔アクセス（requirements 15.5）と同じ仕組みに寄せられる | 公開できるポートと転送先の制約を実機で確かめる必要がある |
| C | 外部のリレー（Cloudflare Workers 等）が受けて保存し、Control Plane が取りに行く（pull） | Mac mini が止まっていても取りこぼさない。Runner と同じ pull の考え方になる | 外部に保存先とその認証情報が増える。Evaloom 側でも署名を再検証するため、生の本文と署名ヘッダをそのまま保存する必要がある |

どの案でも、Evaloom 側の受け口（3.2〜3.4）は同じにする。

### 3.2 Webhook 専用の待ち受け

- `evaloomd` は本人用 API・Runner 用 API とは別に、Webhook 専用の待ち受けを `127.0.0.1:<別ポート>` に持つ（案）。この待ち受けでは `POST /api/webhooks/linear` だけを受け付け、それ以外のパスは 404 にする。
- トンネル（3.1）の転送先はこの待ち受けだけにする。トンネル経由で本人用 API と Runner 用 API に届かないようにするためである。
- 認証区分は「Webhook（署名）」とし、本人のセッションも Runner トークンも受け付けない（[`01-architecture.md`](./01-architecture.md) 3.4 に区分を追加する。3.9）。
- 本人用 API の Origin / Host の検証（[`01-architecture.md`](./01-architecture.md) 3.4）は、この待ち受けには適用しない。代わりに署名で認証する。
- 本文の大きさに上限を設ける（値は実装時に確定）。上限を超えたら読み切らずに 413 を返す。

### 3.3 署名の検証（requirements 6.6, 15.2）

Linear の Webhook は、署名シークレットで本文に付けた HMAC-SHA256 の署名をヘッダで送る。ヘッダ名、署名の形式、時刻の項目と許容する時刻差は、実装時に Linear の公式文書で確かめて確定する。

1. 本文を加工する前のバイト列のまま読む。
2. 署名シークレット（API キーとは別のもの、requirements 6.6）で HMAC-SHA256 を計算し、ヘッダの署名と定数時間で比べる。
3. 本文の送信時刻が許容範囲を外れていたら拒否する（再送攻撃の対策）。
4. 失敗したら 401 を返し、本文を event_inbox に保存しない。audit_logs に `webhook.reject` を記録する（理由、受信時刻、配信 ID のみ。本文と署名の値は記録しない）。

署名シークレットと API キーの置き場所は Q49。

### 3.4 受け付けと重複排除（requirements 6.3 の 1〜3、15.2）

1. 署名を検証できたら、event_inbox に1行書く（status=`pending`）。配信 ID（Linear が配信ごとに付ける ID）に一意制約を置き、同じ配信 ID の再送は保存せずに 200 を返す。
2. 書き込めたら 200 を返す。タスク作成までの処理は応答の後に非同期で行う（requirements 2.2「Go Worker + 永続ジョブ管理」）。Linear の応答待ちの時間制限に掛からないようにするためである。
3. DB に書き込めなかったら 500 を返し、Linear の再送に任せる（Fail Closed）。

event_inbox の処理は Control Plane の中の Worker が `pending` の行を古い順に1件ずつ取り出して行う。再起動しても `pending` の行が残っていれば処理を続ける。

### 3.5 イベントの判定

Webhook は Issue のイベントだけを購読する（案）。Comment など他の種類は購読しない。購読外の種類が届いたら status=`ignored` にする。

| イベント | 判定 | 処理 |
|---|---|---|
| Issue 作成で、作成時点のラベルに `ai-ready` がある | 起動 | 3.6 |
| Issue 更新で、更新前のラベルに `ai-ready` が無く、更新後にある | 起動 | 3.6 |
| Issue 更新で、`ai-ready` が付いたまま他の項目が変わった | 何もしない（requirements 6.2「再び起動しない」） | `ignored` |
| Issue 更新で、更新前に `ai-ready` があり、更新後に無い | 中止 | 3.7 |
| Issue 更新で、状態がキャンセルの種類に変わった | 中止 | 3.7 |
| Issue の削除・アーカイブ | 中止（案） | 3.7 |
| 上記以外 | 何もしない | `ignored` |

- 更新前の値は Webhook の本文にある「更新前の値」の項目から読む。項目名は実装時に Linear の公式文書で確かめる。
- Webhook の本文は契機の判定にだけ使う。ラベル、本文、Team、状態は、処理の時点で Linear GraphQL API から Issue を取り直して使う（案）。Webhook が順不同に届いても、現在の状態で判断できるようにするためである。取り直した時点で `ai-ready` がすでに外れていたら起動しない。
- `ai-ready` のラベル名と実行先ラベルの接頭辞 `repo:` は requirements 6.2 の推奨例である。全体設定で持ち、既定値をこの2つにする案とする（Q48）。

### 3.6 ルーティングとタスク作成（requirements 6.3 の 4〜8）

1. Issue の Team ID を取り出す。
2. Issue のラベルのうち、実行先ラベル（接頭辞 `repo:`）を集める。
3. 実行先ラベルと Team ID の組で、projects を引く（`linear_team_id` と `linear_routing_label` の両方が一致する Project）。
4. 判定:

| 状況 | 処理 | 保留の理由（tasks.intake_block_cause） |
|---|---|---|
| 実行先ラベルがちょうど1つで、Team とラベルが一致する Project が1件 | 次の 5 へ進む | - |
| 実行先ラベルが2つ以上 | 自動実行せず Blocked（requirements 6.3） | `routing_multiple_labels` |
| 実行先ラベルが登録されていない | 自動実行せず Blocked（requirements 6.3） | `routing_unknown_label` |
| ラベルは登録済みだが、Issue の Team がその Project の Team と違う | 自動実行せず Blocked（案） | `routing_team_mismatch` |
| 実行先ラベルが1つも無い | 自動実行せず Blocked（案） | `routing_no_label` |

5. 受け入れ条件を本文から取り出す（3.6.1）。取り出せなければ Blocked にし（requirements 6.3）、Linear へ受け入れ条件の記入を求めるコメントを返す（[`16-linear-feedback.md`](./16-linear-feedback.md)）。保留の理由は `missing_acceptance_criteria`。
6. Project の `default_role` に割り当てが無ければ、手動作成と同じく実行できない。保留の理由を `config_missing_assignment` として Blocked にする（案。手動作成では 422 で断るが、Webhook では断る相手がいないため）。
7. すべて通ったら、手動作成（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.1）と同じ手順でタスクと Execution（seq=1、mode=`continue`、`pending`）を作り、タスクを `queued` にする。違いは次のとおり。
   - tasks.source=`linear`。
   - title は Issue のタイトル、body は Issue の本文、acceptance_criteria は 3.6.1 で取り出した文。
   - short_name は Issue の識別子（例 `ENG-123`）を英小文字にしたもの（例 `eng-123`、案）。ブランチ名 `evaloom/<task-id>-<短い名前>` に入る（requirements 8.5）。
   - idempotency_key は `linear:<Issue ID>:<event_inbox の ID>`（案）。
8. linear_issue_links に Issue とタスクの対応を書く。
9. 1〜8 と event_inbox の status=`processed` への更新を、1つのトランザクションで行う。

Blocked にしたタスクは Execution を作らない（実行できる状態ではないため）。プロジェクトが決まらないタスクは tasks.project_id を NULL にする（Q45）。

タスク分類（requirements 8.2 工程6）は Phase 2 でも行わず、Default Harness で実行する。Collaboration Mode は Solo に固定する（requirements 8.2）。

#### 3.6.1 受け入れ条件の取り出し（Q44）

requirements 6.3 は「受け入れ条件が書かれていない Issue」を Blocked にするとしているが、本文のどこを受け入れ条件とみなすかは決めていない。案は次のとおり。

- 本文（Markdown）のうち、見出しの文言が「受け入れ条件」または「Acceptance Criteria」（大文字小文字を区別しない）の見出しから、次の同じ階層以上の見出しまでを受け入れ条件とする。
- 取り出した文が空白だけなら「書かれていない」とみなす（手動作成の検証と同じ、[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.1）。
- 本文全体は tasks.body に、取り出した部分は tasks.acceptance_criteria に入れる。

#### 3.6.2 Issue 本文の扱い（requirements 15.1）

- Issue のタイトルと本文は外部入力であり、信頼済みの命令として扱わない。Task Context として区切って渡す（[`07-context.md`](./07-context.md) 3.5）。この扱いは手動作成のタスクと変えない。
- Issue のコメントと添付は Phase 2 では取り込まない（案）。
- タスクの title / body / acceptance_criteria はタスク作成の時点で固定し、その後に Issue が編集されても反映しない（案、Q46）。Context の版を Execution ごとに固定する考え方（requirements 8.3）に合わせるためである。

#### 3.6.3 保留したタスクの解除（Q45）

requirements は、ルーティングで保留したタスクと受け入れ条件の無いタスクを Blocked にするとしているが、解除の手順は決めていない。案は次のとおり。

- 保留したタスク（tasks.intake_block_cause が入っているタスク）がある Issue について、Issue の更新イベントが届いたら 3.6 の判定をやり直す。判定に通れば、そのタスクに project_id と受け入れ条件を書き、Execution を作って `queued` にする。
- この再判定は「`ai-ready` が付いたまま更新されても再び起動しない」（requirements 6.2）の例外になる。起動済みのタスクを二重に起動するのではなく、まだ起動できていないタスクを起動するためである。本人の確認が要る。
- 本人は管理画面からこのタスクを中止できる（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.5 の `blocked` の経路）。

### 3.7 中止（requirements 6.2）

`ai-ready` が外されたとき、または Issue がキャンセルされたとき、その Issue に対応するタスクのうち待機中・実行中のものを中止する。

- 対象は linear_issue_links で引いたタスクのうち、状態が `queued` / `running` / `verifying` / `blocked`（保留を含む）のもの。手順は本人の中止（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.5）と同じにし、audit_logs の actor は `system`、理由は `linear_label_removed` / `linear_issue_cancelled` とする。
- `waiting_approval` のタスクも中止する（案）。そのタスクの未使用の承認は無効にする（[`17-approval.md`](./17-approval.md) 3.6）。
- 終端状態のタスクには何もしない。
- 中止したことは Linear にコメントで返す（[`16-linear-feedback.md`](./16-linear-feedback.md)）。

### 3.8 データモデル（Phase 2 で追加）

[`02-data-model.md`](./02-data-model.md) 3.3 で Phase 2 とした表のうち、受信とタスク作成に使うもの。共通の約束（ID、時刻、JSON 列）は 02 の 3.1 に従う。

#### projects への追加列

| 列 | 型 | 説明 | requirements 5.1 |
|---|---|---|---|
| linear_team_id | TEXT NULL | Linear Team ID | Linear Team ID |
| linear_routing_label | TEXT NULL | 実行先ラベルの名前（例 `repo:customer-system`） | Linear Routing Label |

- `UNIQUE(linear_team_id, linear_routing_label)`。同じ組を2つの Project に登録できない。
- どちらかが NULL の Project は Linear から実行されない。
- requirements 17章の project_integrations は Phase 2 では作らず、projects の列で持つ案とする（Q50）。連携先が Linear だけのためである。

#### tasks への追加列

| 列 | 型 | 説明 |
|---|---|---|
| source | TEXT | `manual` / `linear`。既存の行は `manual` |
| intake_block_cause | TEXT NULL | 3.6 の保留の理由。保留していなければ NULL |

- project_id を NULL 可にする。NULL を許すのは source=`linear` かつ intake_block_cause が NULL でないときだけとする（アプリ側で検証する）。
- 保留の理由の値: `routing_multiple_labels` / `routing_unknown_label` / `routing_team_mismatch` / `routing_no_label` / `missing_acceptance_criteria` / `config_missing_assignment`。
- 保留は Execution を持たないため、execution_blocks（[`02-data-model.md`](./02-data-model.md) 5.6）とは別に持つ。

#### event_inbox

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| source | TEXT | `linear` |
| delivery_id | TEXT | Linear の配信 ID。`UNIQUE(source, delivery_id)` |
| event_type | TEXT | 例 `Issue` |
| action | TEXT | 例 `create` / `update` / `remove` |
| resource_id | TEXT | Linear の Issue ID |
| payload | TEXT (JSON) | 受信した本文。署名シークレット・API キーは含まれない |
| status | TEXT | `pending` / `processed` / `ignored` / `failed` |
| result | TEXT (JSON) NULL | 判定結果（起動・中止・保留の理由、作ったタスク ID など） |
| attempts | INTEGER | 処理を試みた回数 |
| received_at / processed_at | TEXT | |

保存期間は Q24 と合わせて決める。

#### linear_issue_links

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| linear_issue_id | TEXT | Linear の Issue ID |
| issue_identifier | TEXT | 表示用の識別子（例 `ENG-123`） |
| issue_url | TEXT | |
| team_id | TEXT | タスク作成時点の Team ID |
| task_id | TEXT FK→tasks | `UNIQUE(task_id)` |
| trigger_event_id | TEXT FK→event_inbox | 起動の契機になったイベント |
| created_at | TEXT | |

1つの Issue に複数のタスクが対応しうる（Q46 で `ai-ready` を付け直したときに新しいタスクを作る場合）。そのため linear_issue_id には一意制約を置かない。同じ Issue に、終端状態でないタスクは同時に1件までとする（アプリ側で検証する）。

### 3.9 既存の仕様への影響

Phase 2 の実装に入るときに、次の確定済みの仕様を書き換える必要がある。書き換えたら、その仕様の status を draft に戻す（spec-writer の規約）。

| 仕様 | 書き換える箇所 |
|---|---|
| [`01-architecture.md`](./01-architecture.md) | 3.1 に Linear の認証情報を持つ主体（Control Plane）を追加。3.2 に Webhook 専用の待ち受けを追加。3.4 に認証区分「Webhook（署名）」を追加。5.1 から `/api/webhooks/linear` を「実装しない」から外す |
| [`02-data-model.md`](./02-data-model.md) | 3.3 の表から project_integrations 以外の Linear のテーブルを外し、本書 3.8 を参照する |
| [`03-registry.md`](./03-registry.md) | Project の登録項目に Linear Team ID と実行先ラベルを追加 |
| [`04-task-lifecycle.md`](./04-task-lifecycle.md) | 3.2 の遷移表に、保留（Execution の無い `blocked`）と Linear 起因の中止を追加 |

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 標準のルーティング | Linear Team + Issue Label。Linear Project を必須にしない | requirements 6.1 |
| 自動実行の契機 | `ai-ready` が付いた時点。付いたままの更新では再び起動しない | requirements 6.2 |
| 中止の契機 | `ai-ready` が外された、Issue がキャンセルされた | requirements 6.2 |
| 保留（Blocked） | 実行先ラベルが複数、未登録ラベル、受け入れ条件が無い | requirements 6.3 |
| 受け入れ条件の無い Issue | Blocked にし、記入を求めるコメントを返す | requirements 6.3 |
| 署名検証 | 必須。API キーとは別の署名シークレットで検証する | requirements 6.6, 15.2 |
| 受信経路 | 公開 HTTPS エンドポイント経由。Mac mini のポートを直接公開しない | requirements 2.2, 6.6 |
| イベント重複排除 | 必須 | requirements 6.3, 15.2 |
| Issue 本文 | 信頼済みの命令として扱わない | requirements 15.1 |
| タスク分類 | 行わない。Default Harness で実行 | requirements 8.2 |
| 扱うタスク | 開発タスクだけ | requirements 8.1 |
| 同時実行の上限 | 全体2本・同一プロジェクト1本（Linear 起因のタスクも同じ） | requirements 8.7 |
| 自動リトライ | 0回 | requirements 8.3 |

## 5. インターフェース

| Method | Endpoint | 認証区分 | 内容 | 出典 |
|---|---|---|---|---|
| POST | /api/webhooks/linear | Webhook（署名） | Linear イベント受信。Webhook 専用の待ち受けだけで受け付ける | 18章 |
| GET | /api/linear/events | 本人用 | event_inbox の一覧（状態、判定結果）。管理画面で受信の状況を確かめるため | 追加案 |
| PATCH | /api/projects/{id} | 本人用 | 既存の更新 API に `linearTeamId` / `linearRoutingLabel` を追加 | 追加案 |
| GET / PATCH | /api/settings | 本人用 | 既存の全体設定に `linear.triggerLabel` / `linear.routingLabelPrefix` を追加（Q48） | 追加案 |

- 応答の形とエラー形式は [`01-architecture.md`](./01-architecture.md) 5.3 に従う。ただし `/api/webhooks/linear` の応答本文は空にする（Linear には中身を返さない）。
- タスクの API（`GET /api/tasks` 等）の応答に `source`、`intakeBlockCause`、`linear`（識別子と URL）を足す。

Linear GraphQL API で使う操作（Issue の取得、ラベルの取得、Team の取得）は Control Plane の Linear クライアントにまとめる。認証は個人 API キー（requirements 6.6）。

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| 署名が無い・一致しない・時刻が範囲外 | 401。保存しない。audit_logs に `webhook.reject` |
| 本文が上限を超える | 413。保存しない |
| 同じ配信 ID の再送 | 保存せず 200 |
| DB に書けない | 500。Linear の再送に任せる |
| Issue の取り直しで Linear API が失敗 | event_inbox の行を `pending` のまま attempts を増やし、間隔を空けて再試行する。上限回数（実装時に確定）を超えたら `failed` にし、Dashboard のエラー状況に出す |
| Linear API の認証エラー | `failed`。API キーの値はログに出さない。macOS 通知で本人に知らせる（案） |
| ルーティング・受け入れ条件で保留 | タスクを Blocked にしてコメントを要求する。エラーとしては扱わない |
| 同じ Issue に終端状態でないタスクがすでにある状態で起動イベント | 新しいタスクを作らず `ignored`（result に理由） |

## 7. スコープ外

- Linear Project を使ったルーティング（requirements 6.1 の任意機能）。Phase 2 では作らない案（Q47）
- Issue のコメント・添付の取り込み
- Gmail / GitHub など Linear 以外のイベント入力（Phase 6、requirements 8.1）
- 調査・文書タスク（requirements 8.1）
- タスク分類・task_steps（requirements 8.2）
- 別端末・遠隔からの管理画面へのアクセス（requirements 15.5）。公開するのは Webhook の受け口だけである

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API と認証区分
- [`02-data-model.md`](./02-data-model.md) tasks / executions と状態の値
- [`03-registry.md`](./03-registry.md) Project の登録
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) タスク作成と中止の手順
- [`07-context.md`](./07-context.md) Task Context の扱い
- [`13-management-ui.md`](./13-management-ui.md) 管理画面
- [`16-linear-feedback.md`](./16-linear-feedback.md) Linear への結果返却
- [`17-approval.md`](./17-approval.md) 承認

## 9. 実装単位

- [ ] （人間） 公開 HTTPS 経路の方式を決め（Q42）、Linear に Webhook を登録して署名シークレットと個人 API キーを Mac mini に置く（Q49）
- [ ] [Backend] Phase 2 のマイグレーション（projects の Linear 列、tasks の source・intake_block_cause と project_id の NULL 許可、event_inbox、linear_issue_links）
  - 受け入れ基準: 既存の tasks の行が source=manual になる; UNIQUE(linear_team_id, linear_routing_label) と UNIQUE(source, delivery_id) が効く; source=manual で project_id が NULL の行を書けない
- [ ] [Backend] Webhook 専用の待ち受けと `POST /api/webhooks/linear`（署名検証、時刻の確認、本文の上限）
  - 受け入れ基準: 正しい署名の要求が 200 で event_inbox に1行できる; 署名が違う・無い・時刻が範囲外の要求が 401 で保存されず audit_logs に残る; 専用の待ち受けで本人用 API のパスが 404 になる; 本人用の待ち受けで `/api/webhooks/linear` を受け付けない
- [ ] [Backend] event_inbox の重複排除と Worker（pending の取り出し、再起動後の継続、再試行と failed）
  - 受け入れ基準: 同じ配信 ID を2回送っても行が1つ; Control Plane を再起動しても pending の行が処理される; Linear API を失敗させるテストで attempts が増え、上限で failed になる
- [ ] [Backend] Linear GraphQL クライアント（Issue・ラベル・Team の取得、API キーの読み込み）
  - 受け入れ基準: テスト用のサーバーに対して Issue のラベルと本文を取得できる; API キーの値がログと DB に現れない
- [ ] [Backend] イベントの判定（3.5 の表）
  - 受け入れ基準: 表の各行に対応する Webhook の本文で、起動・中止・何もしないが期待どおりになる; 取り直した Issue で ai-ready が外れていると起動しない
- [ ] [Backend] ルーティングと受け入れ条件の取り出し、タスク作成（3.6）
  - 受け入れ基準: Team とラベルが一致する Issue から queued のタスクと pending の Execution ができる; 実行先ラベルが2つ・未登録・無し・Team 違いでは Execution の無い blocked のタスクができ、intake_block_cause が表の値になる; 受け入れ条件の見出しが無い Issue が missing_acceptance_criteria で保留になる; Linear Project を設定していない Issue でも実行される
- [ ] [Backend] 保留したタスクの再判定（3.6.3）
  - 受け入れ基準: 保留中のタスクの Issue に実行先ラベルを直す更新が届くと、同じタスクが queued になり Execution が1つできる; 保留していないタスクの Issue の更新では何も起きない
- [ ] [Backend] Linear 起因の中止（3.7）
  - 受け入れ基準: ai-ready を外す Webhook で queued・running・blocked のタスクが中止の経路に入る; Issue のキャンセルでも同じになる; completed のタスクは変わらない; audit_logs の actor が system になる
- [ ] [Backend] `GET /api/linear/events` と、Project・全体設定の API への Linear 項目の追加
  - 受け入れ基準: 受信したイベントが新しい順に判定結果つきで返る; 同じ Team とラベルの組を2つ目の Project に登録すると 409
- [ ] [Frontend] Project 登録・編集に Linear Team ID と実行先ラベル、Settings にトリガーのラベル名と接頭辞、Dashboard に Linear の受信状況
  - 受け入れ基準: Project に Team ID とラベルを保存して表示できる; 409 の内容がフォームに表示される; failed のイベントが Dashboard のエラー状況に出る
- [ ] [Frontend] タスク一覧・詳細での Linear 起因のタスクの表示（Issue へのリンク、保留の理由）
  - 受け入れ基準: source=linear のタスクに Issue の識別子とリンクが出る; 保留中のタスクに理由が文字で表示され、続行ボタンが出ず中止だけが出る
