---
status: confirmed
confirmed_rev: 59873bd
written: 2026-11-10
範囲: Linear Issue への結果の返却（コメント）、「管理画面で判断待ち」の知らせ、Issue の状態（Workflow state）の変更、本人が管理画面で承認した振り分けのラベルの追加、受け入れ条件の記入依頼、送信の再試行、Evaloom 自身の更新による再起動（循環トリガー）の防止
---

# 16. Linear 連携：結果の返却 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Linear 起因のタスクについて、結果と本人の判断待ちを Linear に知らせ、Issue の状態を変え、管理画面で判断した振り分けを Linear に書き戻す。取り込みと振り分けは [`15-linear-intake.md`](./15-linear-intake.md)

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。本人の判断はすべて管理画面で受け付け、Linear には「管理画面で判断待ち」と知らせるだけにする。管理画面で判断した結果は、Evaloom が Linear に書き戻す（振り分けのラベルを付ける、状態を変える）。Evaloom が付けてよいラベルは振り分けのラベルだけで、`ai-ready` には触らない（本人決定 3）。返す内容は PR や Gate の結果ではなく、スペシャルハーネスが報告した `outputs` と確かめる段の `summary` にする（本人決定 1）。テーブルと列挙値の名前は [`02-data-model.md`](./02-data-model.md) に従う。決定済みの点は 00-overview.md の要確認リスト（Q49、Q51、Q52、Q80、Q82〜Q86、Q96〜Q100）に載せている。

## 1. 目的

- Linear から来たタスクの結果を、元の Issue に返す（requirements 6.4）。
- 受け入れ条件の要る仕事で受け入れ条件の無い Issue に、記入を求めるコメントを返す（requirements 6.3）。
- 本人の判断が要るとき（振り分けの承認、操作の承認、止まった仕事の続行、作業中の AI の質問）を Linear にも知らせる。判断そのものは管理画面だけで受け付ける（requirements 8.8、本人決定 3）。
- タスクの進み具合に合わせて Issue の状態（Workflow state）を変え、Linear の画面で進み具合が分かるようにする（Q51 の見直し、2026-11-09 本人決定）。
- 本人が管理画面で振り分けを承認したら、Issue に振り分けのラベルを付けて、Linear から見ても振り分け先が分かるようにする（本人決定 3）。
- Evaloom 自身の Issue 更新で、同じジョブを再び起動しない（requirements 6.4, 15.2 循環トリガー防止）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | タスク・Execution の状態遷移（[`04-task-lifecycle.md`](./04-task-lifecycle.md)）、本人の判断待ちの発生と回答（owner_decisions、[`02-data-model.md`](./02-data-model.md) 3.4）、取り込みの保留（[`15-linear-intake.md`](./15-linear-intake.md) 3.6）、段の結果（契約 6.5 の `summary`、`outputs`） |
| 出力 | Linear Issue へのコメント、Issue の状態の変更、振り分けのラベルの追加、linear_outbox、`notification_sent` / `notification_failed` イベント |

## 3. 処理詳細

### 3.1 送信口

[`12-notification.md`](./12-notification.md) 3.2 の `Notifier` に、Linear への送信の実装を足す。macOS 通知と同じ契機から呼ばれ、source=`linear` のタスクにだけ送る。手動作成のタスクには送らない。

### 3.2 コメントを出す場面（Q52）

| 契機 | 出すか | linear_outbox の kind | 本文の主な内容 |
|---|---|---|---|
| タスクの作成（受付） | 出さない | - | - |
| 本人の判断待ちを作った（owner_decisions の行。kind は `route` / `approval` / `blocked` / `question` のどれでも） | 出す | `decision_open` | 「管理画面で判断待ち」と、判断待ちへのリンク（Q122）。判断の中身（提案した振り分け先、承認の対象、finding、質問の本文）は載せない |
| 保留（受け入れ条件が無い） | 出す（requirements 6.3） | `missing_acceptance_criteria` | 受け入れ条件を書く見出しの書き方（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.1） |
| 保留（直接振り分けたスペシャルハーネスに既定の版が無い） | 出さない | - | Evaloom 側の設定の不備で、Linear で直せることが無いため。管理画面の Dashboard とタスク一覧に出す |
| Execution が `succeeded`（出す段まで成功した） | 出す | `succeeded` | 3.3 の項目 |
| Execution が `failed` / `interrupted` / `timed_out` | 出す | `failed` | 3.3 の項目 |
| タスクの中止 | Linear 起因の中止でも本人の中止でも出す（案） | `cancelled` | 中止の理由 |

- 同じタスクの同じ契機について、コメントは1回だけ出す（[`12-notification.md`](./12-notification.md) 3.4 と同じ考え方）。`decision_open` は判断待ちの行ごとに1回とする（Q162）。
- 判断待ちのコメントは知らせるだけである。Linear のコメントへの返信、リアクション、ラベルの付け外しを、承認・続行・回答として扱わない（本人決定 3）。例外として、振り分けを待つ間に本人が Linear で振り分けのラベルを付けたときは、承認ではなく普通の振り分けとして扱う（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.3）。

### 3.3 本文（requirements 6.4）

| 項目 | 中身 |
|---|---|
| 実行ステータス | タスクの状態の表示名（[`02-data-model.md`](./02-data-model.md) 5.1） |
| 振り分け先と実行担当 | Project の名前（直接振り分けたときは無し）、スペシャルハーネスの ID と Profile の版 |
| 作業概要 | 作業の段の `out/summary.md`（契約 6.3）。無ければ載せない |
| 確かめた結果 | 確かめる段の結果の `summary`（check_results.summary）と、finding の件数（severity ごと） |
| 外に出したもの | 出す段が報告した `outputs`（executions.outputs）の `label` と `url` |
| エラー情報 | エラー分類（[`02-data-model.md`](./02-data-model.md) 5.5）と、機密情報を除いた `message` の要約 |
| 管理画面へのリンク | タスク詳細と、開いている判断待ちがあればその判断待ち（Q122） |

