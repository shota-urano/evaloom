---
status: draft
written: 2026-11-10
範囲: Linear API の定期照会（ポーリング）による ai-ready 付き Issue の取得、付与・取り外しの検出と重複排除、振り分けのラベルによる振り分けと振り分けの提案、Linear からの自動タスク作成、ai-ready の取り外しと Issue のキャンセルによる中止
---

# 15. Linear 連携：取り込みと振り分け 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend（設定画面は Frontend） ｜ **範囲**: Linear の Issue を取り込み、振り分けのラベルで振り分け先（Project かスペシャルハーネス）を決めてタスクを作る。ラベルで決まらない Issue は振り分けの提案に回す。Linear の変化は Evaloom から Linear API へ一定間隔で照会して取得する（ポーリング、requirements 6.6）。結果の返却は [`16-linear-feedback.md`](./16-linear-feedback.md)

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。振り分けはラベルだけで決め、Linear の Team は振り分けに使わない（本人決定 2）。ラベルで決まらない Issue は保留にせず、AI の提案と本人の承認に回す（vision.md 3章）。テーブルと列挙値の名前は [`02-data-model.md`](./02-data-model.md)、API は [`01-architecture.md`](./01-architecture.md) に従う。決定済みの点は 00-overview.md の要確認リスト（Q43〜Q49、Q64〜Q67、Q76〜Q79、Q81、Q84、Q86）に載せている。

## 1. 目的

- Linear の Issue に `ai-ready` が付いた時点で、Evaloom が自動でタスクを作り、振り分け先で実行できるようにする（requirements 6.2）。
- 振り分け先を Issue の振り分けのラベルだけで決める。ラベルが無い、知らない、2つ以上のときは、AI が振り分け先を提案し、本人が管理画面で承認するまで実行しない（vision.md 3章、本人決定 2）。
- 開発の仕事に限らず、振り分けの表にある振り分け先なら、どの種類の仕事でも同じ手順で取り込む（vision.md 1章）。
- Mac mini のポートを公開しない。Linear との通信は Mac mini から外へ向かう HTTPS だけで行う（requirements 2.2, 6.6, 15.2）。
- 外部から来る Issue 本文を信頼済みの命令として扱わない（requirements 15.1）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | Linear GraphQL API の照会結果（照会先の Team の `ai-ready` 付き Issue の一覧と、前回あったのに今回の一覧から消えた Issue の個別の取得結果）、振り分けの表（routes）、全体設定（settings）、本人の振り分けの判断（[`13-management-ui.md`](./13-management-ui.md) 3.4） |
| 出力 | linear_issue_states、tasks（`source=linear`）、executions、route_proposals、owner_decisions（kind=`route`）、linear_issue_links、audit_logs、[`16-linear-feedback.md`](./16-linear-feedback.md) へのコメント・状態の変更・ラベルの追加の要求（linear_outbox） |

テーブルの定義は [`02-data-model.md`](./02-data-model.md) 3.3・3.4・3.7 にあり、linear_issue_states と linear_issue_links の列は本書 3.8 で定める。

## 3. 処理詳細

### 3.1 取得の方式（requirements 6.6、Q42, Q43）

Evaloom は Webhook を使わず、Control Plane の中の Linear Poller が Linear GraphQL API へ一定間隔で照会する。

- 外から Mac mini に届く住所（公開 HTTPS エンドポイント、トンネル）を持たない。Webhook の登録、署名シークレット、署名の検証も無い。
- Linear への接続は Control Plane から外へ向かう HTTPS だけで行う。認証は API キー（requirements 6.6）。キーは `~/.config/evaloom/linear-api-key`（権限 0600）に置き、Control Plane だけが読む（Q49、ファイル名は 2026-11-07 決定）。この鍵はスペシャルハーネスに渡す鍵の保管（[`01-architecture.md`](./01-architecture.md) 3.5）には入れない。
- 照会の間隔は1分とする案（Q43）。Linear の変化は最大でこの間隔だけ遅れて取り込まれる。仕事の開始が1分遅れても困らないため、この遅れを許容する（requirements 6.6）。
- 照会は1本ずつ行う。前の照会が終わっていなければ次の照会を始めない。
- Control Plane の起動時に1回照会し、その後は間隔ごとに照会する。Mac mini が止まっていた間に付いた `ai-ready` は、再開後の最初の照会で取り込む。
- Linear API のレート制限（要求数と複雑さの上限）は実装時に Linear の公式文書で確かめ、1分間隔の照会が収まることを確かめる。収まらなければ間隔を延ばす（requirements 21章）。

### 3.2 照会の対象

1回の照会で、次の条件に当てはまる Issue をすべて取得する。

- Team が、全体設定 `linear.teamId` の Team である。照会先は1つだけで、Issue はずっと同じ Team に登録する（本人決定 2）。
- ラベルに `ai-ready`（全体設定 `linear.triggerLabel`、Q48）が付いている。
- アーカイブされていない。

取得する項目は、Issue ID、識別子（例 `ENG-123`）、URL、タイトル、本文、Team ID、ラベル名の一覧、状態の種類（キャンセルかどうか）、更新時刻（updatedAt）である。取得した内容をそのまま判定に使う。

- 照会の結果は複数のページに分かれうる。最後のページまで読み切ってから判定に進む。途中で失敗したら、その回の照会は判定に使わずに捨てる（3.4 の差分を、欠けた一覧から計算しないため）。
- 照会では Issue だけを読み、コメントと添付は読まない（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.5）。
- `linear.teamId` が設定されていないときは照会しない。
- Team は照会先を決めるためだけに使い、振り分けには使わない（本人決定 2）。

### 3.3 前回の照会結果の持ち方（linear_issue_states）

ポーリングでは、Linear から「ラベルが付いた」という出来事は届かず、照会した時点の状態だけが分かる。そのため Evaloom は、前回の照会で `ai-ready` が付いていた Issue を linear_issue_states（3.8）に記録しておき、今回の照会結果と比べて付与と取り外しを判定する。

