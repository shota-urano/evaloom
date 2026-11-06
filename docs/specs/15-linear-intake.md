---
status: draft
範囲: Linear API の定期照会（ポーリング）による ai-ready 付き Issue の取得、付与・取り外しの検出と重複排除、Team + Label ルーティング、Linear からの自動タスク作成、ai-ready の取り外しと Issue のキャンセルによる中止
---

# 15. Linear 連携：取り込みとタスク作成 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend（設定画面は Frontend） ｜ **範囲**: requirements 8.2 の工程1〜4（イベント受信、確認、重複排除、プロジェクト特定）を Linear について行い、タスクを作る。Linear の変化は Evaloom から Linear API へ一定間隔で照会して取得する（ポーリング、requirements 6.6）。結果の返却は [`16-linear-feedback.md`](./16-linear-feedback.md)

この文書は Phase 2 の下書きである。requirements で確定していない点は「案」とし、00-overview.md の要確認リスト（Q43〜Q50、Q64〜Q67）に載せている。

## 1. 目的

- Linear の Issue に `ai-ready` が付いた時点で、Evaloom が自動でタスクを作り、対象プロジェクトで実行できるようにする（requirements 6.2, 19章 Phase 2）。
- 実行先は Linear Team と Issue Label で決める。Linear Project を使わなくても自動実行できるようにする（条件19, 20、requirements 6.1）。
- Mac mini のポートを公開しない。Linear との通信は Mac mini から外へ向かう HTTPS だけで行う（requirements 2.2, 6.6, 15.2）。
- 外部から来る Issue 本文を信頼済みの命令として扱わない（requirements 15.1）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | Linear GraphQL API の照会結果（対象 Team の `ai-ready` 付き Issue の一覧と、前回あったのに今回の一覧から消えた Issue の個別の取得結果） |
| 出力 | linear_issue_states、tasks（`source=linear`）、executions、linear_issue_links、audit_logs、[`16-linear-feedback.md`](./16-linear-feedback.md) へのコメント要求 |

テーブルの定義は本書 3.8 に書き、[`02-data-model.md`](./02-data-model.md) 3.3 から参照する。

## 3. 処理詳細

### 3.1 取得の方式（requirements 6.6、Q42, Q43）

Evaloom は Webhook を使わず、Control Plane の中の Linear Poller が Linear GraphQL API へ一定間隔で照会する。

- 外から Mac mini に届く住所（公開 HTTPS エンドポイント、トンネル）を持たない。Webhook の登録、署名シークレット、署名の検証も無い。
- Linear への接続は Control Plane から外へ向かう HTTPS だけで行う。認証は API キー（requirements 6.6）。置き場所は Q49。
- 照会の間隔は1分とする案（Q43）。Linear の変化は最大でこの間隔だけ遅れて取り込まれる。開発タスクの開始が1分遅れても困らないため、この遅れを許容する（requirements 6.6）。
- 照会は1本ずつ行う。前の照会が終わっていなければ次の照会を始めない。
- Control Plane の起動時に1回照会し、その後は間隔ごとに照会する。Mac mini が止まっていた間に付いた `ai-ready` は、再開後の最初の照会で取り込む。
- Linear API のレート制限（要求数と複雑さの上限）は実装時に Linear の公式文書で確かめ、1分間隔の照会が収まることを確かめる。収まらなければ間隔を延ばす（requirements 21章）。

### 3.2 照会の対象

1回の照会で、次の条件に当てはまる Issue をすべて取得する。

- Team が、`linear_team_id` を登録した Project（3.8）のどれかの Team である。複数の Project が同じ Team を登録していても、その Team は1回だけ照会の対象にする。
- ラベルに `ai-ready`（全体設定のトリガーのラベル名、Q48）が付いている。
- アーカイブされていない。

取得する項目は、Issue ID、識別子（例 `ENG-123`）、URL、タイトル、本文、Team ID、ラベル名の一覧、状態の種類（キャンセルかどうか）、更新時刻（updatedAt）である。Webhook のように契機の本文と現在の内容がずれることは無いため、取得した内容をそのまま判定に使う。