- 本文には秘密情報を入れない。ログの本文、確かめる段の出力の全文、Runner のパスも入れない（案）。Linear の Issue は本人以外のワークスペースのメンバーも読めるためである。
- 作業概要と確かめた結果の summary は、AI とスペシャルハーネスが書いた文なので、そのまま載せる前に機密情報除外（[`05-runner.md`](./05-runner.md)）と同じ処理を通す。
- `outputs` の中身は Evaloom が解釈しない不透明な値で、表示するだけである（[`02-data-model.md`](./02-data-model.md) 3.1）。
- 管理画面のリンクは、`evaloomd` の起動時に指定した Tailscale の名前の URL にする。指定が無いときは、タスクと判断待ちの ID だけを載せる（[`01-architecture.md`](./01-architecture.md) Q122）。

### 3.4 送信と再試行

1. 契機が起きたら、状態遷移と同じトランザクションで linear_outbox に1行書く（status=`pending`）。
2. Control Plane の Worker が `pending` の行を古い順に送る。Linear GraphQL API を使い、認証は Evaloom 専用ユーザーの API キー（requirements 6.6、Q49）。
3. 成功したら status=`sent`、コメントなら Linear のコメント ID を記録し、契機に Execution があれば `notification_sent` イベント（channel=`linear`）を書く。
4. 失敗したら attempts を増やし、間隔を空けて再試行する。上限は5回。間隔は1分・2分・4分・8分と倍にし、5回目も失敗したら status=`failed` にし、`notification_failed`（channel=`linear`）を記録する（最初の失敗からおよそ15分。Q80、2026-11-08 決定）。
5. 送信に失敗しても、タスクの状態遷移は取り消さない（[`12-notification.md`](./12-notification.md) 3.4 と同じ）。

コメントで送った結果が分からない（応答の前に接続が切れた）ときは、同じ dedupe_key（3.7）のコメントがすでに付いていないかを Linear から読んで確かめてから再送する。照合できるよう、コメントの本文の末尾に dedupe_key を入れる（案）（requirements 15.8 の「重複して実行する前に外部システムの状態を照合する」と同じ考え方）。照合の要否は DB に残し、再起動をまたいでも照合してから送る（3.4.1）。

#### 3.4.1 結果が分からない送信の照合（2026-11-08 決定、Q83）

送った結果が分からない状態を linear_outbox の `outcome_unknown`（3.7）に残す。プロセスの中の記憶には頼らない。再起動の後も、この列が 1 の行は照合してから送る。照合が要るのは action=`comment` の行だけである（状態の変更とラベルの追加は、同じ操作を2回送っても結果が変わらないため）。

1. Worker はコメント作成の要求を Linear へ送る前に、その行の outcome_unknown を 1 にしてコミットする。
2. 結果が分かったら、outcome_unknown を 0 に戻す。結果が分かるのは、送信が成功したとき（status=`sent` と同じトランザクション）、Linear が応答を返したうえで失敗したとき、接続を始める前に失敗したときである。
3. 次の場合は outcome_unknown が 1 のまま残る。接続した後の通信の失敗・時間切れ・応答の本文を読めなかったとき、Control Plane の停止で送信が中断したとき、送信中にプロセスが異常終了したとき。
4. outcome_unknown が 1 の行を次に送る前（再起動の後を含む）に、Issue のコメントを読み、最終行が同じ dedupe_key のコメントを探す。あれば送らずに status=`sent` にし、そのコメントの ID を linear_comment_id に記録する。無ければ 1 から送り直す。
5. コメントを読めなかったときは送らず、その回の失敗として attempts を増やし、3.4 の再試行に従う。outcome_unknown は 1 のまま残す。
6. 停止による中断は attempts を増やさない。outcome_unknown が 1 で残るため、再起動の後は照合してから送る。

#### 3.4.2 送信の失敗の記録と管理画面への表示（2026-11-08 決定、Q82）

送れなかった行は、契機に Execution があってもなくても、すべて管理画面の通知失敗の一覧（[`12-notification.md`](./12-notification.md) 5章の `GET /api/notification-failures`）に出す。

- Execution のある契機（Execution のある判断待ちの `decision_open`、`succeeded`、`failed`、Execution のあるタスクの `cancelled`、状態の変更の `started` / `waiting_decision` / `completed`）は、execution_events に `notification_failed`（channel=`linear`）を書く。一覧はこのイベントを返す。
- Execution の無い契機（振り分けの判断待ちの `decision_open`、`missing_acceptance_criteria`、振り分けを待つタスク・保留したタスクの中止の `cancelled`、振り分けのラベルの追加の `route_approved`）は execution_events に書けない。linear_outbox の行（status=`failed`、`failure_reason`、`failed_at`、3.7）を失敗の記録とし、一覧はこの行も返す。
- どちらの契機かは、linear_outbox の `execution_id`（3.7）で決める。outbox の行を書くときに契機の Execution の ID を入れ、Execution の無い契機では NULL にする。Worker は execution_id が NULL でない行だけ `notification_failed` を書く。一覧は execution_id が NULL で status=`failed` の行を足す。同じ失敗を2回出さない。
- failed にするときは、Worker が `failure_reason` に `notification_failed` の payload の reason と同じ値を、`failed_at` に failed にした時刻を書く。行の更新とイベントの書き込みは1つのトランザクションで行う。
- 一覧に linear_outbox の行が混ざるときは、記録した時刻（execution_events.created_at、linear_outbox.failed_at）の新しい順に並べ、cursor はその並びの位置を表す値にする（2026-11-08 決定、Q82。[`12-notification.md`](./12-notification.md) 5章）。

`notification_failed` の reason のうち channel=`linear` で使う値（2026-11-08 決定、Q85）は次のとおりとする。