- linear_issue_states は Issue ごとに1行で、`ai_ready`（前回の照会で付いていたか）、`attach_seq`（付与を検出した回数）、最後に見た更新時刻、削除済みの印（`removed_at`）を持つ。
- Issue が削除・アーカイブされても行を消さない。削除済みの印を付け、`attach_seq` を保つ（3.4、Q78）。
- 起動したかどうか、起動したタスクが終わったかどうかは linear_issue_links と tasks で判定する。linear_issue_states は「前回どう見えたか」だけを持つ。

### 3.4 付与・取り外しの判定（requirements 6.2）

照会を読み切ったら、照会の結果（今回の一覧）と linear_issue_states（前回の記録）を Issue ごとに比べる。

| 前回の記録 | 今回の一覧 | 判定 | 処理 |
|---|---|---|---|
| 行が無い、または `ai_ready=0` で削除済みの印が無い | ある（キャンセル以外） | 付与（起動） | attach_seq を1増やし、3.6 |
| 行が無い、または `ai_ready=0` で削除済みの印が無い | ある（状態がキャンセル） | 何もしない（案） | 行を `ai_ready=1` にするだけで起動しない |
| 削除済みの印がある（`removed_at` が入っている） | ある（状態を問わない） | 元に戻された（起動しない、Q78） | 行を `ai_ready=1` にし、`removed_at` を NULL に戻す。attach_seq は変えない。last_decision=`skipped_restored` |
| `ai_ready=1` | ある（キャンセル以外） | 付いたまま（requirements 6.2「再び起動しない」） | 何もしない。振り分けを待つタスクか保留中のタスクがあれば 3.6.3 |
| `ai_ready=1` | ある（状態がキャンセル） | 中止 | 3.7（理由 `linear_issue_cancelled`） |
| `ai_ready=1` | 無い | 消えた理由を確かめる | 下の「一覧から消えた Issue」 |

一覧から消えた Issue は、Issue ID で1件ずつ取り直して理由を見分ける。

| 取り直した結果 | 判定 | 処理 |
|---|---|---|
| `ai-ready` が外れていた | 取り外し（中止） | 3.7（理由 `linear_label_removed`）。行を `ai_ready=0` にする |
| 状態がキャンセルになっていた | 中止 | 3.7（理由 `linear_issue_cancelled`） |
| 削除・アーカイブされていた | 中止（案） | 3.7（理由 `linear_issue_removed`）。行を消さず、`ai_ready=0` にして `removed_at` に検出した時刻を入れる。attach_seq は保つ（Q78） |
| `linear.teamId` と違う Team へ移っていた | 中止（案、Q67） | 3.7（理由 `linear_issue_moved`）。行を `ai_ready=0` にする |
| 取り直しに失敗した | 決めない | 行を変えず、次の照会で再び確かめる |

- 判定は「前回付いていなかったものが今回付いている」ことだけを見る。`ai-ready` が付いたまま本文、ラベル、状態が変わっても起動しない（requirements 6.2）。例外は、振り分けを待つタスクと保留中のタスクの判定のやり直し（3.6.3）だけである。
- 照会と照会の間（案では1分の間）に付けて外した、または外して付け直した変化は、Evaloom からは見えない。前者は何も起きず、後者は「付いたまま」として扱う（案、Q65）。
- 最初の照会では前回の記録が無い。このとき、すでに `ai-ready` が付いている Issue をすべて「付与」とみなすと、導入前から付いている Issue を一斉に起動してしまう。そのため照会先の Team の最初の照会では、一覧の Issue を `ai_ready=1` として記録するだけで起動しない（案、Q64）。最初の照会を終えたかどうかは linear_poll_teams（[`02-data-model.md`](./02-data-model.md) 3.7）で持つ。本人がこれらの Issue を起動したいときは、`ai-ready` を外して付け直す。
- Mac mini が止まっていた間に付いた `ai-ready` は、再開後の照会で「前回は付いていない、今回は付いている」になるため、通常の付与として起動する。止まっていた間に外された `ai-ready` も、同じく取り外しとして中止する。
- 削除・アーカイブされた Issue が元に戻され、`ai-ready` が付いたまま一覧に再び現れても起動しない（2026-11-08 決定、Q78）。Linear が自動でアーカイブするのは完了・キャンセル済みの Issue で、Evaloom は `ai-ready` を外さない（Q51）。そのため、終わった Issue を戻しただけで同じ仕事をやり直さないようにする。最初の照会で起動しない扱い（Q64）とも揃える。行は `ai_ready=1` として記録するだけにする。
- 元に戻した Issue をもう一度実行したいときは、本人が `ai-ready` を外して付け直す。外したことを照会で検出すると行が `ai_ready=0` になり、付け直しを検出すると attach_seq が1増える。冪等キー（3.5）が以前のタスクと変わるため、新しいタスクを作る。attach_seq を保つのは、行を消して attach_seq が 0 に戻ると、付け直しても以前のタスクと同じ冪等キーになり何も起きないためである。
- Evaloom は `ai-ready` が付いている Issue だけを一覧で読む。そのため、本人が Issue を元に戻してから Evaloom が照会するまでの間に `ai-ready` を外した場合、Evaloom は元に戻されたことも外したことも見ていない。後で `ai-ready` を付け直すと、Evaloom から見えるのは「削除済みの印がある Issue が一覧に現れた」ことだけになるため、元に戻された場合と同じく起動しない（案、Q79）。本人は、Evaloom が一覧に現れたことを記録した後（`GET /api/linear/status` に出た後）に、もう一度外して付け直す。
- 照会先、起動の合図のラベル名、振り分けのラベルの接頭辞は全体設定（`linear.teamId`、`linear.triggerLabel`、`linear.routingLabelPrefix`、[`02-data-model.md`](./02-data-model.md) 3.8）で持つ。既定値は `ai-ready` と `route:`（Q48、Q120）。値は DB の settings に保存し、Control Plane を再起動しても保つ（2026-11-08 決定、Q76）。Linear Poller は照会の回の始めに設定と振り分けの表を1回読み、その回の一覧の取得、取り外しの判定、振り分けに同じ値を使う。設定を変えると次の回から効く。

### 3.5 重複排除（requirements 6.3, 15.2）