- 照会の結果は複数のページに分かれうる。最後のページまで読み切ってから判定に進む。途中で失敗したら、その回の照会は判定に使わずに捨てる（3.4 の差分を、欠けた一覧から計算しないため）。
- 照会では Issue だけを読み、コメントと添付は読まない（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.5）。
- `linear_team_id` を登録した Project が1件も無いときは照会しない。

### 3.3 前回の照会結果の持ち方（linear_issue_states）

ポーリングでは、Linear から「ラベルが付いた」という出来事は届かず、照会した時点の状態だけが分かる。そのため Evaloom は、前回の照会で `ai-ready` が付いていた Issue を linear_issue_states（3.8）に記録しておき、今回の照会結果と比べて付与と取り外しを判定する。

- linear_issue_states は Issue ごとに1行で、`ai_ready`（前回の照会で付いていたか）、`attach_seq`（付与を検出した回数）、最後に見た更新時刻を持つ。
- 起動したかどうか、起動したタスクが終わったかどうかは linear_issue_links と tasks で判定する。linear_issue_states は「前回どう見えたか」だけを持つ。

### 3.4 付与・取り外しの判定（requirements 6.2）

照会を読み切ったら、照会の結果（今回の一覧）と linear_issue_states（前回の記録）を Issue ごとに比べる。

| 前回の記録 | 今回の一覧 | 判定 | 処理 |
|---|---|---|---|
| 行が無い、または `ai_ready=0` | ある（キャンセル以外） | 付与（起動） | attach_seq を1増やし、3.6 |
| 行が無い、または `ai_ready=0` | ある（状態がキャンセル） | 何もしない（案） | 行を `ai_ready=1` にするだけで起動しない |
| `ai_ready=1` | ある（キャンセル以外） | 付いたまま（requirements 6.2「再び起動しない」） | 何もしない。保留中のタスクがあれば 3.6.3 |
| `ai_ready=1` | ある（状態がキャンセル） | 中止 | 3.7（理由 `linear_issue_cancelled`） |
| `ai_ready=1` | 無い | 消えた理由を確かめる | 下の「一覧から消えた Issue」 |

一覧から消えた Issue は、Issue ID で1件ずつ取り直して理由を見分ける。

| 取り直した結果 | 判定 | 処理 |
|---|---|---|
| `ai-ready` が外れていた | 取り外し（中止） | 3.7（理由 `linear_label_removed`）。行を `ai_ready=0` にする |
| 状態がキャンセルになっていた | 中止 | 3.7（理由 `linear_issue_cancelled`） |
| 削除・アーカイブされていた | 中止（案） | 3.7（理由 `linear_issue_removed`）。行を消す |
| 登録していない Team へ移っていた | 中止（案、Q67） | 3.7（理由 `linear_issue_moved`）。行を `ai_ready=0` にする |
| 取り直しに失敗した | 決めない | 行を変えず、次の照会で再び確かめる |

- 判定は「前回付いていなかったものが今回付いている」ことだけを見る。`ai-ready` が付いたまま本文、ラベル、状態が変わっても起動しない（requirements 6.2）。
- 照会と照会の間（案では1分の間）に付けて外した、または外して付け直した変化は、Evaloom からは見えない。前者は何も起きず、後者は「付いたまま」として扱う（案、Q65）。
- 最初の照会では前回の記録が無い。このとき、すでに `ai-ready` が付いている Issue をすべて「付与」とみなすと、導入前から付いている Issue を一斉に起動してしまう。そのため Team ごとの最初の照会では、一覧の Issue を `ai_ready=1` として記録するだけで起動しない（案、Q64）。Team ごとに最初の照会を終えたかどうかは linear_poll_teams（3.8）で持つ。本人がこれらの Issue を起動したいときは、`ai-ready` を外して付け直す。
- Mac mini が止まっていた間に付いた `ai-ready` は、再開後の照会で「前回は付いていない、今回は付いている」になるため、通常の付与として起動する。止まっていた間に外された `ai-ready` も、同じく取り外しとして中止する。
- `ai-ready` のラベル名と実行先ラベルの接頭辞 `repo:` は requirements 6.2 の推奨例である。全体設定で持ち、既定値をこの2つにする案とする（Q48）。

### 3.5 重複排除（requirements 6.3, 15.2）