| `reason` | 当てはまる失敗 |
|---|---|
| `auth` | Linear API の認証エラー（再試行せず failed、6章） |
| `issue_removed` | Issue が削除・アーカイブされていた（6章） |
| `retries_exhausted` | 一時的な失敗が続き、5回目も失敗した（3.4 の 4） |
| `state_not_found` | 状態の変更で、付ける名前の状態が Issue の Team に無かった（3.6.2） |
| `label_not_found` | 振り分けのラベルの追加で、付けるラベルが Linear の Team に無かった（3.6.5、Q164）。再試行しない |
| `other` | 上のどれにも当てはまらない失敗 |

- この値は [`02-data-model.md`](./02-data-model.md) 5.3 の列挙に channel=`linear` 用として載っている。[`12-notification.md`](./12-notification.md) 3.4 の分類は macOS 通知のヘルパーの失敗だけを想定しているため、Linear の失敗には使わない。
- payload にエラーの文字列を入れない（12 の 3.4）。

#### 3.4.3 認証エラーの macOS 通知（2026-11-08 決定、Q84）

Worker が Linear API の認証エラーで行を failed にしたら、macOS 通知で本人に知らせる。取り込み側の通知（[`15-linear-intake.md`](./15-linear-intake.md) 6章、[`12-notification.md`](./12-notification.md) 3.5 の Dashboard を開く通知）と同じ扱いにし、抑止は取り込み側と共通の1つを使う（2026-11-08 決定、Q84）。

- 通知は 12 の 3.5 の Dashboard を開く通知として出す。クリックで `http://127.0.0.1:<port>/` を開く。
- 本文には、Linear への送信の失敗であることと失敗の分類（`auth`）だけを入れる。API キーの値、Linear の応答の本文、Issue の識別子、コメントの本文は入れない。
- この通知は Execution を持たないため、execution_events の `notification_sent` / `notification_failed` には記録しない。通知の送信に失敗したら Control Plane のログにだけ残す（12 の 3.5）。認証エラーで failed になった行そのものは、3.4.2 のとおり通知失敗の一覧に出る。
- 抑止は「認証エラーの通知の抑止」1つを Control Plane が持ち、取り込み側の Linear Poller（[`15-linear-intake.md`](./15-linear-intake.md) 6章）と送信 Worker が共有する（2026-11-08 決定、Q84）。同じ API キーが無効になって照会と送信の両方が認証エラーになっても、通知は1回だけ出る。
  1. 照会か送信のどちらかが認証エラーになったとき、抑止が掛かっていなければ通知を1回出し、抑止を掛ける。抑止が掛かっているときは出さない。
  2. 通知は、先に認証エラーを検出した側の通知として出す。照会が先なら 12 の 3.5 の「Linear の照会の失敗」（分類 `auth`）、送信が先なら「Linear への送信の失敗」（分類 `auth`）である。後から検出した側は抑止に従い、通知を出さない。
  3. 抑止が掛かっているかを確かめて掛ける操作は1つの操作として行う。照会と送信が同時に認証エラーを検出しても、通知は1回になる。Worker の1回の処理で複数の行が認証エラーになっても1回にまとめる。
  4. 取り込みの照会か、送信（コメント、状態の変更、ラベルの追加のどれでも）のどちらかが1回成功したら抑止を解く。どちらも同じ API キーで認証するため、どちらかの成功はキーが有効に戻ったことを示す。解いた後にまた認証エラーになったら、もう一度出す。
  5. Linear が認証を通したうえで返した失敗（Issue が削除・アーカイブされていた、など）は成功ではないため、抑止を解かない。
- 抑止の状態はプロセスの中に持つ。再起動の後に認証エラーになったら、もう一度出す。
- 認証エラーの通知は、この共通の抑止だけで扱う（2026-11-08 決定、Q86）。取り込み側の「5回続けて失敗した」通知（15 の 6章）は、5回目の失敗の分類が `auth` 以外のときだけ出す。
- API キーのファイルが無い・読めない・権限が 0600 でない場合も認証エラーとして扱い、共通の抑止に含める（2026-11-08 決定、Q86。取り込み側の扱いは 15 の 6章）。

### 3.5 循環トリガーの防止（requirements 6.4, 15.2）

Evaloom は Linear の変化を Issue の定期照会で取得する（[`15-linear-intake.md`](./15-linear-intake.md) 3.1）。Evaloom が Issue にコメントを付けたり、状態を変えたり、振り分けのラベルを付けたりしても、同じジョブを再び起動しないことを、次の5つで確かめる。

1. **起動の契機を `ai-ready` の付与に限り、Evaloom は `ai-ready` に触らない**: 起動は「前回の照会で `ai-ready` が付いていなかった Issue に、今回は付いている」ときだけである（[`15-linear-intake.md`](./15-linear-intake.md) 3.4）。Evaloom が付けるラベルは振り分けのラベルだけで、`ai-ready` を付けることも外すこともしない（本人決定 3）。起動の合図を Evaloom が自分で作らないためである。そのため Evaloom の操作で起動の条件が満たされることは無い。
2. **照会でコメントを読まない**: 照会は Issue の項目（ラベル、状態、Team、本文、更新時刻）だけを読み、コメントを読まない（[`15-linear-intake.md`](./15-linear-intake.md) 3.2）。Evaloom のコメントは判定の入力にならない。コメントで Issue の更新時刻が進んだとしても、起動はラベルの有無の変化だけを見るため起動しない。
3. **判定のやり直しは新しいタスクを作らない**: 更新時刻を使うのは、振り分けを待つタスクと保留したタスクの判定のやり直し（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.3）だけである。Evaloom のコメントで更新時刻が進んで判定のやり直しが走っても、Issue の中身が変わっていなければ同じ結果になり、何も起きない。同じ理由のコメントは dedupe_key（3.7）で2回出さない。本人が管理画面で設定を変えたときの判定のやり直しも、新しいタスクを作らない点は同じである。
4. **Evaloom が付ける状態は取り込み側の起動の条件にならない**: 取り込み側が状態で動くのは、状態の種類がキャンセル（canceled）のときの中止だけである（[`15-linear-intake.md`](./15-linear-intake.md) 3.4、3.7）。Evaloom が付ける状態のうち種類がキャンセルになるのは Canceled だけで、Canceled を付けるのはタスクがすでに `cancelled`（終端）になった後である（3.6.1）。照会で Canceled を見て中止の経路に入っても、終端のタスクには何もしない（15 の 3.7）。In Progress・In Review・Done は起動にも中止にも使われない。
5. **Evaloom が付ける振り分けのラベルで二重に起動しない**: Evaloom が振り分けのラベルを付けるのは、本人が管理画面で振り分けを承認したときだけで、同じトランザクションでタスクに振り分け先を書いて `queued` 以降に進めている（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.4）。照会で振り分けのラベルを見て判定し直すのは、振り分けを待つタスクと保留したタスクだけである。ラベルを付けた後の照会では、そのタスクはどちらでもないため、ラベルは判定に使われない。振り分けのラベルは起動の契機でもない（1）。同じ Issue に終端でないタスクは1件までのため（15 の 3.5）、ほかのタスクがこのラベルで動き出すことも無い。そのタスクが終わった後に本人が `ai-ready` を付け直したときは、付いている振り分けのラベルで普通に振り分ける。これは本人の操作による起動であり、循環ではない。