重複排除の対象は「同じ付与から二重にタスクを作ること」である。次の3つで防ぐ。

1. **1回の照会の判定と書き込みを1つのトランザクションにする**: Issue ごとに、linear_issue_states の更新と、3.6 のタスク作成（または 3.7 の中止）を同じトランザクションで行う。途中で止まっても、次の照会では前回の記録が変わっていないため、同じ判定をやり直す。
2. **冪等キーを付与の回数で決める**: tasks.idempotency_key を `linear:<Issue ID>:<attach_seq>` とする（案）。同じ付与からタスクを2回作ろうとすると、一意制約で弾かれる。
3. **同じ Issue に終端でないタスクは同時に1件まで**: 付与を検出しても、linear_issue_links で引いたタスクのうち終端状態でないものがあれば、新しいタスクを作らない（Q46）。linear_issue_states の last_decision に `skipped_active_task` を残す。

照会は1本ずつ行う（3.1）ため、2つの照会が同じ Issue を同時に判定することは無い。

### 3.6 振り分けとタスク作成（requirements 6.3）

付与を検出した Issue について、次の手順でタスクを作る。手順 1〜7 と linear_issue_states の更新は、1つのトランザクションで行う（3.5）。

1. タスクを作る。tasks.source=`linear`、title は Issue のタイトル、body は Issue の本文、idempotency_key は `linear:<Issue ID>:<attach_seq>`（3.5）。linear_issue_links に Issue とタスクの対応を書く。
2. Issue のラベルのうち、`linear.routingLabelPrefix` で始まるものを振り分けのラベルとして集める。`linear.triggerLabel` は振り分けのラベルとして数えない。
3. 振り分けの表（routes）を引いて判定する。

| 状況 | 処理 | route_proposals.trigger |
|---|---|---|
| 振り分けのラベルがちょうど1つで、routes にある | 振り分け先が決まる。手順 4 へ | - |
| 振り分けのラベルが1つも無い | 振り分けの提案に回す（3.6.4） | `no_label` |
| 振り分けのラベルが routes に無い | 振り分けの提案に回す（3.6.4） | `unknown_label` |
| 振り分けのラベルが2つ以上 | 振り分けの提案に回す（3.6.4） | `multiple_labels` |

4. 振り分け先をタスクに書く（routes の project_id か harness_id）。振り分け先の Profile を決める。Project なら projects.profile_id、スペシャルハーネスなら harnesses.default_profile_id である。スペシャルハーネスへ直接振り分けて default_profile_id が無ければ、保留にする（保留の理由 `config_missing_profile`、3.6.2）。
5. 受け入れ条件を本文から取り出す（3.6.1）。振り分け先の Profile が `acceptanceCriteria: required` と宣言していて、取り出せなければ保留にし（保留の理由 `missing_acceptance_criteria`）、Linear へ受け入れ条件の記入を求めるコメントを返す（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.2）。このときタスクの acceptance_criteria は空文字列にする（2026-11-08 決定）。`optional` と宣言した Profile では、取り出せなくても空文字列のまま進める（契約 5.2）。
6. すべて通ったら、手動作成（[`04-task-lifecycle.md`](./04-task-lifecycle.md)）と同じ手順で Execution（seq=1、mode=`continue`、`pending`）を作り、タスクを `queued` にする。linear_issue_states の last_decision は `started` にする。
7. 保留にしたときは、タスクを `blocked`、tasks.intake_block_cause に理由を入れ、Execution は作らない。last_decision は `held` にする。振り分けの提案に回したときは、タスクを `routing` にし、last_decision は `routing` にする。

- 振り分け先は、タスクを `queued` にした時点で決まり、その後は変えない（[`02-data-model.md`](./02-data-model.md) 3.3 tasks）。`queued` にした後に Issue の振り分けのラベルが変わっても、そのタスクには反映しない。
- タスク分類（requirements 8.2 工程6）は行わない。仕事の種類はスペシャルハーネスが決める。Collaboration Mode は Solo に固定する（requirements 8.2）。
- ブランチ名など、タスクから名前を作る方法はスペシャルハーネスが決める（[`02-data-model.md`](./02-data-model.md) 3.3 tasks）。Evaloom は短い名前を作らない。

#### 3.6.1 受け入れ条件の取り出し（Q44）

- 本文（Markdown）のうち、見出しの文言が「受け入れ条件」または「Acceptance Criteria」（大文字小文字を区別しない）の見出しから、次の同じ階層以上の見出しまでを受け入れ条件とする。
- 取り出した文が空白だけなら「書かれていない」とみなす（手動作成の検証と同じ、[`04-task-lifecycle.md`](./04-task-lifecycle.md)）。
- 本文全体は tasks.body に、取り出した部分は tasks.acceptance_criteria に入れる。
- 受け入れ条件が要るかどうかは、振り分け先の Profile の宣言（`acceptanceCriteria`）で決める（契約 5.2、vision-alignment.md 5章）。振り分け先が決まるまでは判定しない（Q157）。

#### 3.6.2 保留の理由

保留の理由の値は [`02-data-model.md`](./02-data-model.md) 5.7 の表に従う（`missing_acceptance_criteria` / `config_missing_profile` の2つ）。振り分けが決まらない場合は保留ではなく、振り分けの提案に回す（3.6.4）。

#### 3.6.3 判定のやり直し（Q45、Q81）

振り分けを待つタスク（`routing`）と保留したタスク（tasks.intake_block_cause が入っているタスク）は、次の契機で 3.6 の判定をやり直す。判定のやり直しは新しいタスクを作らない。同じタスクを進められるかを見直すだけである。

**Issue の更新による判定のやり直し**: 照会で取得した更新時刻が linear_issue_states に記録した値より新しければ、判定をやり直す。