Webhook の再送が無くなったため、重複排除の対象は「同じ付与から二重にタスクを作ること」である。次の3つで防ぐ。

1. **1回の照会の判定と書き込みを1つのトランザクションにする**: Issue ごとに、linear_issue_states の更新と、3.6 のタスク作成（または 3.7 の中止）を同じトランザクションで行う。途中で止まっても、次の照会では前回の記録が変わっていないため、同じ判定をやり直す。
2. **冪等キーを付与の回数で決める**: tasks.idempotency_key を `linear:<Issue ID>:<attach_seq>` とする（案）。同じ付与からタスクを2回作ろうとすると、一意制約で弾かれる。
3. **同じ Issue に終端でないタスクは同時に1件まで**: 付与を検出しても、linear_issue_links で引いたタスクのうち終端状態でないものがあれば、新しいタスクを作らない（Q46）。linear_issue_states の結果欄に理由を残す。

照会は1本ずつ行う（3.1）ため、2つの照会が同じ Issue を同時に判定することは無い。

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
6. Project の `default_role` に割り当てが無ければ、手動作成と同じく実行できない。保留の理由を `config_missing_assignment` として Blocked にする（案。手動作成では 422 で断るが、Linear からの取り込みでは断る相手がいないため）。
7. すべて通ったら、手動作成（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.1）と同じ手順でタスクと Execution（seq=1、mode=`continue`、`pending`）を作り、タスクを `queued` にする。違いは次のとおり。
   - tasks.source=`linear`。
   - title は Issue のタイトル、body は Issue の本文、acceptance_criteria は 3.6.1 で取り出した文。
   - short_name は Issue の識別子（例 `ENG-123`）を英小文字にしたもの（例 `eng-123`、案）。ブランチ名 `evaloom/<task-id>-<短い名前>` に入る（requirements 8.5）。
   - idempotency_key は `linear:<Issue ID>:<attach_seq>`（案、3.5）。
8. linear_issue_links に Issue とタスクの対応を書く。
9. 1〜8 と linear_issue_states の更新を、1つのトランザクションで行う（3.5）。

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

- 保留したタスク（tasks.intake_block_cause が入っているタスク）がある Issue について、照会で取得した更新時刻が linear_issue_states に記録した値より新しければ、3.6 の判定をやり直す。判定に通れば、そのタスクに project_id と受け入れ条件を書き、Execution を作って `queued` にする。保留の理由が変わらなければ何もしない（同じ理由のコメントを2回出さない。[`16-linear-feedback.md`](./16-linear-feedback.md) 3.2）。
- この再判定は「`ai-ready` が付いたまま更新されても再び起動しない」（requirements 6.2）の例外になる。起動済みのタスクを二重に起動するのではなく、まだ起動できていないタスクを起動するためである。本人の確認が要る。
- 再判定は新しいタスクを作らない。同じタスクを起動できるかを見直すだけである。
- 本人は管理画面からこのタスクを中止できる（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.5 の `blocked` の経路）。

### 3.7 中止（requirements 6.2）

`ai-ready` が外されたとき、または Issue がキャンセルされたとき、その Issue に対応するタスクのうち待機中・実行中のものを中止する。3.4 の表で中止とした場合も同じに扱う。

- 対象は linear_issue_links で引いたタスクのうち、状態が `queued` / `running` / `verifying` / `blocked`（保留を含む）のもの。手順は本人の中止（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.5）と同じにし、audit_logs の actor は `system`、理由は 3.4 の表の値（`linear_label_removed` / `linear_issue_cancelled` / `linear_issue_removed` / `linear_issue_moved`）とする。
- `waiting_approval` のタスクも中止する（案）。そのタスクの未使用の承認は無効にする（[`17-approval.md`](./17-approval.md) 3.6）。
- 終端状態のタスクには何もしない。
- 中止したことは Linear にコメントで返す（[`16-linear-feedback.md`](./16-linear-feedback.md)）。Issue が削除・アーカイブされていた場合は、送信が失敗として記録される（16 の 6章）。

### 3.8 データモデル（Phase 2 で追加）

[`02-data-model.md`](./02-data-model.md) 3.3 で Phase 2 とした表のうち、取り込みとタスク作成に使うもの。共通の約束（ID、時刻、JSON 列）は 02 の 3.1 に従う。