照会の結果には操作者が含まれないため、操作者での見分けは行わない。Evaloom 専用の Linear ユーザーの API キーを使う理由（Q49）は、次の2つである。

- Linear の画面で、Evaloom のコメント・ラベルの追加と本人の操作を見分けられる。
- API キーの持ち主を本人と分けておけば、キーを無効にしても本人の Linear の使い方に影響しない。

### 3.6 Issue の状態とラベル（Q51、2026-11-09 見直し、2026-11-10 書き戻しを追加）

2026-11-09 に本人が見直し、Evaloom が Issue の状態（Workflow state）を変えることにした。2026-11-10 に、管理画面で判断した結果を Evaloom が Linear に書き戻すことにした（本人決定 3）。Evaloom が付けてよいラベルは振り分けのラベルだけで、`ai-ready` は変えない。

#### 3.6.1 状態の対応表（2026-11-09 本人決定、2026-11-10 契機を一般化）

状態の名前は In Progress / In Review / Done / Canceled を使う。2026-11-09 の本人決定では「PR Review」だったが、2026-11-10 に本人が In Review に変えた（Q161）。契機は、PR や GitHub に結び付けず、タスクの状態と本人の判断待ちで決める。

| 契機 | Issue の状態 | outbox の kind |
|---|---|---|
| 実行開始（Execution が段を動かし始め、タスクが `running` になった。本人の判断の後の再開を含む） | In Progress | `started` |
| 本人の判断待ち（Execution のある判断待ち、つまり kind が `approval` / `blocked` / `question` の owner_decisions を作り、タスクが `waiting_approval` か `blocked` になった） | In Review | `waiting_decision` |
| 完了（タスクが `completed` になった。出す段まで成功した） | Done | `completed` |
| 失敗・タイムアウト（`failed` / `timed_out`） | 変えない | - |
| 振り分けの判断待ち（kind=`route`）、取り込みの保留 | 変えない（Q163） | - |
| Evaloom で中止（本人が管理画面で中止した） | Canceled | `cancelled` |

- 状態の名前 In Review は「本人の判断待ち」の意味で使う（Q161）。本人が照会先の Team に In Review の状態を作る。
- 失敗・タイムアウトでは状態を変えないため、Issue は直前の状態（多くは In Progress）のままになる。本人の対応が要ることはコメント（3.2）で知らせる。
- 実行開始の契機は、Execution の最初の開始と、本人の判断の後の再開ごとに1回とする（Q100 を、判断の後に同じ Execution が再開する形に合わせて書き換え）。続行・回答・承認の後に Execution が再び動き出したとき、In Review から In Progress に戻す。再実行で新しい Execution が動き出したときも In Progress を付ける。すでに In Progress なら変更は起きない。
- Linear 起因の中止（`ai-ready` の取り外し、Issue のキャンセル、削除・アーカイブ、照会先と違う Team への移動。[`15-linear-intake.md`](./15-linear-intake.md) 3.7）では状態を変えない（案、Q99）。本人が Linear 側で操作した結果なので、Evaloom が上書きしない。
- 本人が管理画面で中止したタスクには、振り分けを待つタスクと保留したタスク（Execution が無い）も含める（案、Q99）。本人が振り分けの提案を却下してタスクが `cancelled` になったときも、管理画面での判断による中止として Canceled を付ける。
- 手動作成のタスク（source=`manual`）では何もしない（3.1 と同じ）。

#### 3.6.2 状態は名前で選ぶ（2026-11-09 本人決定）

Linear の状態には種類（type: `backlog` / `unstarted` / `started` / `completed` / `canceled`）と名前がある。照会先の Team では種類が `started` の状態が In Progress と In Review の2つあるため、種類では1つに決まらない。Evaloom は状態を名前で選ぶ。

- 状態は Team ごとに別の ID を持つ。Worker は Issue の Team の状態の一覧（ID・名前・種類）を Linear から読み、名前が一致する状態の ID を使う。一覧は Team ごとにプロセスの中に持ってよい。名前が見つからないときは一覧を読み直してから判断する。
- 名前の一致は完全一致とする（大文字・小文字、空白を区別する）（案、Q96）。
- 使う名前は `In Progress` / `In Review` / `Done` / `Canceled` に固定し、設定にはしない（案、Q96）。名前を変える必要が出たら、全体設定（[`02-data-model.md`](./02-data-model.md) 3.8）に持たせる（Q161）。
- 名前が一致する状態が Team に無いときは、状態を変えずにその行を failed（reason=`state_not_found`）にする（6章）。似た名前や同じ種類の状態で代用しない。

#### 3.6.3 状態の変更の送信

状態の変更もコメントと同じ linear_outbox（3.7）と送信 Worker（3.4）で送る。