- 振り分けを待つタスク: 振り分けのラベルがちょうど1つで routes にあるようになっていれば、本人が Linear でラベルを付けたものとして、普通の振り分けとして扱う（本人決定 3）。開いている振り分けの提案を `superseded`、判断待ちを `withdrawn`（理由 `superseded`）にして、3.6 の手順 4 から進める。まだ決まらなければ何もしない。提案を作り直さない（Q159）。
- 保留したタスク: 判定に通れば、そのタスクに受け入れ条件を書き、Execution を作って `queued` にする。保留の理由が変わらなければ何もしない（同じ理由のコメントを2回出さない。[`16-linear-feedback.md`](./16-linear-feedback.md) 3.2）。理由が変わったときは、新しい理由で保留し直す。
- この判定のやり直しは「`ai-ready` が付いたまま更新されても再び起動しない」（requirements 6.2）の例外になる。起動済みのタスクを二重に起動するのではなく、まだ振り分けか起動ができていないタスクを進めるためである。
- 本人は管理画面からこのタスクを中止できる（[`04-task-lifecycle.md`](./04-task-lifecycle.md) の中止）。

**Evaloom 側の設定の変更による判定のやり直し（2026-11-08 決定、Q81 を本人決定 2 に合わせて書き換え）**: 振り分けと保留の結果には、Issue ではなく Evaloom 側の設定で決まるものがある。本人が管理画面でその設定を直したときも、Issue を更新しなくても判定をやり直す。

| 本人が変えた設定 | 判定をやり直す対象 |
|---|---|
| 振り分けの表（`POST /api/routes`、`PATCH /api/routes/{label}`、`DELETE /api/routes/{label}`） | 振り分けを待つタスクすべて |
| スペシャルハーネスの既定の版（`PATCH /api/harnesses/{id}` の `defaultProfileId`） | そのスペシャルハーネスへ直接振り分けて、保留の理由が `config_missing_profile` または `missing_acceptance_criteria` のタスク |
| Project の固定する版（`PATCH /api/projects/{id}` の `profileId`） | その Project に振り分けて、保留の理由が `missing_acceptance_criteria` のタスク（新しい版が受け入れ条件を要らないと宣言したときに進めるため） |

- 設定を変える API は、設定の書き込みと同じトランザクションで、対象のタスクの Issue の linear_issue_states に判定のやり直しの印（`recheck_requested_at`、3.8）を付ける。値が変わらない更新では印を付けない。
- 次の照会で、印の付いた Issue は更新時刻が進んでいなくても判定をやり直す。Issue のラベルと本文は、その回の照会で取得した値を使う（linear_issue_states は Issue の中身を持たないため）。判定の結果の扱いは更新時刻による判定のやり直しと同じである。
- 印は判定のやり直しと同じトランザクションで消す。その回の照会でその Issue が一覧に無かった（`ai-ready` が外れた、削除・アーカイブされた）ときは、3.4・3.7 の判定に従い、印も消す。
- 照会が失敗した回は判定をしない（6章）ため、印は残り、次に照会が成功した回で判定をやり直す。
- 振り分けの表を変えても、判定の時点で決まらないタスクは振り分けを待ったままで、開いている判断待ちはそのまま残す。本人は管理画面で判断できる。

#### 3.6.4 振り分けの提案に回す

ラベルで振り分け先が決まらないタスクは、保留ではなく振り分けの提案に回す（vision.md 3章）。提案の行の形と状態は [`02-data-model.md`](./02-data-model.md) 3.4 route_proposals に従う。

1. route_proposals に行を作る（status=`queued`、trigger は 3.6 の表の値、seen_labels は判定したときの振り分けのラベル、candidates は判定の時点の振り分けの表の全行と、振り分け先の種類・ID・Profile の description）。
2. 全体設定 `routing.proposer` の model が null のときは、提案を作らない。route_proposals の行は Runner に渡さずに `failed` にする（error_class は NULL）。
3. それ以外は、全体設定 `routing.defaultRunnerId` の Runner が claim で提案の仕事を受け取り（`job.kind=route_proposal`、[`01-architecture.md`](./01-architecture.md) 5.2）、鍵の無い `proposal` の隔離で AI に提案を作らせる（Q111）。AI に渡すのは、Issue のタイトルと本文（外から来た文として区切る、3.6.5）と candidates だけである。
4. AI は結果を `out/proposal.json` に書く（形は Q202）。上限時間は Q201。Runner は結果を `POST /api/route-proposals/{id}/result` で返す。選んだラベルが candidates に無い、形が合わないときは `failed`（`contract_error`）にする。隔離の失敗は `sandbox_error`、上限時間を超えたら `timeout` にする。
5. 提案が `proposed` になっても `failed` になっても、owner_decisions（kind=`route`、subject_id=提案の ID、basis=提案の ID）を作り、Linear に「管理画面で判断待ち」とリンクを返す（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.2）。`failed` のときは提案なしで、本人が候補から選ぶ。
6. 本人が管理画面で承認するまで、タスクは `routing` のままで実行しない。AI は提案するだけで、承認なしに実行を始めない（vision.md 3章）。

本人の判断の後は次のとおり扱う（回答の形は [`02-data-model.md`](./02-data-model.md) 5.6）。

| 回答 | 処理 |
|---|---|
| 承認（`approve`、`label`） | label は判断の時点の routes にあるラベルでなければならない（提案と別のラベルを選べる、Q112）。提案を `decided` にし、3.6 の手順 4 から進める。同じトランザクションで、Linear に振り分けのラベルを付ける行（linear_outbox action=`add_label`、kind=`route_approved`）を書く（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.6.5） |
| 却下（`reject`） | タスクを `cancelled` にする（Q117）。もう一度実行したいときは、本人が Linear で振り分けのラベルを付けて `ai-ready` を外して付け直す |

- 提案を待つ間に、本人が Linear で振り分けのラベルを1つだけ付けて決まったら、3.6.3 のとおり普通の振り分けとして扱う。Linear でラベルを付けることは承認ではなく、振り分けそのものである（本人決定 3）。
- 振り分けの提案の結果が、判断待ちが閉じた後（タスクの中止、普通の振り分けで置き換え）に届いたら、記録だけ残してタスクには使わない。

#### 3.6.5 Issue 本文の扱い（requirements 15.1）