requirements 17章の event_inbox（受信イベント）は Phase 2 では作らない（案、Q66）。Webhook をやめたため、受け取った本文を先に保存して後で処理する必要が無くなった。前回の照会結果は linear_issue_states が持ち、判定の結果は linear_issue_states と audit_logs に残す。

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

#### linear_issue_states（追加）

17章に無い。前回の照会で見えた Issue の状態を持ち、3.4 の差分の判定に使う。

| 列 | 型 | 説明 |
|---|---|---|
| linear_issue_id | TEXT PK | Linear の Issue ID |
| issue_identifier | TEXT | 表示用の識別子（例 `ENG-123`） |
| team_id | TEXT | 最後に見た Team ID |
| ai_ready | INTEGER | 1 = 前回の照会で `ai-ready` が付いていた、0 = 付いていなかった |
| attach_seq | INTEGER | 付与を検出した回数。3.5 の冪等キーに使う |
| issue_updated_at | TEXT | 最後に見た Linear の更新時刻（3.6.3 の再判定に使う） |
| last_decision | TEXT NULL | 最後の判定（`started` / `held` / `cancelled` / `skipped_active_task` / `skipped_initial` / `skipped_cancelled` など） |
| last_decision_at | TEXT NULL | |
| first_seen_at / last_seen_at | TEXT | |

- `ai_ready=0` の行は、中止の後に付け直しを検出するために残す（Q46）。保存期間は Q24 と合わせて決める。

#### linear_poll_teams（追加）

17章に無い。照会の対象 Team ごとの状況を持つ。

| 列 | 型 | 説明 |
|---|---|---|
| team_id | TEXT PK | Linear Team ID |
| baseline_done_at | TEXT NULL | Team ごとの最初の照会（3.4、Q64）を終えた時刻。NULL のうちは起動しない |
| last_success_at | TEXT NULL | 最後に照会を読み切れた時刻 |
| last_error | TEXT NULL | 最後の照会の失敗の分類（`auth` / `rate_limited` / `network` / `other`）。成功したら NULL に戻す。API キーの値や応答の本文は入れない |
| consecutive_failures | INTEGER | 続けて失敗した回数 |

- Project から `linear_team_id` を外して、その Team を登録した Project が無くなったら行を消す。後で登録し直したら、もう一度最初の照会からやり直す（案、Q64）。

#### linear_issue_links

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| linear_issue_id | TEXT | Linear の Issue ID |
| issue_identifier | TEXT | 表示用の識別子（例 `ENG-123`） |
| issue_url | TEXT | |
| team_id | TEXT | タスク作成時点の Team ID |
| task_id | TEXT FK→tasks | `UNIQUE(task_id)` |
| attach_seq | INTEGER | 起動の契機になった付与の回数（linear_issue_states.attach_seq） |
| created_at | TEXT | |

1つの Issue に複数のタスクが対応しうる（Q46 で `ai-ready` を付け直したときに新しいタスクを作る場合）。そのため linear_issue_id には一意制約を置かない。`UNIQUE(linear_issue_id, attach_seq)` で、同じ付与から2つのタスクを作れないようにする。同じ Issue に、終端状態でないタスクは同時に1件までとする（アプリ側で検証する）。

### 3.9 既存の仕様への影響

Phase 2 の実装に入るときに、次の確定済みの仕様を書き換える必要がある。書き換えたら、その仕様の status を draft に戻す（spec-writer の規約）。

| 仕様 | 書き換える箇所 |
|---|---|
| [`01-architecture.md`](./01-architecture.md) | 3.1 に Linear の認証情報を持つ主体（Control Plane）と Linear Poller を追加。7章の「Linear Webhook 用の公開 HTTPS エンドポイント（Phase 2）」を外す。5.1 の「実装しない」から `/api/webhooks/linear` を外す（requirements 18章から消えたため） |
| [`02-data-model.md`](./02-data-model.md) | 3.3 の表から project_integrations と event_inbox 以外の Linear のテーブルを外し、本書 3.8 を参照する。event_inbox は Phase 2 で作らないことを書く（Q66） |
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
| 取得の方式 | Linear API への定期照会（ポーリング）。Webhook は使わない | requirements 6.6 |
| 受信口 | 持たない。Mac mini のポートを公開しない | requirements 2.2, 6.6, 15.2 |
| 照会の間隔 | 1分（案、Q43） | - |
| イベント重複排除 | 必須 | requirements 6.3, 15.2 |
| Issue 本文 | 信頼済みの命令として扱わない | requirements 15.1 |
| タスク分類 | 行わない。Default Harness で実行 | requirements 8.2 |
| 扱うタスク | 開発タスクだけ | requirements 8.1 |
| 同時実行の上限 | 全体2本・同一プロジェクト1本（Linear 起因のタスクも同じ） | requirements 8.7 |
| 自動リトライ | 0回 | requirements 8.3 |