1. 契機が起きたら、状態遷移と同じトランザクションで linear_outbox に action=`set_state` の行を1つ書く。state_name に 3.6.2 の名前を入れ、body は NULL にする。dedupe_key は `<task_id>:state_<kind>:<対象の ID>` とする。対象の ID は、`started` では最初の開始なら Execution の ID、判断の後の再開なら回答した判断待ちの ID、`waiting_decision` では判断待ちの ID、`completed` では Execution の ID、`cancelled` では Execution の ID（Execution の無いタスクの中止ではタスクの ID）である。コメントの行と dedupe_key が重ならないようにするためである。
2. Worker は Issue の現在の状態を読み、次の場合は変更を送らずに status=`sent` にする。
   - すでに目的の状態になっている。
   - 現在の状態の種類が `completed` か `canceled` で、目的の状態がそれと異なる（案、Q97）。本人が Linear で手で終わらせた Issue を、Evaloom が開き直さないためである。
3. それ以外は Linear GraphQL API の Issue の更新（stateId の変更）を送る。
4. 再試行、上限、失敗の記録（`notification_failed`、通知失敗の一覧）、認証エラーの通知と共通の抑止は、コメントと同じにする（3.4、3.4.2、3.4.3）。状態の変更の成功も、抑止を解く「送信の成功」に含める。
5. 状態の変更は同じ状態を2回付けても結果が変わらないため、結果が分からない送信の照合（3.4.1）は行わない。action=`set_state` の行は outcome_unknown を常に 0 にし、結果が分からなければそのまま送り直す。

同じタスクの状態の変更は、作った順に送る。新しい action=`set_state` の行ができた時点で、同じタスクの古い行が status=`pending` のまま残っていれば、古い行は送らずに status=`skipped` にする（案、Q98）。再試行の間に、古い状態（例 In Progress）が新しい状態（例 In Review）の後に付くことを防ぐためである。status=`skipped` は送信の失敗ではないため、通知失敗の一覧には出さない。

#### 3.6.4 循環しないことの確認

状態とラベルを変えても循環しない理由は 3.5 の 4 と 5 に書いた。加えて次の点を確かめる。

- Evaloom が Canceled を付けた Issue に本人が `ai-ready` を付け直しても、起動しない（[`15-linear-intake.md`](./15-linear-intake.md) 3.4 の「状態がキャンセルなら何もしない」）。もう一度実行したいときは、本人が状態をキャンセル以外に戻してから `ai-ready` を付け直す。
- Linear 側の自動化（外のサービスとの連携で Issue の状態を変える、など）が状態を変えても、取り込み側は起動に状態を使わないため起動しない。自動化が Issue をキャンセルにした場合だけ、15 の 3.7 の中止の経路に入る。Evaloom はこの自動化を設定しない。
- Evaloom の状態の変更とラベルの追加で Issue の更新時刻が進み、判定のやり直し（15 の 3.6.3）が走っても、3.5 の 3 と同じく新しいタスクは作らない。

#### 3.6.5 振り分けのラベルの追加（2026-11-10 本人決定 3）

本人が管理画面で振り分けを承認したら（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.4）、Evaloom は Issue にそのラベルを付ける。Linear から見ても振り分け先が分かるようにし、同じ Issue に後で `ai-ready` を付け直したときに同じ振り分け先になるようにするためである。

1. 承認を記録するのと同じトランザクションで、linear_outbox に action=`add_label`、kind=`route_approved` の行を1つ書く。label_name は本人が承認したラベル（routes.label）、body と state_name は NULL、dedupe_key は `<task_id>:label:<decision_id>`（[`02-data-model.md`](./02-data-model.md) 3.7）とする。
2. 書いてよいラベルは、行を書く時点で routes にあるラベルだけである。`linear.triggerLabel`（`ai-ready`）と、`linear.routingLabelPrefix` で始まらないラベルは書けない（[`02-data-model.md`](./02-data-model.md) 3.7）。Worker は送る直前にもこの条件を確かめ、合わなければ送らずに failed（reason=`other`）にする。
3. Worker は Issue の現在のラベルを読み、すでにそのラベルが付いていれば送らずに status=`sent` にする。
4. それ以外は、Issue の Team で使えるラベルの一覧から名前が完全一致するラベルの ID を引き、Issue にそのラベルを足す要求を送る。ほかのラベルは外さない（ラベル一覧の置き換えではなく追加の操作を使う。使う操作は実装時に Linear の公式文書で確かめる）。
5. 同じラベルを2回付けても結果は変わらないため、outcome_unknown は常に 0 にし、照合（3.4.1）はしない。再試行、失敗の記録、認証エラーの通知は 3.4 と同じにする。
6. ラベルの追加に失敗しても、タスクの振り分けと実行は取り消さない。振り分け先はタスクに書いてあり、Linear のラベルは知らせるためのものである。

Evaloom はラベルを外さない。本人が付けた振り分けのラベル、ほかのラベル、`ai-ready` はそのまま残す。

### 3.7 データモデル