- Issue のタイトルと本文は外部入力であり、信頼済みの命令として扱わない。スペシャルハーネスへは要求 JSON の `task` として渡し（契約 6.6）、作業の段では Task Context として区切って渡す（[`07-context.md`](./07-context.md)）。振り分けの提案の AI にも、タスクの内容として区切って渡す。この扱いは手動作成のタスクと変えない。
- 振り分けの提案の AI が本文に引きずられて誤った振り分け先を選んでも、本人の承認で止められる。提案の隔離には鍵も、モデルの API のほかの外への接続も無い（[`01-architecture.md`](./01-architecture.md) 3.1）。
- Issue のコメントと添付は取り込まない（案）。
- タスクの title / body / acceptance_criteria はタスク作成の時点で固定し、その後に Issue が編集されても反映しない（案、Q46）。ただし保留の理由が `missing_acceptance_criteria` のタスクは、判定のやり直し（3.6.3）で、その回の照会で取得した本文から受け入れ条件を取り出し直す。

### 3.7 中止（requirements 6.2）

`ai-ready` が外されたとき、または Issue がキャンセルされたとき、その Issue に対応する終端でないタスクを中止する。3.4 の表で中止とした場合も同じに扱う。

- 対象は linear_issue_links で引いたタスクのうち、状態が `received` / `routing` / `queued` / `running` / `waiting_approval` / `blocked`（保留を含む）のもの。手順は本人の中止（[`04-task-lifecycle.md`](./04-task-lifecycle.md)）と同じにし、audit_logs の actor は `system`、理由は 3.4 の表の値（`linear_label_removed` / `linear_issue_cancelled` / `linear_issue_removed` / `linear_issue_moved`）とする。
- 開いている判断待ちは `withdrawn`（理由 `cancelled`）にする。`waiting_approval` のタスクの未使用の承認は無効にする（[`17-approval.md`](./17-approval.md) 3.6）。
- 終端状態のタスクには何もしない。
- Issue の削除・アーカイブによる中止でも、linear_issue_states の行は消さない。削除済みの印（`removed_at`）を付け、attach_seq を保つ（3.4、Q78）。元に戻されて一覧に再び現れても、この中止を取り消さず、新しいタスクも作らない。
- 中止したことは Linear にコメントで返す（[`16-linear-feedback.md`](./16-linear-feedback.md)）。Issue が削除・アーカイブされていた場合は、送信が失敗として記録される（16 の 6章）。

### 3.8 データモデル

共通の約束（ID、時刻、JSON 列）は [`02-data-model.md`](./02-data-model.md) 3.1 に従う。tasks の source・intake_block_cause、linear_poll_teams、linear_outbox、settings は 02 で定める。この節では linear_issue_states と linear_issue_links の列を定める（[`02-data-model.md`](./02-data-model.md) 3.7）。

requirements 17章の event_inbox（受信イベント）は作らない（Q66）。前回の照会結果は linear_issue_states が持ち、判定の結果は linear_issue_states と audit_logs に残す。projects には Linear の列を持たない（本人決定 2・6）。

#### linear_issue_states

前回の照会で見えた Issue の状態を持ち、3.4 の差分の判定に使う。

| 列 | 型 | 説明 |
|---|---|---|
| linear_issue_id | TEXT PK | Linear の Issue ID |
| issue_identifier | TEXT | 表示用の識別子（例 `ENG-123`） |
| team_id | TEXT | 最後に見た Team ID |
| ai_ready | INTEGER | 1 = 前回の照会で `ai-ready` が付いていた、0 = 付いていなかった |
| attach_seq | INTEGER | 付与を検出した回数。3.5 の冪等キーに使う |
| issue_updated_at | TEXT | 最後に見た Linear の更新時刻（3.6.3 の判定のやり直しに使う） |
| last_decision | TEXT NULL | 最後の判定（`started` / `routing` / `held` / `cancelled` / `skipped_active_task` / `skipped_initial` / `skipped_cancelled` / `skipped_restored`） |
| last_decision_at | TEXT NULL | |
| first_seen_at / last_seen_at | TEXT | |
| removed_at | TEXT NULL | 削除・アーカイブを検出した時刻（削除済みの印）。元に戻されて一覧に再び現れたら NULL に戻す（3.4、Q78） |
| recheck_requested_at | TEXT NULL | Evaloom 側の設定の変更で、判定のやり直しを求めた時刻（3.6.3、Q81）。次の照会で判定をやり直したら NULL に戻す |

- `ai_ready=0` の行は、中止の後に付け直しを検出するために残す（Q46）。保存期間は Q24 と合わせて決める。
- 削除・アーカイブされた Issue の行も消さない。attach_seq を保ち、元に戻された後の付け直しで以前のタスクと同じ冪等キーにならないようにする（Q78）。

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

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 振り分けのキー | 振り分けのラベルだけ。Linear Team と Linear Project は使わない | 本人決定 2 |
| 照会先 | 全体設定 `linear.teamId` の Team 1つ | 本人決定 2 |
| 振り分けのラベルが無い・知らない・2つ以上 | 保留にせず、AI の提案と本人の承認（管理画面）に回す | vision.md 3章 |
| 振り分けの承認の場所 | 管理画面だけ。Linear でラベルを付けたときは普通の振り分けとして扱う | 本人決定 3 |
| 自動実行の契機 | `ai-ready` が付いた時点。付いたままの更新では再び起動しない | requirements 6.2 |
| 中止の契機 | `ai-ready` が外された、Issue がキャンセルされた | requirements 6.2 |
| 保留 | 受け入れ条件が要る Profile で受け入れ条件が無い、直接振り分けたスペシャルハーネスに既定の版が無い | [`02-data-model.md`](./02-data-model.md) 5.7 |
| 受け入れ条件の要否 | 振り分け先の Profile の宣言（`acceptanceCriteria`） | 契約 5.2 |
| 取得の方式 | Linear API への定期照会（ポーリング）。Webhook は使わない | requirements 6.6 |
| 受信口 | 持たない。Mac mini のポートを公開しない | requirements 2.2, 6.6, 15.2 |
| 照会の間隔 | 1分（案、Q43） | - |
| イベント重複排除 | 必須 | requirements 6.3, 15.2 |
| Issue 本文 | 信頼済みの命令として扱わない | requirements 15.1 |
| タスク分類 | 行わない。仕事の種類はスペシャルハーネスが決める | requirements 8.2、vision.md 4章 |
| 扱う仕事 | 振り分けの表にある振り分け先の仕事すべて。仕事の種類で絞らない | vision.md 1章 |
| 同時実行の上限 | 全体2本、振り分け先ごとに1本（Linear 起因のタスクも同じ） | requirements 8.7、[`02-data-model.md`](./02-data-model.md) 4章 |
| 自動リトライ | 0回 | requirements 8.3 |