## 5. インターフェース

| Method | Endpoint | 認証区分 | 内容 | 出典 |
|---|---|---|---|---|
| GET | /api/linear/status | 本人用 | 照会の状況（Team ごとの最後に成功した時刻、失敗の分類、続けて失敗した回数）と、`ai-ready` が付いている Issue の一覧と最後の判定。管理画面で取り込みの状況を確かめるため | 追加案 |
| PATCH | /api/projects/{id} | 本人用 | 既存の更新 API に `linearTeamId` / `linearRoutingLabel` を追加 | 追加案 |
| GET / PATCH | /api/settings | 本人用 | 既存の全体設定に `linear.triggerLabel` / `linear.routingLabelPrefix` を追加（Q48） | 追加案 |

- 応答の形とエラー形式は [`01-architecture.md`](./01-architecture.md) 5.3 に従う。
- タスクの API（`GET /api/tasks` 等）の応答に `source`、`intakeBlockCause`、`linear`（識別子と URL）を足す。
- Linear から Evaloom へ届く要求は無い。外から呼ばれる API は追加しない。

Linear GraphQL API で使う操作（対象 Team の `ai-ready` 付き Issue の一覧、Issue の個別取得、ラベルと Team の取得）は Control Plane の Linear クライアントにまとめる。認証は API キー（requirements 6.6、Q49）。

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| 照会の途中（ページの途中を含む）で Linear API が失敗 | その回の照会を捨て、判定もしない。linear_poll_teams の last_error と consecutive_failures を更新し、次の間隔で照会し直す |
| 続けて失敗した回数が上限（実装時に確定）を超えた | Dashboard のエラー状況に出し、macOS 通知で本人に知らせる（案）。照会は続ける |
| Linear API の認証エラー | last_error=`auth`。API キーの値はログに出さない。macOS 通知で本人に知らせる（案）。照会は間隔ごとに続ける |
| レート制限に掛かった | last_error=`rate_limited`。Linear が返す再開の目安まで照会を止める |
| 一覧から消えた Issue の取り直しに失敗 | その Issue の行を変えず、次の照会で再び確かめる。ほかの Issue の判定は進める |
| タスク作成のトランザクションが失敗 | その Issue の linear_issue_states を変えない。次の照会で同じ判定をやり直す（3.5） |
| ルーティング・受け入れ条件で保留 | タスクを Blocked にしてコメントを要求する。エラーとしては扱わない |
| 同じ Issue に終端状態でないタスクがすでにある状態で付与を検出 | 新しいタスクを作らない。last_decision=`skipped_active_task` |

## 7. スコープ外

- Linear Webhook の受信、公開 HTTPS エンドポイント、トンネル、署名の検証（Q42 で取り消し）
- Linear Project を使ったルーティング（requirements 6.1 の任意機能）。Phase 2 では作らない案（Q47）
- Issue のコメント・添付の取り込み
- Gmail / GitHub など Linear 以外のイベント入力（Phase 6、requirements 8.1）
- 調査・文書タスク（requirements 8.1）
- タスク分類・task_steps（requirements 8.2）
- 別端末・遠隔からの管理画面へのアクセス（requirements 15.5）

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

- [ ] （人間） Evaloom 専用の Linear ユーザーの API キーを作って Mac mini の `~/.config/evaloom/` に置く（Q49）
- [ ] [Backend] Phase 2 のマイグレーション（projects の Linear 列、tasks の source・intake_block_cause と project_id の NULL 許可、linear_issue_states、linear_poll_teams、linear_issue_links）
  - 受け入れ基準: 既存の tasks の行が source=manual になる; UNIQUE(linear_team_id, linear_routing_label) と UNIQUE(linear_issue_id, attach_seq) が効く; source=manual で project_id が NULL の行を書けない; event_inbox の表が作られない