linear_outbox の列は次のとおりとする（[`02-data-model.md`](./02-data-model.md) 3.7 と 5.8）。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| task_id | TEXT FK→tasks | |
| linear_issue_id | TEXT | |
| action | TEXT | `comment` / `set_state`（3.6.3）/ `add_label`（3.6.5） |
| kind | TEXT | action=`comment` は 3.2 の契機（`missing_acceptance_criteria` / `decision_open` / `succeeded` / `failed` / `cancelled`）。action=`set_state` は 3.6.1 の契機（`started` / `waiting_decision` / `completed` / `cancelled`）。action=`add_label` は `route_approved` |
| state_name | TEXT NULL | action=`set_state` のとき、付ける状態の名前（3.6.2）。ほかは NULL |
| label_name | TEXT NULL | action=`add_label` のとき、付けるラベル（3.6.5）。ほかは NULL |
| dedupe_key | TEXT | コメントは `<task_id>:<kind>:<対象の ID>`（`decision_open` の対象は判断待ちの ID）、状態の変更は `<task_id>:state_<kind>:<対象の ID>`（3.6.3）、ラベルの追加は `<task_id>:label:<decision_id>`。`UNIQUE(dedupe_key)` で同じ契機の送信を1回にする |
| body | TEXT NULL | 送る本文（機密情報除外済み）。action=`comment` のときだけ |
| status | TEXT | `pending` / `sent` / `failed` / `skipped`（新しい状態の変更に置き換えられて送らなかった行。3.6.3、Q98） |
| attempts | INTEGER | |
| next_attempt_at | TEXT NULL | |
| linear_comment_id | TEXT NULL | 送信後に Linear が返したコメントの ID。action=`comment` のときだけ |
| created_at / sent_at | TEXT | |
| execution_id | TEXT NULL FK→executions | 契機の Execution。Execution の無い契機（3.4.2）では NULL（Q82） |
| outcome_unknown | INTEGER | 1 = 最後に始めた送信の結果が分からず、次に送る前に照合が要る。0 = 照合は要らない（3.4.1、Q83）。action=`set_state` と `add_label` では常に 0 |
| failure_reason | TEXT NULL | status=`failed` にしたときの reason（3.4.2）。`notification_failed` の payload と同じ値 |
| failed_at | TEXT NULL | status=`failed` にした時刻（3.4.2） |

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 返す項目 | 実行ステータス、振り分け先と実行担当、作業概要、確かめた結果、外に出したもの、エラー情報、管理画面へのリンク | requirements 6.4 |
| 本人の判断待ちの知らせ | 「管理画面で判断待ち」とリンクだけ。判断の中身は載せない | 本人決定 3 |
| 判断の場所 | 管理画面だけ。Linear 上の操作を承認・続行・回答として扱わない | 本人決定 3、requirements 15.5 |
| 受け入れ条件の無い Issue | 受け入れ条件の要る Profile なら、記入を求めるコメントを返す | requirements 6.3、契約 5.2 |
| 循環トリガー | Evaloom 自身の Issue 更新で同じジョブを起動しない | requirements 6.4, 15.2 |
| Linear API の認証 | Evaloom 専用ユーザーの API キー | requirements 6.6、Q49 |
| 長期の履歴 | Evaloom 側に保持し、Linear の保存状態に依存しない | requirements 6.5 |
| 通知の経路 | Linear コメント + macOS 通知 | requirements 8.8 |
| 送信の再試行 | 上限5回。間隔は1分・2分・4分・8分（倍にする）。5回目も失敗したら failed | Q80（2026-11-08 本人決定） |
| 送信の失敗の表示 | Execution の無い契機の失敗も含め、すべて通知失敗の一覧に出す | Q82（2026-11-08 本人決定） |
| 結果が分からないコメント | DB に残し、再起動の後も照合してから送る | Q83（2026-11-08 本人決定） |
| 認証エラーの通知 | macOS 通知（Dashboard を開く）。抑止は取り込み側と共通の1つで、同じ API キーの無効化で通知は1回。照会か送信のどちらかが成功したら解く | Q84（2026-11-08 本人決定） |
| notification_failed の reason（channel=linear） | `auth` / `issue_removed` / `retries_exhausted` / `state_not_found` / `other` | Q85（2026-11-08 本人決定）、`state_not_found` は 2026-11-09 追加 |
| Issue の状態の変更 | 実行開始→In Progress、本人の判断待ち→In Review、完了→Done、失敗・タイムアウト→変えない、Evaloom で中止→Canceled | Q51 見直し（2026-11-09 本人決定）、契機は 2026-11-10 に一般化 |
| ラベルの変更 | 本人が管理画面で承認した振り分けのラベルを足すだけ。`ai-ready` は変えない。ラベルを外さない | 本人決定 3 |
| 状態の選び方 | 種類でなく名前で選ぶ（照会先の Team は種類 started の状態が In Progress と In Review の2つあるため） | Q51 見直し（2026-11-09 本人決定） |

## 5. インターフェース

- 送信口: `Notifier`（[`12-notification.md`](./12-notification.md) 3.2）
- イベント: `notification_sent` / `notification_failed`（[`02-data-model.md`](./02-data-model.md) 5.3）。channel=`linear`
- 本人用 API: `GET /api/notification-failures`（[`01-architecture.md`](./01-architecture.md) 5.1）が channel=`linear` の失敗も返す。Execution の無い契機の失敗は linear_outbox の行から返す（3.4.2、[`12-notification.md`](./12-notification.md) 5章、Q82）
- macOS 通知: 認証エラーの通知は [`12-notification.md`](./12-notification.md) 3.5 の Dashboard を開く通知（3.4.3、Q84）
- Linear GraphQL API: コメントの作成と、再送前のコメントの読み取り。Issue の状態の変更（`issueUpdate` の stateId）、Issue の現在の状態とラベルの読み取り、Team の状態の一覧（ID・名前・種類）とラベルの一覧の読み取り、Issue へのラベルの追加（3.6.5）。クライアントは [`15-linear-intake.md`](./15-linear-intake.md) 5章と共通

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Linear API の一時的な失敗 | 1分・2分・4分・8分の間隔で再試行する（3.4）。5回目も失敗したら failed と `notification_failed`（reason=`retries_exhausted`、Q85） |
| Linear API の認証エラー | 再試行せず failed（reason=`auth`、Q85）。macOS 通知（Dashboard を開く通知）で本人に知らせる。抑止は取り込み側と共通の1つで、照会と送信の両方が認証エラーになっても通知は1回だけ出し、照会か送信のどちらかが成功したら解く（3.4.3、Q84）。API キーの値はログに出さない |
| Issue が削除・アーカイブされていた | failed（reason=`issue_removed`、Q85）。タスクには影響させない |
| 付ける名前の状態が Team に無い | 状態の一覧を読み直しても無ければ、再試行せず failed（reason=`state_not_found`）。似た名前や同じ種類の状態で代用しない。タスクには影響させない（3.6.2） |
| 付けるラベルが Team に無い | Q164 のとおり扱う。ラベルを作らない。タスクには影響させない（3.6.5） |
| 付けるラベルが routes に無い、または `ai-ready` や接頭辞で始まらないラベル | 送らずに failed（reason=`other`）。行を書く時点で拒否するため、通常は起きない（3.6.5） |
| 状態の変更・ラベルの追加の失敗 | コメントと同じく再試行し、上限で failed にする。失敗してもタスクの状態とコメントの送信は変わらない（3.6.3、3.6.5） |
| コメントを送った結果が分からない | outcome_unknown を 1 のまま残し、再起動の後も、Linear のコメントを読んで同じ dedupe_key のコメントが無いことを確かめてから再送する（3.4.1、Q83） |