## 5. インターフェース

| Method | Endpoint | 認証区分 | 内容 |
|---|---|---|---|
| GET | /api/linear/status | 本人用 | 照会の状況（照会先の Team、最後に成功した時刻、失敗の分類、続けて失敗した回数）と、`ai-ready` が付いている Issue の一覧と最後の判定。管理画面で取り込みの状況を確かめるため |
| GET / POST / PATCH / DELETE | /api/routes | 本人用 | 振り分けの表（[`01-architecture.md`](./01-architecture.md) 5.1）。変更は 3.6.3 の判定のやり直しの印を付ける |
| GET / PATCH | /api/settings | 本人用 | 全体設定の `linear.teamId` / `linear.triggerLabel` / `linear.routingLabelPrefix` / `routing.proposer` / `routing.defaultRunnerId`（[`02-data-model.md`](./02-data-model.md) 3.8）。値は settings に保存し、再起動後も保つ（Q76） |
| GET / POST | /api/decisions、/api/decisions/{id}/answer | 本人用 | 振り分けの判断（kind=`route`）を含む本人の判断待ち（[`01-architecture.md`](./01-architecture.md) 5.1、[`02-data-model.md`](./02-data-model.md) 5.6） |
| POST | /api/runners/{id}/claim | Runner 用 | 応答の `job.kind=route_proposal` で振り分けの提案の仕事を渡す |
| POST | /api/route-proposals/{id}/artifacts、/api/route-proposals/{id}/result | Runner 用 | 振り分けの提案のログと結果（選んだラベル、理由、エラー分類） |

- 応答の形とエラー形式は [`01-architecture.md`](./01-architecture.md) 5.3 に従う。
- `PATCH /api/settings` は指定したキーだけを置き換え、省略したキーは残す。[`02-data-model.md`](./02-data-model.md) 3.8 の表に無いキーは 400 にする。Linear のキーの値が null・空文字列・空白だけのときは 400（`<キー>` / `required`）にする。1つでも拒否したら何も変えず、settings にも audit_logs にも書かない（Q76）。
- 値を変えたら、変わったキーごとに audit_logs へ1行書く（actor `owner`、action `settings.update`、target_type `settings`、target_id はキー、detail は変更前後の値）。settings の更新と audit_logs の書き込みは1つのトランザクションで行う（Q76）。
- `linear.routingLabelPrefix` と `linear.triggerLabel` の変更は、Q160 の検証に従う。
- タスクの API（`GET /api/tasks` 等）の応答に `source`、`intakeBlockCause`、`linear`（識別子と URL）を足す。
- Linear から Evaloom へ届く要求は無い。外から呼ばれる API は追加しない。

Linear GraphQL API で使う操作（照会先の Team の `ai-ready` 付き Issue の一覧、Issue の個別取得、ラベルと Team の取得）は Control Plane の Linear クライアントにまとめる。認証は API キー（requirements 6.6、Q49）。

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| 照会の途中（ページの途中を含む）で Linear API が失敗 | その回の照会を捨て、判定もしない。linear_poll_teams の last_error と consecutive_failures を更新し、次の間隔で照会し直す |
| 5回続けて失敗した（1分おきの照会で5分にあたる。2026-11-08 決定） | Dashboard のエラー状況に出す。5回目の失敗の分類が `auth` 以外のときだけ、macOS 通知で本人に知らせる。5回目の失敗の分類が `auth` のときはこの通知を出さず、下の認証エラーの共通の抑止だけで扱う（2026-11-08 決定、Q86）。照会は続ける |
| API キーのファイルが無い・読めない・権限が 0600 でない | 照会を始めない。照会先の Team の last_error を `auth` にし、Dashboard の取り込み状況に出す。理由（ファイルが無い・権限が違う）は値を含めずにログにも出す（2026-11-08 決定）。認証エラーとして扱い、下の Linear API の認証エラーと同じく共通の抑止に従って macOS 通知を出す（2026-11-08 決定、Q86） |
| `linear.teamId` が設定されていない | 照会しない。Dashboard の取り込み状況に「照会先が未設定」と出す |
| settings に保存した値が空・空白だけ、または読めない | そのキーは既定値を使い、Control Plane のログに出す（Q76）。`linear.teamId` は既定値が無いため照会しない |
| Linear API の認証エラー | last_error=`auth`。API キーの値はログに出さない。macOS 通知（[`12-notification.md`](./12-notification.md) 3.5 の Dashboard を開く通知）で本人に知らせる。通知の抑止は送信側（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.4.3 の送信 Worker）と共通の1つで、照会と送信の両方が認証エラーになっても通知は1回だけ出し、照会か送信のどちらかが1回成功したら解く（2026-11-08 決定、Q84）。照会は間隔ごとに続ける |
| レート制限に掛かった | last_error=`rate_limited`。Linear が返す再開の目安まで照会を止める |
| 一覧から消えた Issue の取り直しに失敗 | その Issue の行を変えず、次の照会で再び確かめる。ほかの Issue の判定は進める |
| タスク作成のトランザクションが失敗 | その Issue の linear_issue_states を変えない。次の照会で同じ判定をやり直す（3.5） |
| 振り分けが決まらない | 振り分けの提案に回す（3.6.4）。エラーとしては扱わない |
| 受け入れ条件・既定の版で保留 | タスクを保留にし、受け入れ条件のときはコメントを要求する。エラーとしては扱わない |
| 振り分けの提案の失敗（`sandbox_error` / `timeout` / `contract_error`） | 提案を `failed` にし、提案なしの判断待ちを作る（3.6.4）。本人が候補から選ぶ |
| `routing.defaultRunnerId` が未設定・Runner が offline | 提案は `queued` のまま待つ。Dashboard に、振り分けの提案を動かす Runner が無いことを出す |
| 同じ Issue に終端状態でないタスクがすでにある状態で付与を検出 | 新しいタスクを作らない。last_decision=`skipped_active_task` |