- [ ] [Backend] Linear GraphQL クライアント（API キーの読み込み、対象 Team の ai-ready 付き Issue の一覧をページを読み切って取得、Issue の個別取得）
  - 受け入れ基準: テスト用のサーバーに対して、複数ページに分かれた一覧を最後まで取得できる; ページの途中で失敗させると、一覧を返さずにエラーになる; API キーの値がログと DB に現れない; API キーのファイルが無い・権限が 0600 でないときは照会を始めず理由を記録する
- [ ] [Backend] Linear Poller（起動時と1分ごとの照会、1本ずつの実行、Team ごとの成功・失敗の記録、レート制限で待つ）
  - 受け入れ基準: Control Plane を起動すると1回照会し、その後は設定した間隔で照会する; 照会が間隔より長く掛かっても、2本目が同時に始まらない; 失敗すると linear_poll_teams の last_error と consecutive_failures が更新され、成功で戻る; linear_team_id を登録した Project が無いときは照会しない
- [ ] [Backend] 付与・取り外しの判定（3.4 の2つの表、最初の照会の扱い、重複排除）
  - 受け入れ基準: 前回に無く今回ある Issue で起動になり attach_seq が1増える; 付いたまま本文やラベルを変えた Issue で何も起きない; 一覧から消えた Issue を取り直し、ラベルが外れていれば中止、キャンセルなら中止になる; Team の最初の照会では、一覧の Issue を記録するだけでタスクが作られない; 外して付け直した Issue で、前のタスクが終端なら新しいタスクができ、終端でなければできない; 判定の途中で失敗させても、次の照会でタスクが二重にできない
- [ ] [Backend] ルーティングと受け入れ条件の取り出し、タスク作成（3.6）
  - 受け入れ基準: Team とラベルが一致する Issue から queued のタスクと pending の Execution ができる; 実行先ラベルが2つ・未登録・無し・Team 違いでは Execution の無い blocked のタスクができ、intake_block_cause が表の値になる; 受け入れ条件の見出しが無い Issue が missing_acceptance_criteria で保留になる; Linear Project を設定していない Issue でも実行される
- [ ] [Backend] 保留したタスクの再判定（3.6.3）
  - 受け入れ基準: 保留中のタスクの Issue で実行先ラベルを直し、更新時刻が進んだ状態で照会すると、同じタスクが queued になり Execution が1つできる; 更新時刻が変わらない照会では再判定しない; 保留の理由が変わらなければ何も起きない; 保留していないタスクの Issue の更新では何も起きない
- [ ] [Backend] Linear 起因の中止（3.7）
  - 受け入れ基準: ai-ready を外した Issue の照会で queued・running・blocked のタスクが中止の経路に入る; Issue のキャンセル、削除・アーカイブでも同じになる; completed のタスクは変わらない; audit_logs の actor が system で、理由が 3.4 の表の値になる
- [ ] [Backend] `GET /api/linear/status` と、Project・全体設定の API への Linear 項目の追加
  - 受け入れ基準: Team ごとの最後に成功した時刻と失敗の分類、ai-ready が付いている Issue と最後の判定が返る; 同じ Team とラベルの組を2つ目の Project に登録すると 409; 応答に API キーの値が含まれない
- [ ] [Frontend] Project 登録・編集に Linear Team ID と実行先ラベル、Settings にトリガーのラベル名と接頭辞、Dashboard に Linear の取り込みの状況
  - 受け入れ基準: Project に Team ID とラベルを保存して表示できる; 409 の内容がフォームに表示される; 照会が続けて失敗しているとき、Dashboard のエラー状況に失敗の分類と最後に成功した時刻が出る
- [ ] [Frontend] タスク一覧・詳細での Linear 起因のタスクの表示（Issue へのリンク、保留の理由）
  - 受け入れ基準: source=linear のタスクに Issue の識別子とリンクが出る; 保留中のタスクに理由が文字で表示され、続行ボタンが出ず中止だけが出る