## 7. スコープ外

- `ai-ready` の付け外しと、振り分けのラベルを外すこと（本人決定 3）
- Linear 起因の中止での状態の変更（3.6.1、Q99 案）
- Linear 側の自動化（状態の変更を契機にした処理）の設定
- Linear からの承認・続行・回答の操作（本人決定 3、requirements 15.5。判断は管理画面だけ）
- Linear のコメントに判断の中身（提案の理由、承認の対象、finding、質問の本文）を載せること
- Slack、スマートフォンへの通知（requirements 8.8 の将来）
- 手動作成のタスクの Linear への返却

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約（`summary`、`outputs`）
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) 管理画面の URL（Q122）
- [`02-data-model.md`](./02-data-model.md) 状態の値、イベント種別、owner_decisions、linear_outbox
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 状態遷移と本人の判断の後の再開
- [`05-runner.md`](./05-runner.md) 機密情報除外
- [`12-notification.md`](./12-notification.md) 送信口と重複・失敗の扱い
- [`13-management-ui.md`](./13-management-ui.md) 本人の判断待ちの画面
- [`15-linear-intake.md`](./15-linear-intake.md) 取り込みと振り分け
- [`17-approval.md`](./17-approval.md) 承認待ち

## 9. 実装単位

- [ ] [Backend] linear_outbox と、状態遷移・判断待ちの作成と同じトランザクションでの書き込み
  - 受け入れ基準: source=linear のタスクが succeeded になると outbox に1行できる; 同じ契機を2回発生させても dedupe_key の一意制約で行が1つ; source=manual のタスクでは行ができない
- [ ] [Backend] Linear への送信の Notifier と送信 Worker（再試行、上限で failed、notification_sent / notification_failed）
  - 受け入れ基準: テスト用のサーバーにコメントが1回届き、status=sent とコメント ID が記録される; サーバーを失敗させると attempts が増え、上限で failed と notification_failed（channel=linear）が記録される; 送信の失敗でタスクの状態が変わらない
- [ ] [Backend] コメント本文の組み立て（3.3 の項目、機密情報除外）
  - 受け入れ基準: 本文に 3.3 の項目のうちその場面に該当するものが入る; outputs の label と url、確かめた結果の summary と finding の件数が載る; Runner が保持する秘密の値を summary に含めたテストで本文が [REDACTED] になる; ログの本文と Runner のパスが本文に入らない
- [ ] [Backend] 「管理画面で判断待ち」のコメント（3.2）
  - 受け入れ基準: route・approval・blocked・question の判断待ちを作ると、それぞれ decision_open のコメントの行が1つでき、本文に「管理画面で判断待ち」と判断待ちのリンク（remote-host の指定が無ければ ID）だけが入る; 提案の理由、承認の対象、finding、質問の本文が本文に入らない; Linear のコメントへの返信で判断待ちの状態が変わらない
- [ ] [Backend] 受け入れ条件の記入依頼のコメント
  - 受け入れ基準: required の Profile で missing_acceptance_criteria で保留したタスクの Issue に記入依頼のコメントが1回付く; config_missing_profile の保留ではコメントの行ができない
- [ ] [Backend] 送った結果が分からないコメントの照合（3.4.1、Q83）
  - 受け入れ基準: 送信を始める前に outcome_unknown が 1 でコミットされる; 成功、Linear が応答した失敗、接続前の失敗で 0 に戻る; 応答の前に接続を切るテストで 1 のまま残り、Worker を作り直した（再起動した）後の送信で先にコメントを読み、同じ dedupe_key のコメントがあれば再送せず sent とそのコメント ID が記録される; 同じ状況でコメントが無ければ1回だけ送る; 停止で送信を中断すると attempts は増えず、再起動の後に照合してから送る; コメントを読めなかったときは送らず attempts が1増える; 照合の要否をプロセスの中の記憶で決めていない
- [ ] [Backend] Execution の無い契機の送信の失敗を linear_outbox に記録し、通知失敗の一覧 API に出す（3.4.2、12 の 5章、Q82）
  - 受け入れ基準: Execution の契機の行を書くと execution_id が入り、振り分けの判断待ちの decision_open・missing_acceptance_criteria・Execution の無いタスクの中止・route_approved の行では NULL になる; これらの送信が上限で failed になると、GET /api/notification-failures に taskId・taskTitle・kind・channel=linear・reason が入った項目が出て、eventId と executionId が null、outboxId が入る; Execution のある契機の失敗は execution_events の項目として1回だけ出て、outbox の行から二重に出ない; macOS 通知の失敗と Linear の失敗が混ざった一覧が記録した時刻の新しい順に並び、cursor でページを続けて読んで欠けも重複も無い; Linear の失敗の reason が、認証エラーで auth、Issue が削除・アーカイブされていたときに issue_removed、5回目も失敗したときに retries_exhausted、それ以外で other になり、notification_failed の reason と linear_outbox の failure_reason が同じ値になる（Q85）; 応答にエラーの文字列とコメントの本文が入らない