## 7. スコープ外

- Linear Webhook の受信、公開 HTTPS エンドポイント、トンネル、署名の検証（Q42 で取り消し）
- Linear Team と Linear Project を使った振り分け（本人決定 2、Q47）
- 複数の Team の照会（本人決定 2）
- Issue のコメント・添付の取り込み
- Linear での振り分けの承認（コメントへの返信など）。本人の判断は管理画面だけで受け付ける（本人決定 3）
- Gmail / GitHub など Linear 以外の入口（定期実行などは後で足す、vision.md 3章）
- タスク分類・task_steps（requirements 8.2）

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約（受け入れ条件の要否、外から来た文）
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API と認証区分、Proposal Sandbox
- [`02-data-model.md`](./02-data-model.md) tasks、routes、route_proposals、owner_decisions、Linear の表、settings
- [`03-registry.md`](./03-registry.md) Project と振り分けの表の登録
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) タスク作成、中止、本人の判断の後の再開
- [`07-context.md`](./07-context.md) Task Context の扱い
- [`13-management-ui.md`](./13-management-ui.md) 本人の判断待ちの画面、振り分けの表の画面
- [`16-linear-feedback.md`](./16-linear-feedback.md) Linear への結果返却と振り分けのラベルの追加
- [`17-approval.md`](./17-approval.md) 承認

## 9. 実装単位

- [ ] （人間） Evaloom 専用の Linear ユーザーの API キーを作って Mac mini の `~/.config/evaloom/linear-api-key`（権限 0600）に置く（Q49）。管理画面で `linear.teamId` を設定する
- [ ] [Backend] Linear GraphQL クライアント（API キーの読み込み、照会先の Team の ai-ready 付き Issue の一覧をページを読み切って取得、Issue の個別取得）
  - 受け入れ基準: テスト用のサーバーに対して、複数ページに分かれた一覧を最後まで取得できる; ページの途中で失敗させると、一覧を返さずにエラーになる; API キーの値がログと DB に現れない; API キーのファイルが無い・権限が 0600 でないときは照会を始めず理由を記録する; 一覧の要求の Team が linear.teamId の1つだけになる
- [ ] [Backend] Linear Poller（起動時と1分ごとの照会、1本ずつの実行、成功・失敗の記録、レート制限で待つ）
  - 受け入れ基準: Control Plane を起動すると1回照会し、その後は設定した間隔で照会する; 照会が間隔より長く掛かっても、2本目が同時に始まらない; 失敗すると linear_poll_teams の last_error と consecutive_failures が更新され、成功で戻る; linear.teamId が未設定のときは照会しない; 5回続けて失敗すると Dashboard のエラー状況に出て macOS 通知が1回出る; API キーのファイルが無い・権限が 0600 でないとき、照会先の Team の last_error が auth になる
- [ ] [Backend] 付与・取り外しの判定（3.4 の2つの表、最初の照会の扱い、重複排除）
  - 受け入れ基準: 前回に無く今回ある Issue で起動になり attach_seq が1増える; 付いたまま本文やラベルを変えた Issue で、queued 以降のタスクに何も起きない; 一覧から消えた Issue を取り直し、ラベルが外れていれば中止、キャンセルなら中止、linear.teamId と違う Team なら中止になる; 照会先の Team の最初の照会では、一覧の Issue を記録するだけでタスクが作られない; linear.teamId を変えた後の最初の照会でも記録するだけになる; 外して付け直した Issue で、前のタスクが終端なら新しいタスクができ、終端でなければできない; 判定の途中で失敗させても、次の照会でタスクが二重にできない
- [ ] [Backend] 振り分けとタスク作成（3.6、3.6.1、3.6.2）
  - 受け入れ基準: routes にある振り分けのラベルがちょうど1つの Issue から、振り分け先の入った queued のタスクと pending の Execution ができる; Project にもスペシャルハーネスにも振り分けられる; 振り分けのラベルが無い・routes に無い・2つ以上の Issue では、routing のタスクと queued の route_proposals ができ、Execution と保留の理由はできない; ai-ready は振り分けのラベルとして数えない; required の Profile で受け入れ条件の見出しが無い Issue が missing_acceptance_criteria で保留になり、acceptance_criteria が空文字列で保存される; optional の Profile では受け入れ条件が無くても queued になる; 既定の版の無いスペシャルハーネスへ直接振り分けると config_missing_profile で保留になる; Team を振り分けの判定に使っていない
- [ ] [Backend] 振り分けの提案と本人の判断（3.6.4）
  - 受け入れ基準: 提案が proposed になると kind=route の判断待ちができ、linear_outbox に decision_open のコメントの行ができる; routing.proposer の model が null だと Runner に渡さず failed になり、提案なしの判断待ちができる; candidates に無いラベルを返した提案は failed（contract_error）になる; 本人が承認すると、タスクに振り分け先が入り queued と Execution ができ、linear_outbox に add_label（route_approved）の行ができる; 提案と別の routes のラベルで承認できる; routes に無いラベルの承認は 422 になる; basis の違う回答が 422 になる; 却下でタスクが cancelled になる; 承認まで Execution ができない
- [ ] [Backend] 判定のやり直し（3.6.3。Issue の更新と Evaloom 側の設定の変更、recheck_requested_at）
  - 受け入れ基準: 振り分けを待つタスクの Issue に routes のラベルを1つだけ付けて更新時刻が進むと、提案が superseded、判断待ちが withdrawn（superseded）になり、同じタスクが queued になる; ラベルを付けてもまだ決まらなければ提案が作り直されない; 保留中のタスクの Issue に受け入れ条件を書いて更新時刻が進むと、同じタスクが queued になり Execution が1つできる; 更新時刻が変わらない照会では判定をやり直さない; 理由が変わらなければ何も起きずコメントも増えない; 振り分けを待つタスクの Issue のラベルを routes に登録すると、Issue の更新時刻が変わらないまま次の照会で queued になる; スペシャルハーネスの既定の版を設定すると config_missing_profile のタスクが次の照会で進む; Project の版を optional の版に変えると、その Project の missing_acceptance_criteria のタスクが次の照会で queued になる; ほかの Project の版を変えても、そのタスクの Issue に印が付かない; 値が変わらない PATCH では印が付かない; 判定のやり直しの後に印が NULL に戻る; 照会が失敗した回は印が残り、次に成功した回でやり直す; 設定の書き込みがロールバックされると印も付かない
- [ ] [Backend] Linear 起因の中止（3.7）
  - 受け入れ基準: ai-ready を外した Issue の照会で routing・queued・running・waiting_approval・blocked のタスクが中止の経路に入る; 開いている判断待ちが withdrawn（cancelled）になる; Issue のキャンセル、削除・アーカイブでも同じになる; completed のタスクは変わらない; audit_logs の actor が system で、理由が 3.4 の表の値になる
- [ ] [Backend] 削除・アーカイブした Issue の記録を残す（3.4、3.7、3.8 の removed_at、Q78）
  - 受け入れ基準: 削除・アーカイブを検出すると中止の経路に入り、linear_issue_states の行が残って ai_ready=0・removed_at が入り、attach_seq が変わらない; その Issue が ai-ready 付きのまま一覧に再び現れると、行が ai_ready=1・removed_at が NULL・last_decision=skipped_restored になり、タスクも Execution も作られない; 状態がキャンセルのまま再び現れても同じになる; その後に ai-ready を外した照会で ai_ready=0 になり、付け直した照会で attach_seq が1増えて新しい冪等キーのタスクができる
- [ ] [Backend] 全体設定（linear.teamId / linear.triggerLabel / linear.routingLabelPrefix / routing.proposer / routing.defaultRunnerId）を settings に保存する（3.4、5章、Q76、Q160）
  - 受け入れ基準: PATCH /api/settings で変えた値が、Control Plane を再起動した後の GET /api/settings と Linear Poller の照会に使われる; settings に行が無いキーは既定値になる; 値を変えると変わったキーごとに audit_logs に settings.update の行が1つでき、detail に変更前後の値が入る; 同じ値で PATCH しても audit_logs に行ができない; 未知のキー、null・空文字列・空白だけの値は 400 になり、settings と audit_logs が変わらない; 2つのキーのうち1つが不正な PATCH では、正しいほうのキーも変わらない; routes のラベルが合わなくなる接頭辞の変更が 422 になる
- [ ] [Backend] `GET /api/linear/status`
  - 受け入れ基準: 照会先の Team の最後に成功した時刻と失敗の分類、ai-ready が付いている Issue と最後の判定が返る; 応答に API キーの値が含まれない
- [ ] [Frontend] Settings に照会先の Team、起動の合図のラベル名、振り分けのラベルの接頭辞、振り分けを提案する AI と Runner。Dashboard に Linear の取り込みの状況
  - 受け入れ基準: 設定を保存して表示できる; 400 と 422 の内容がフォームに表示される; 照会が続けて失敗しているとき、Dashboard のエラー状況に失敗の分類と最後に成功した時刻が出る
- [ ] [Frontend] タスク一覧・詳細での Linear 起因のタスクの表示（Issue へのリンク、振り分けを待っていること、保留の理由）
  - 受け入れ基準: source=linear のタスクに Issue の識別子とリンクが出る; routing のタスクに判断待ちへのリンクが出る; 保留中のタスクに理由が文字で表示され、続行ボタンが出ず中止だけが出る

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q157 | 受け入れ条件の要否は振り分け先の Profile で決まるため、振り分けが決まらないタスクの受け入れ条件をいつ判定するか | 振り分け先が決まった後（ラベルで決まったとき、本人が振り分けを承認したとき、Linear でラベルを付けて決まったとき）に判定する。振り分けを待つ間は判定しない。承認の後に受け入れ条件が無くて保留になったら、記入を求めるコメントを返す |
| Q158 | 全体設定 `linear.teamId` を変えたとき、前の Team の Issue と、それに対応する終端でないタスクをどう扱うか | 前の Team の Issue は照会の対象から外れる。終端でないタスクは中止せず、そのまま進める（Linear への返却も続ける）。ただし以後の `ai-ready` の取り外しとキャンセルは検出しない。前の Team の linear_issue_states と linear_poll_teams の行は消さずに残す。変更の前に、終端でないタスクがあることを管理画面で示す |
| Q159 | 振り分けを待つ間に本人が Linear でラベルを変えたが、まだ決まらない（2つ以上、知らないラベル）とき、提案を作り直すか | 作り直さない。開いている判断待ちをそのまま残し、本人が管理画面で選ぶ。提案は判定したときのラベル（seen_labels）で作ったことを画面に示す |
| Q160 | 振り分けのラベルの接頭辞と起動の合図のラベル名を変えたときの整合 | 変更の後に、routes に接頭辞で始まらないラベルが残るなら 422 で拒否する（先に振り分けの表を直す）。起動の合図のラベル名が振り分けのラベルの接頭辞で始まる値は 400 で拒否する（`ai-ready` を振り分けのラベルとして数えず、Evaloom が付けないようにするため） |
| Q201 | 振り分けの提案の上限時間（09 の 3.2 と 05 の 3.13 がこの文書に任せていたが、値が無かった。04 の 3.3.1 の claim の応答の例は 300 秒） | 300 秒の固定値（実装の定数）とする。全体設定のキーは足さない。超えたら `failed`（`timeout`）にし、提案なしの判断待ちを作る |
| Q202 | 振り分けの提案の AI が書く結果のファイルの形（08 の 3.10 がこの文書に任せていた） | `out/proposal.json` に `{ "label": "<candidates のラベル>", "reason": "<2000 文字以内>" }` だけを書かせる。64 KiB 以下の通常ファイルでなければ、またはほかの項目があれば `failed`（`contract_error`）。Runner はファイルを成果物（kind=`proposal_result`、02 の artifacts）として登録し、中身を `POST /api/route-proposals/{id}/result` で送る |