- [ ] [Backend] Linear への送信の認証エラーで macOS 通知を出す（3.4.3、12 の 3.5、Q84、Q86）
  - 受け入れ基準: 送信が認証エラーで failed になると、Dashboard の URL（http://127.0.0.1:<port>/）を開く通知が1回出る; 本文が送信の失敗であることと分類 auth だけで、API キー・応答の本文・Issue の識別子・コメントの本文が入らない; 1回の処理で複数の行が認証エラーになっても通知は1回; 照会と送信の両方が認証エラーになっても、通知は先に検出した側の1回だけ出る; 照会と送信が同時に認証エラーを検出しても通知は1回; 認証エラーが続く間はもう出ず、照会か送信（コメント・状態の変更・ラベルの追加のどれか）が1回成功した後にまた認証エラーになると、もう一度出る; Linear が認証を通したうえで返した失敗（issue_removed など）では抑止が解けない; 通知の送信に失敗してもタスクと outbox の行は変わらない; 照会の5回目の失敗の分類が auth のときは「5回続けて失敗した」通知が出ない; API キーのファイルが無い・読めない・権限が 0600 でないときも共通の抑止で通知が1回出る
- [ ] [Backend] 状態の変更の行の書き込み（3.6.1、3.6.3）
  - 受け入れ基準: source=linear のタスクの Execution が running になると action=set_state・state_name=In Progress の行が1つでき、同じ契機を2回発生させても1行のまま; approval・blocked・question の判断待ちを作ると In Review（kind=waiting_decision）の行ができる; 判断に答えて同じ Execution が再び running になると In Progress の行がもう1つできる; route の判断待ちと取り込みの保留では set_state の行ができない; completed で Done、本人の中止（振り分けを待つタスク、保留したタスク、振り分けの却下を含む）で Canceled の行ができる; failed・timed_out、Linear 起因の中止では set_state の行ができない; source=manual のタスクでは行ができない; 同じタスクに新しい set_state の行ができると、pending のままの古い set_state の行が skipped になり、comment と add_label の行は変わらない
- [ ] [Backend] 送信 Worker で Issue の状態を名前で選んで変える（3.6.2、3.6.3）
  - 受け入れ基準: テスト用の Linear サーバーで、種類 started の状態が In Progress と In Review の2つある Team の Issue に、In Review の行を送ると In Review の ID で issueUpdate が1回送られる; 名前は完全一致で、大文字・小文字の違う状態を選ばない; Team の状態の一覧を読み直しても名前が無ければ issueUpdate を送らず、再試行せず failed（reason=state_not_found）になる; すでに目的の状態の Issue には issueUpdate を送らず sent になる; 現在の状態の種類が completed か canceled で目的の状態と異なる Issue には issueUpdate を送らず sent になる; 一時的な失敗で attempts が増え、上限で failed（retries_exhausted）になる; 状態の変更の成功で認証エラーの抑止が解ける; 応答の前に接続を切っても set_state の行の outcome_unknown は 0 のまま; skipped の行が通知失敗の一覧に出ない
- [ ] [Backend] 振り分けのラベルの追加（3.6.5）
  - 受け入れ基準: 本人が管理画面で振り分けを承認すると、同じトランザクションで action=add_label・kind=route_approved・label_name=承認したラベルの行が1つできる; routes に無いラベル、linear.triggerLabel、接頭辞で始まらないラベルの行を書けない; テスト用の Linear サーバーで、ほかのラベルを外さずにそのラベルだけが足される; すでにそのラベルが付いている Issue には送らず sent になる; 送る直前に routes からラベルが消えていたら送らず failed（other）になる; 応答の前に接続を切っても outcome_unknown は 0 のまま; ラベルの追加が失敗してもタスクの振り分け先と状態が変わらない; ai-ready を付ける・外す要求がどの経路でも送られない
- [ ] [Backend] 循環しないことの確認テスト（3.5、3.6.4）
  - 受け入れ基準: ai-ready が付いたままの Issue に Evaloom がコメントを付け、更新時刻が進んだ後の照会でタスクが作られない; 保留中のタスクの Issue に Evaloom が保留のコメントを付けた後の照会で、同じ理由のコメントがもう1回作られない; Linear クライアントの照会がコメントを要求しない; 本人の中止でタスクが cancelled になり Issue を Canceled にした後の照会で、中止の経路に入ってもタスクと audit_logs が変わらない; Canceled の Issue に ai-ready を付け直した照会でタスクが作られない; In Progress・In Review・Done に変えて更新時刻が進んだ後の照会で、ai-ready が付いたままの Issue にタスクが作られない; Evaloom が振り分けのラベルを付けて更新時刻が進んだ後の照会で、新しいタスクも2つ目の Execution も作られず、提案と判断待ちも作られない
- [ ] （人間） 照会先の Team の Workflow state に In Progress・In Review・Done・Canceled の名前の状態があることを Linear で確かめる。振り分けの表のラベルを Linear の Team に作っておく（Q164）。Linear 側の自動化を使うかを決める（3.6.4）

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q161 | 状態の名前 In Review を、PR に限らない名前に変えるか | 決定（2026-11-10 本人）: In Review に変える。本人が Linear の Team に In Review の状態を作る |
| Q162 | 「管理画面で判断待ち」のコメントを、判断待ちの行ごとに出すか。同じ Execution で操作ごとに複数の承認待ちができると、コメントが続けて複数付く | 決定（2026-11-10 本人、案を採用）: 判断待ちの行ごとに1回とし、同じトランザクションで作った複数の承認待ちは1つのコメントにまとめる（dedupe_key の対象を、承認待ちでは Execution と段の実行の ID にする） |
| Q163 | 振り分けの判断待ち（kind=`route`）と取り込みの保留で、Issue の状態を変えるか | 決定（2026-11-10 本人、案を採用）: 変えない。まだ仕事を始めておらず、本人が Linear で振り分けのラベルを付けて決めることもできるため、Issue は本人が置いた状態のままにする。判断待ちはコメントで知らせる |
| Q164 | 振り分けのラベルを付けるときに、そのラベルが Linear の Team に無かったら | 決定（2026-11-10 本人、案を採用）: ラベルを作らない（Evaloom の API キーにラベルを作る権限を求めないため）。再試行せず failed にし、reason は新しい値 `label_not_found` にする（[`02-data-model.md`](./02-data-model.md) 5.3 と 3.4.2 の表に足した）。タスクの振り分けと実行は進める。振り分けの表にラベルを登録するときに、Linear に同じ名前のラベルがあるかを管理画面で示す |
