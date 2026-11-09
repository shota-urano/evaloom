---
status: confirmed
confirmed_rev: 559a891
範囲: Linear Issue への実行結果の返却（コメント）、Issue の状態（Workflow state）の変更、受け入れ条件の記入依頼、送信の再試行、Evaloom 自身の更新による再起動（循環トリガー）の防止
---

# 16. Linear 連携：結果の返却 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: requirements 8.2 の工程15（結果返却）を Linear について行う。受信とタスク作成は [`15-linear-intake.md`](./15-linear-intake.md)

この文書は Phase 2 の下書きである。requirements で確定していない点は「案」とし、00-overview.md の要確認リスト（Q49, Q51, Q52）に載せている。2026-11-08 に本人が決めた点は Q80、Q82〜Q86 に載せている。2026-11-09 に Q51 を見直し、Evaloom が Issue の状態を変えることにした（3.6）。決まっていない点は Q96〜Q100 に載せている。

## 1. 目的

- Linear から来たタスクの実行結果を、元の Issue に返す（requirements 6.4、19章 Phase 2）。
- 受け入れ条件の無い Issue に、記入を求めるコメントを返す（requirements 6.3）。
- 本人の対応が必要なとき（承認待ちなど）を Linear にも知らせる（requirements 8.8）。
- タスクの進み具合に合わせて Issue の状態（Workflow state）を変え、Linear の画面で進み具合が分かるようにする（Q51 の見直し、2026-11-09 本人決定）。
- Evaloom 自身の Issue 更新で、同じジョブを再び起動しない（requirements 6.4, 15.2 循環トリガー防止）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | タスク・Execution の状態遷移（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.4）、保留（[`15-linear-intake.md`](./15-linear-intake.md) 3.6）、承認の発生（[`17-approval.md`](./17-approval.md)） |
| 出力 | Linear Issue へのコメント、Issue の状態の変更、linear_outbox、`notification_sent` / `notification_failed` イベント |

## 3. 処理詳細

### 3.1 送信口

[`12-notification.md`](./12-notification.md) 3.2 の `Notifier` に、Linear コメントの実装を足す。macOS 通知と同じ契機から呼ばれ、source=`linear` のタスクにだけ送る。手動作成のタスクには送らない。

Q18（Phase 1 は macOS 通知だけ）は、Phase 2 でこの実装を足すことで解消する。

### 3.2 コメントを出す場面（Q52）

requirements 6.4 は「処理完了後」に返すとし、8.8 は「承認待ちなど、本人の対応が必要なときに通知する」としている。場面の案は次のとおり。

| 契機 | 出すか | 本文の主な内容 |
|---|---|---|
| タスクの作成（受付） | 出さない（案） | - |
| 保留（ルーティング） | 出す | 保留の理由と、直し方（実行先ラベルを1つにする、登録済みのラベルを付ける） |
| 保留（受け入れ条件が無い） | 出す（requirements 6.3） | 受け入れ条件を書く見出しの書き方（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.1） |
| Execution が `blocked` | 出す | Block 原因（[`02-data-model.md`](./02-data-model.md) 5.6）。続行は管理画面で本人が選ぶこと |
| タスクが `waiting_approval` | 出す | 必要な承認事項（操作の種類と対象）。承認は管理画面で本人が行うこと |
| Execution が `succeeded`（PR を作った、またはベースブランチへ push した） | 出す | 6.4 の項目 |
| Execution が `failed` / `interrupted` / `timed_out` | 出す | 6.4 の項目 |
| マージが完了した | 出す | マージした commit の SHA |
| タスクの中止 | Linear 起因の中止でも本人の中止でも出す（案） | 中止の理由 |

同じタスクの同じ状態について、コメントは1回だけ出す（[`12-notification.md`](./12-notification.md) 3.4 と同じ考え方）。

### 3.3 本文（requirements 6.4）

| 項目 | 中身 |
|---|---|
| 実行ステータス | タスクの状態の表示名（[`02-data-model.md`](./02-data-model.md) 5.1） |
| 実行担当ハーネス | Harness ID と Profile の版 |
| 作業概要 | HarnessResult の summary（[`02-data-model.md`](./02-data-model.md) 5.8） |
| 検証結果 | Gate 2 の実行数・skip 数と確認方法、Gate 3 の結果 |
| PR URL | executions.pr_url。ベースブランチへ直接 push したときは push した commit の SHA |
| エラー情報 | エラー分類と、機密情報を除いた理由の要約 |
| 必要な承認事項 | 承認待ちの操作の種類と対象（[`17-approval.md`](./17-approval.md)） |

- 本文には秘密情報を入れない。ログの本文、テストの出力、Runner のパスも入れない（案）。Linear の Issue は本人以外のワークスペースのメンバーも読めるためである。
- 作業概要はハーネスが書いた文なので、そのまま載せる前に機密情報除外（[`05-runner.md`](./05-runner.md) 3.5）と同じ処理を通す。
- 管理画面の URL は 127.0.0.1 のため、Mac mini 以外では開けない。本文には「管理画面のタスク ID」として ID だけを載せる。

### 3.4 送信と再試行

1. 契機が起きたら、状態遷移と同じトランザクションで linear_outbox に1行書く（status=`pending`）。
2. Control Plane の Worker が `pending` の行を古い順に送る。Linear GraphQL API のコメント作成を使い、認証は個人 API キー（requirements 6.6）。
3. 成功したら status=`sent`、Linear のコメント ID を記録し、`notification_sent` イベント（channel=`linear`）を書く。
4. 失敗したら attempts を増やし、間隔を空けて再試行する。上限は5回。間隔は1分・2分・4分・8分と倍にし、5回目も失敗したら status=`failed` にし、`notification_failed` イベント（channel=`linear`）を書く（最初の失敗からおよそ15分。Q80、2026-11-08 決定）。
5. 送信に失敗しても、タスクの状態遷移は取り消さない（[`12-notification.md`](./12-notification.md) 3.4 と同じ）。

送った結果が分からない（応答の前に接続が切れた）ときは、同じ dedupe_key（3.7）のコメントがすでに付いていないかを Linear から読んで確かめてから再送する。照合できるよう、コメントの本文の末尾に dedupe_key を入れる（案）（requirements 15.8 の「重複して実行する前に外部システムの状態を照合する」と同じ考え方）。照合の要否は DB に残し、再起動をまたいでも照合してから送る（3.4.1）。

#### 3.4.1 結果が分からない送信の照合（2026-11-08 決定、Q83）

送った結果が分からない状態を linear_outbox の `outcome_unknown`（3.7）に残す。プロセスの中の記憶には頼らない。再起動の後も、この列が 1 の行は照合してから送る。

1. Worker はコメント作成の要求を Linear へ送る前に、その行の outcome_unknown を 1 にしてコミットする。
2. 結果が分かったら、outcome_unknown を 0 に戻す。結果が分かるのは、送信が成功したとき（status=`sent` と同じトランザクション）、Linear が応答を返したうえで失敗したとき、接続を始める前に失敗したときである。
3. 次の場合は outcome_unknown が 1 のまま残る。接続した後の通信の失敗・時間切れ・応答の本文を読めなかったとき、Control Plane の停止で送信が中断したとき、送信中にプロセスが異常終了したとき。
4. outcome_unknown が 1 の行を次に送る前（再起動の後を含む）に、Issue のコメントを読み、最終行が同じ dedupe_key のコメントを探す。あれば送らずに status=`sent` にし、そのコメントの ID を linear_comment_id に記録する。無ければ 1 から送り直す。
5. コメントを読めなかったときは送らず、その回の失敗として attempts を増やし、3.4 の再試行に従う。outcome_unknown は 1 のまま残す。
6. 停止による中断はこれまでどおり attempts を増やさない。outcome_unknown が 1 で残るため、再起動の後は照合してから送る。

- 既存の DB に outcome_unknown の列を足すマイグレーションでは、status=`pending` かつ attempts が1以上の行を 1、それ以外の行を 0 にする。PR #69 の実装は、再起動の前に試した行を照合する側に倒していたため、同じ扱いにする。

#### 3.4.2 送信の失敗の記録と管理画面への表示（2026-11-08 決定、Q82）

送れなかったコメントは、契機に Execution があってもなくても、すべて管理画面の通知失敗の一覧（[`12-notification.md`](./12-notification.md) 5章の `GET /api/notification-failures`）に出す。

- Execution のある契機（`blocked` / `waiting_approval` / `succeeded` / `failed` / `merged` と、Execution のあるタスクの `cancelled`）は、これまでどおり execution_events に `notification_failed`（channel=`linear`）を書く。一覧はこのイベントを返す。
- Execution の無い契機（`intake_blocked`、`missing_acceptance_criteria`、保留したタスクの中止の `cancelled`）は execution_events に書けない。linear_outbox の行（status=`failed`、`failure_reason`、`failed_at`、3.7）を失敗の記録とし、一覧はこの行も返す。
- どちらの契機かは、linear_outbox の `execution_id`（3.7）で決める。outbox の行を書くときに契機の Execution の ID を入れ、Execution の無い契機では NULL にする。Worker は execution_id が NULL でない行だけ `notification_failed` を書く。一覧は execution_id が NULL で status=`failed` の行を足す。同じ失敗を2回出さない。
- failed にするときは、Worker が `failure_reason` に `notification_failed` の payload の reason と同じ値を、`failed_at` に failed にした時刻を書く。行の更新とイベントの書き込みは1つのトランザクションで行う。
- 既存の DB に3つの列を足すマイグレーションでは、execution_id を、dedupe_key の対象（`<task_id>:<kind>:<対象の ID>` の対象の ID）がその行のタスクの Execution のときだけ埋め、それ以外は NULL にする。すでに failed の行は、failure_reason を `other`、failed_at を created_at にする（2026-11-08 決定、Q82。失敗した時刻と理由の記録が無いため）。
- 一覧に linear_outbox の行が混ざるときは、記録した時刻（execution_events.created_at、linear_outbox.failed_at）の新しい順に並べ、cursor はその並びの位置を表す値にする（2026-11-08 決定、Q82。[`12-notification.md`](./12-notification.md) 5章）。

`notification_failed` の reason のうち channel=`linear` で使う値（2026-11-08 決定、Q85）:

| `reason` | 当てはまる失敗 |
|---|---|
| `auth` | Linear API の認証エラー（再試行せず failed、6章） |
| `issue_removed` | Issue が削除・アーカイブされていた（6章） |
| `retries_exhausted` | 一時的な失敗が続き、5回目も失敗した（3.4 の 4） |
| `state_not_found` | 状態の変更で、付ける名前の状態が Issue の Team に無かった（3.6.2。2026-11-09 追加、案） |
| `other` | 上のどれにも当てはまらない失敗 |

- この5つの値は [`02-data-model.md`](./02-data-model.md) 5.3 の列挙に channel=`linear` 用として載せる。[`12-notification.md`](./12-notification.md) 3.4 の分類は macOS 通知のヘルパーの失敗だけを想定しているため、Linear の失敗には使わない。
- Worker は failed にした理由に合わせて、`notification_failed` の reason と linear_outbox の failure_reason に同じ値を書く。PR #69 の実装は Linear の失敗をすべて `other` にしているため、この表のとおりに分けるよう直す。
- 既存の failed の行の failure_reason は、理由の記録が無いため `other` のままにする（上のマイグレーション）。
- 値を足しても、payload にエラーの文字列を入れないこと（12 の 3.4）は変えない。

#### 3.4.3 認証エラーの macOS 通知（2026-11-08 決定、Q84）

Worker が Linear API の認証エラーで行を failed にしたら、macOS 通知で本人に知らせる。取り込み側の通知（[`15-linear-intake.md`](./15-linear-intake.md) 6章、[`12-notification.md`](./12-notification.md) 3.5 の Dashboard を開く通知）と同じ扱いにし、抑止は取り込み側と共通の1つを使う（2026-11-08 決定、Q84）。

- 通知は 12 の 3.5 の Dashboard を開く通知として出す。クリックで `http://127.0.0.1:<port>/` を開く。
- 本文には、Linear コメントの送信の失敗であることと失敗の分類（`auth`）だけを入れる。API キーの値、Linear の応答の本文、Issue の識別子、コメントの本文は入れない。
- この通知は Execution を持たないため、execution_events の `notification_sent` / `notification_failed` には記録しない。通知の送信に失敗したら Control Plane のログにだけ残す（12 の 3.5）。認証エラーで failed になった行そのものは、3.4.2 のとおり通知失敗の一覧に出る。
- 抑止は「認証エラーの通知の抑止」1つを Control Plane が持ち、取り込み側の Linear Poller（[`15-linear-intake.md`](./15-linear-intake.md) 6章）と送信 Worker が共有する（2026-11-08 決定、Q84）。同じ API キーが無効になって照会と送信の両方が認証エラーになっても、通知は1回だけ出る。
  1. 照会か送信のどちらかが認証エラーになったとき、抑止が掛かっていなければ通知を1回出し、抑止を掛ける。抑止が掛かっているときは出さない。
  2. 通知は、先に認証エラーを検出した側の通知として出す。照会が先なら 12 の 3.5 の「Linear の照会の失敗」（分類 `auth`）、送信が先なら「Linear コメントの送信の失敗」（分類 `auth`）である。後から検出した側は抑止に従い、通知を出さない。
  3. 抑止が掛かっているかを確かめて掛ける操作は1つの操作として行う。照会と送信が同時に認証エラーを検出しても、通知は1回になる。Worker の1回の処理で複数の行が認証エラーになっても1回にまとめる。
  4. 取り込みの照会（どれか1つの Team）か、コメントの送信のどちらかが1回成功したら抑止を解く。どちらも同じ API キーで認証するため、どちらかの成功はキーが有効に戻ったことを示す。解いた後にまた認証エラーになったら、もう一度出す。
  5. Linear が認証を通したうえで返した失敗（Issue が削除・アーカイブされていた、など）は成功ではないため、抑止を解かない。
- 抑止の状態はプロセスの中に持つ。再起動の後に認証エラーになったら、もう一度出す。
- 認証エラーの通知は、この共通の抑止だけで扱う（2026-11-08 決定、Q86）。取り込み側の「5回続けて失敗した」通知（15 の 6章）は、5回目の失敗の分類が `auth` 以外のときだけ Team ごとの抑止で出す。5回目の失敗の分類が `auth` のときは、5回続けて失敗した通知を出さず、共通の抑止に従う認証エラーの通知だけにする。
- API キーのファイルが無い・読めない・権限が 0600 でない場合も認証エラーとして扱い、共通の抑止に含める（2026-11-08 決定、Q86。取り込み側の扱いは 15 の 6章）。

### 3.5 循環トリガーの防止（requirements 6.4, 15.2）

Evaloom は Linear の変化を Issue の定期照会で取得する（[`15-linear-intake.md`](./15-linear-intake.md) 3.1）。Evaloom が Issue にコメントを付けたり状態を変えたりしても、同じジョブを再び起動しないことを、次の4つで確かめる。

1. **起動の契機を `ai-ready` の付与に限る**: 起動は「前回の照会で `ai-ready` が付いていなかった Issue に、今回は付いている」ときだけである（[`15-linear-intake.md`](./15-linear-intake.md) 3.4）。Evaloom は Issue のラベルを変えない（Q51。2026-11-09 の見直しでもラベルは変えない）。そのため Evaloom の操作で起動の条件が満たされることは無い。
2. **照会でコメントを読まない**: 照会は Issue の項目（ラベル、状態、Team、本文、更新時刻）だけを読み、コメントを読まない（[`15-linear-intake.md`](./15-linear-intake.md) 3.2）。Evaloom のコメントは判定の入力にならない。コメントで Issue の更新時刻が進んだとしても、判定はラベルの有無の変化だけを見るため起動しない。
3. **保留の再判定は新しいタスクを作らない**: 更新時刻を使うのは保留したタスクの再判定（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.3）だけである。Evaloom のコメントで更新時刻が進んで再判定が走っても、Issue の中身が変わっていなければ同じ保留の理由になり、何も起きない。同じ理由のコメントは dedupe_key（3.7）で2回出さない。本人が管理画面で Project の設定を変えたときの再判定（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.3、Q81）も、新しいタスクを作らない点は同じである。
4. **Evaloom が付ける状態は取り込み側の起動の条件にならない**: 取り込み側が状態で動くのは、状態の種類がキャンセル（canceled）のときの中止だけである（[`15-linear-intake.md`](./15-linear-intake.md) 3.4、3.7）。Evaloom が付ける状態のうち種類がキャンセルになるのは Canceled だけで、Canceled を付けるのはタスクがすでに `cancelled`（終端）になった後である（3.6.1）。照会で Canceled を見て中止の経路に入っても、終端のタスクには何もしない（15 の 3.7）。In Progress・PR Review・Done は起動にも中止にも使われない。

Webhook のときに考えていた「Comment のイベントを購読しない」と「操作者が Evaloom のユーザーと一致するイベントを無視する」は、ポーリングでは不要になった。照会の結果には操作者が含まれないため、操作者での見分けは行わない。

Evaloom 専用の Linear ユーザーの API キーを使う理由（Q49）は、次の2つになる。

- Linear の画面で、Evaloom のコメントと本人のコメントを見分けられる。
- API キーの持ち主を本人と分けておけば、キーを無効にしても本人の Linear の使い方に影響しない。

### 3.6 Issue の状態（Q51、2026-11-09 見直し）

2026-11-06 の決定（Q51）は「コメントだけを返し、状態とラベルは変えない」だった。2026-11-09 に本人が見直し、Evaloom が Issue の状態（Workflow state）を変えることにした。ラベル（`ai-ready` と実行先ラベル）は今までどおり変えない。

#### 3.6.1 対応表（2026-11-09 本人決定）

| 契機 | Issue の状態 | outbox の kind |
|---|---|---|
| 実行開始（タスクが `running` になった） | In Progress | `started` |
| PR 作成・承認待ち（タスクが `waiting_approval` になった） | PR Review | `waiting_approval` |
| マージ・完了（タスクが `completed` になった。PR のマージ、ベースブランチへの直接 push、GitHub 上で本人が直接マージした場合（[`17-approval.md`](./17-approval.md) Q58）を含む） | Done | `completed` |
| 失敗・Blocked（`failed` / `timed_out` / `blocked`） | 変えない | - |
| Evaloom で中止（本人が管理画面で中止した） | Canceled | `cancelled` |

- 失敗・Blocked では状態を変えないため、Issue は直前の状態（多くは In Progress）のままになる。本人の対応が要ることはコメント（3.2）で知らせる。
- 実行開始の契機は Execution ごとに1回とする（案、Q100）。続行・再実行で新しい Execution が `running` になったときも In Progress を付ける。すでに In Progress なら変更は起きない。
- Linear 起因の中止（`ai-ready` の取り外し、Issue のキャンセル、削除・アーカイブ、登録していない Team への移動。[`15-linear-intake.md`](./15-linear-intake.md) 3.7）では状態を変えない（案、Q99）。本人が Linear 側で操作した結果なので、Evaloom が上書きしない。
- 本人が管理画面で中止したタスクには、保留したタスク（Execution が無い）も含める（案、Q99）。
- 手動作成のタスク（source=`manual`）では何もしない（3.1 と同じ）。

#### 3.6.2 状態は名前で選ぶ（2026-11-09 本人決定）

Linear の状態には種類（type: `backlog` / `unstarted` / `started` / `completed` / `canceled`）と名前がある。Uslab の Team では種類が `started` の状態が In Progress と PR Review の2つあるため、種類では1つに決まらない。Evaloom は状態を名前で選ぶ。

- 状態は Team ごとに別の ID を持つ。Worker は Issue の Team の状態の一覧（ID・名前・種類）を Linear から読み、名前が一致する状態の ID を使う。一覧は Team ごとにプロセスの中に持ってよい。名前が見つからないときは一覧を読み直してから判断する。
- 名前の一致は完全一致とする（大文字・小文字、空白を区別する）（案、Q96）。15 の Q51 への言及（「Evaloom はラベルを外さない」）は見直しの後も正しいため、15 は書き換えない。
- 使う名前は `In Progress` / `PR Review` / `Done` / `Canceled` に固定し、設定にはしない（案、Q96）。Team によって名前を変える必要が出たら、Linear の全体設定（[`15-linear-intake.md`](./15-linear-intake.md) の `linear.triggerLabel` と同じ置き場、Q76）に持たせる。
- 名前が一致する状態が Team に無いときは、状態を変えずにその行を failed（reason=`state_not_found`）にする（6章）。似た名前や同じ種類の状態で代用しない。

#### 3.6.3 送信

状態の変更もコメントと同じ linear_outbox（3.7）と送信 Worker（3.4）で送る。

1. 契機が起きたら、状態遷移と同じトランザクションで linear_outbox に action=`set_state` の行を1つ書く。state_name に 3.6.2 の名前を入れ、body は NULL にする。dedupe_key は `<task_id>:state_<kind>:<対象の ID>`（対象は Execution の ID。Execution の無い保留したタスクの中止ではタスクの ID）とする。コメントの行と dedupe_key が重ならないようにするためである。
2. Worker は Issue の現在の状態を読み、次の場合は変更を送らずに status=`sent` にする。
   - すでに目的の状態になっている。
   - 現在の状態の種類が `completed` か `canceled` で、目的の状態がそれと異なる（案、Q97）。本人が Linear で手で終わらせた Issue を、Evaloom が開き直さないためである。
3. それ以外は Linear GraphQL API の Issue の更新（stateId の変更）を送る。
4. 再試行、上限、失敗の記録（`notification_failed`、通知失敗の一覧）、認証エラーの通知と共通の抑止は、コメントと同じにする（3.4、3.4.2、3.4.3）。状態の変更の成功も、抑止を解く「送信の成功」に含める。
5. 状態の変更は同じ状態を2回付けても結果が変わらないため、結果が分からない送信の照合（3.4.1）は行わない。action=`set_state` の行は outcome_unknown を常に 0 にし、結果が分からなければそのまま送り直す。

同じタスクの状態の変更は、作った順に送る。新しい action=`set_state` の行ができた時点で、同じタスクの古い行が status=`pending` のまま残っていれば、古い行は送らずに status=`skipped` にする（案、Q98）。再試行の間に、古い状態（例: In Progress）が新しい状態（例: PR Review）の後に付くことを防ぐためである。status=`skipped` は送信の失敗ではないため、通知失敗の一覧には出さない。

#### 3.6.4 循環しないことの確認

状態を変えても循環しない理由は 3.5 の 4 に書いた。加えて次の点を確かめる。

- Evaloom が Canceled を付けた Issue に本人が `ai-ready` を付け直しても、起動しない（[`15-linear-intake.md`](./15-linear-intake.md) 3.4 の「状態がキャンセルなら何もしない」）。もう一度実行したいときは、本人が状態をキャンセル以外に戻してから `ai-ready` を付け直す。
- Linear 側の自動化（GitHub 連携で PR のマージに合わせて Issue を Done にする、など）が状態を変えても、取り込み側は起動に状態を使わないため起動しない。自動化が Issue をキャンセルにした場合だけ、15 の 3.7 の中止の経路に入る。Evaloom はこの自動化を設定しない。
- Evaloom の状態の変更で Issue の更新時刻が進み、保留したタスクの再判定（15 の 3.6.3）が走っても、3.5 の 3 と同じく新しいタスクは作らない。

### 3.7 データモデル（Phase 2 で追加）

#### linear_outbox（追加）

17章に無い。Linear へのコメントと Issue の状態の変更を確実に1回送るために追加する。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| task_id | TEXT FK→tasks | |
| linear_issue_id | TEXT | |
| kind | TEXT | コメントは 3.2 の契機（`intake_blocked` / `missing_acceptance_criteria` / `blocked` / `waiting_approval` / `succeeded` / `failed` / `merged` / `cancelled`）。状態の変更は 3.6.1 の契機（`started` / `waiting_approval` / `completed` / `cancelled`） |
| action | TEXT | `comment` / `set_state`（3.6.3）。既存の行は `comment` |
| state_name | TEXT NULL | action=`set_state` のとき、付ける状態の名前（3.6.2）。`comment` では NULL |
| dedupe_key | TEXT | コメントは `<task_id>:<kind>:<対象の ID>`、状態の変更は `<task_id>:state_<kind>:<対象の ID>`（3.6.3）。`UNIQUE(dedupe_key)` で同じ契機のコメントを1回にする |
| body | TEXT NULL | 送る本文（機密情報除外済み）。action=`set_state` では NULL |
| status | TEXT | `pending` / `sent` / `failed` / `skipped`（新しい状態の変更に置き換えられて送らなかった行。3.6.3、Q98） |
| attempts | INTEGER | |
| next_attempt_at | TEXT NULL | |
| linear_comment_id | TEXT NULL | 送信後に Linear が返したコメントの ID。action=`set_state` では NULL |
| created_at / sent_at | TEXT | |
| execution_id | TEXT NULL FK→executions | 契機の Execution。Execution の無い契機（`intake_blocked`、`missing_acceptance_criteria`、保留したタスクの中止）では NULL（3.4.2、Q82） |
| outcome_unknown | INTEGER | 1 = 最後に始めた送信の結果が分からず、次に送る前に照合が要る。0 = 照合は要らない（3.4.1、Q83）。action=`set_state` では常に 0（3.6.3） |
| failure_reason | TEXT NULL | status=`failed` にしたときの reason（3.4.2）。`notification_failed` の payload と同じ値 |
| failed_at | TEXT NULL | status=`failed` にした時刻（3.4.2） |

### 3.8 書き換える確定済みの仕様（2026-11-09 の Q51 見直し）

実装に入るときに、次の確定済みの仕様を書き換える。書き換えたら、その仕様の status を draft に戻す。

| 仕様 | 書き換える点 |
|---|---|
| [`02-data-model.md`](./02-data-model.md) | 5.3 の `notification_failed` の reason（channel=`linear`）に `state_not_found` を足す |

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 返す項目 | 実行ステータス、実行担当ハーネス、作業概要、検証結果、PR URL、エラー情報、必要な承認事項 | requirements 6.4 |
| 受け入れ条件の無い Issue | 記入を求めるコメントを返す | requirements 6.3 |
| 循環トリガー | Evaloom 自身の Issue 更新で同じジョブを起動しない | requirements 6.4, 15.2 |
| Linear API の認証 | 個人 API キー | requirements 6.6 |
| 長期の履歴 | Evaloom 側に保持し、Linear の保存状態に依存しない | requirements 6.5 |
| 通知の経路 | Linear コメント + macOS 通知 | requirements 8.8 |
| 承認の操作 | Linear 上の操作を承認として扱わない。承認は管理画面で本人だけが行う | requirements 15.5 |
| コメント送信の再試行 | 上限5回。間隔は1分・2分・4分・8分（倍にする）。5回目も失敗したら failed | Q80（2026-11-08 本人決定） |
| 送信の失敗の表示 | Execution の無い契機の失敗も含め、すべて通知失敗の一覧に出す | Q82（2026-11-08 本人決定） |
| 結果が分からない送信 | DB に残し、再起動の後も照合してから送る | Q83（2026-11-08 本人決定） |
| 認証エラーの通知 | macOS 通知（Dashboard を開く）。抑止は取り込み側と共通の1つで、同じ API キーの無効化で通知は1回。照会か送信のどちらかが成功したら解く | Q84（2026-11-08 本人決定） |
| notification_failed の reason（channel=linear） | `auth` / `issue_removed` / `retries_exhausted` / `state_not_found` / `other` | Q85（2026-11-08 本人決定）。`state_not_found` は 2026-11-09 の Q51 見直しで追加（案） |
| Issue の状態の変更 | 実行開始→In Progress、PR 作成・承認待ち→PR Review、マージ・完了→Done、失敗・Blocked→変えない、Evaloom で中止→Canceled | Q51 見直し（2026-11-09 本人決定） |
| ラベルの変更 | しない（`ai-ready` も実行先ラベルも変えない） | Q51 見直し（2026-11-09 本人決定） |
| 状態の選び方 | 種類でなく名前で選ぶ（Uslab は種類 started の状態が In Progress と PR Review の2つあるため） | Q51 見直し（2026-11-09 本人決定） |

## 5. インターフェース

- 送信口: `Notifier`（[`12-notification.md`](./12-notification.md) 3.2）
- イベント: `notification_sent` / `notification_failed`（[`02-data-model.md`](./02-data-model.md) 5.3）。channel の値に `linear` を足す
- 本人用 API: `GET /api/notification-failures`（[`01-architecture.md`](./01-architecture.md) 5.1）が channel=`linear` の失敗も返す。Execution の無い契機の失敗は linear_outbox の行から返す（3.4.2、[`12-notification.md`](./12-notification.md) 5章、Q82）
- macOS 通知: 認証エラーの通知は [`12-notification.md`](./12-notification.md) 3.5 の Dashboard を開く通知（3.4.3、Q84）
- Linear GraphQL API: コメントの作成と、再送前のコメントの読み取り。Issue の状態の変更（`issueUpdate` の stateId）、Issue の現在の状態の読み取り、Team の状態の一覧（ID・名前・種類）の読み取り（3.6）。クライアントは [`15-linear-intake.md`](./15-linear-intake.md) 5 と共通

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Linear API の一時的な失敗 | 1分・2分・4分・8分の間隔で再試行する（3.4）。5回目も失敗したら failed と `notification_failed`（reason=`retries_exhausted`、Q85） |
| Linear API の認証エラー | 再試行せず failed（reason=`auth`、Q85）。macOS 通知（Dashboard を開く通知）で本人に知らせる。抑止は取り込み側と共通の1つで、照会と送信の両方が認証エラーになっても通知は1回だけ出し、照会か送信のどちらかが成功したら解く（3.4.3、Q84）。API キーの値はログに出さない |
| Issue が削除・アーカイブされていた | failed（reason=`issue_removed`、Q85）。タスクには影響させない |
| 付ける名前の状態が Team に無い | 状態の一覧を読み直しても無ければ、再試行せず failed（reason=`state_not_found`）。似た名前や同じ種類の状態で代用しない。タスクには影響させない（3.6.2） |
| 状態の変更の失敗 | コメントと同じく再試行し、上限で failed にする。失敗してもタスクの状態とコメントの送信は変わらない（3.6.3） |
| 送った結果が分からない | outcome_unknown を 1 のまま残し、再起動の後も、Linear のコメントを読んで同じ dedupe_key のコメントが無いことを確かめてから再送する（3.4.1、Q83） |

## 7. スコープ外

- Issue のラベルの変更（Q51。2026-11-09 の見直しでも変えない）
- Linear 起因の中止での状態の変更（3.6.1、Q99 案）
- Linear 側の自動化（状態の変更を契機にした処理）の設定
- Linear からの承認・続行の操作（requirements 15.5。承認は管理画面だけ）
- Slack、スマートフォンへの通知（requirements 8.8 の将来）
- 手動作成のタスクの Linear への返却

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) 状態の値とイベント種別
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 状態遷移と通知の要求
- [`05-runner.md`](./05-runner.md) 機密情報除外
- [`12-notification.md`](./12-notification.md) 送信口と重複・失敗の扱い
- [`15-linear-intake.md`](./15-linear-intake.md) 受信とタスク作成
- [`17-approval.md`](./17-approval.md) 承認待ちとマージ

## 9. 実装単位

- [ ] [Backend] linear_outbox のマイグレーションと、状態遷移と同じトランザクションでの書き込み
  - 受け入れ基準: source=linear のタスクが succeeded になると outbox に1行できる; 同じ契機を2回発生させても dedupe_key の一意制約で行が1つ; source=manual のタスクでは行ができない
- [ ] [Backend] Linear コメントの Notifier と送信 Worker（再試行、上限で failed、notification_sent / notification_failed）
  - 受け入れ基準: テスト用のサーバーにコメントが1回届き、status=sent とコメント ID が記録される; サーバーを失敗させると attempts が増え、上限で failed と notification_failed（channel=linear）が記録される; 送信の失敗でタスクの状態が変わらない
- [ ] [Backend] コメント本文の組み立て（6.4 の項目、機密情報除外）
  - 受け入れ基準: 本文に 6.4 の7項目のうちその場面に該当するものが入る; Runner が保持する秘密の値を summary に含めたテストで本文が [REDACTED] になる; ログの本文と Runner のパスが本文に入らない
- [ ] [Backend] 受け入れ条件の記入依頼と保留のコメント
  - 受け入れ基準: missing_acceptance_criteria で保留したタスクの Issue に記入依頼のコメントが1回付く; ルーティングの保留では理由と直し方が載る
- [ ] [Backend] 送った結果が分からないときの照合
  - 受け入れ基準: 応答の前に接続を切るテストで、再送の前にコメントの有無を読み、すでにあれば再送しない
- [ ] [Backend] 循環トリガーの防止の確認テスト（3.5）
  - 受け入れ基準: ai-ready が付いたままの Issue に Evaloom がコメントを付け、更新時刻が進んだ後の照会でタスクが作られない; 保留中のタスクの Issue に Evaloom が保留のコメントを付けた後の照会で、同じ理由のコメントがもう1回作られない; Linear クライアントの照会がコメントを要求しない
- [ ] [Backend] 結果が分からない送信の状態を linear_outbox に残し、再起動の後も照合してから送る（3.4.1、3.7 の outcome_unknown、Q83）
  - 受け入れ基準: outcome_unknown の列を足すマイグレーションで、pending かつ attempts が1以上の行が 1、それ以外の行が 0 になる; 送信を始める前に outcome_unknown が 1 でコミットされる; 成功、Linear が応答した失敗、接続前の失敗で 0 に戻る; 応答の前に接続を切るテストで 1 のまま残り、Worker を作り直した（再起動した）後の送信で先にコメントを読み、同じ dedupe_key のコメントがあれば再送せず sent とそのコメント ID が記録される; 同じ状況でコメントが無ければ1回だけ送る; 停止で送信を中断すると attempts は増えず、再起動の後に照合してから送る; コメントを読めなかったときは送らず attempts が1増える; 照合の要否をプロセスの中の記憶で決めていない
- [ ] [Backend] Execution の無い契機の送信の失敗を linear_outbox に記録し、通知失敗の一覧 API に出す（3.4.2、3.7、12 の 5章、Q82）
  - 受け入れ基準: execution_id・failure_reason・failed_at を足すマイグレーションで、既存の行の execution_id が dedupe_key の対象の Execution で埋まり、Execution の無い行は NULL、すでに failed の行は failure_reason が other で failed_at が created_at になる; Execution の契機の行を書くと execution_id が入り、intake_blocked・missing_acceptance_criteria・保留したタスクの中止の行では NULL になる; intake_blocked・missing_acceptance_criteria・保留したタスクの中止の送信が上限で failed になると、GET /api/notification-failures に taskId・taskTitle・kind・channel=linear・reason が入った項目が出て、eventId と executionId が null、outboxId が入る; Execution のある契機の失敗は execution_events の項目として1回だけ出て、outbox の行から二重に出ない; macOS 通知の失敗と Linear の失敗が混ざった一覧が記録した時刻（execution_events.created_at、linear_outbox.failed_at）の新しい順に並び、cursor でページを続けて読んで欠けも重複も無い; Linear の失敗の reason が、認証エラーで auth、Issue が削除・アーカイブされていたときに issue_removed、5回目も失敗したときに retries_exhausted、それ以外で other になり、notification_failed の reason と linear_outbox の failure_reason が同じ値になる（Q85）; 応答にエラーの文字列とコメントの本文が入らない
- [ ] [Backend] Linear コメントの送信の認証エラーで macOS 通知を出す（3.4.3、12 の 3.5、Q84）
  - 受け入れ基準: 送信が認証エラーで failed になると、Dashboard の URL（http://127.0.0.1:<port>/）を開く通知が1回出る; 本文が送信の失敗であることと分類 auth だけで、API キー・応答の本文・Issue の識別子・コメントの本文が入らない; 1回の処理で複数の行が認証エラーになっても通知は1回; 認証エラーの通知の抑止を Linear Poller と送信 Worker で共通の1つにする（Poller の認証エラーの通知も、Team ごとの抑止ではなくこの共通の抑止で決める）; 照会と送信の両方が認証エラーになっても、通知は先に検出した側の1回だけ出る; 照会と送信が同時に認証エラーを検出しても通知は1回; 認証エラーが続く間はもう出ず、照会（どれか1つの Team）か送信のどちらかが1回成功した後にまた認証エラーになると、もう一度出る; Linear が認証を通したうえで返した失敗（issue_removed など）では抑止が解けない; 通知の送信に失敗してもタスクと outbox の行は変わらず、execution_events に notification_sent / notification_failed が書かれない; 照会の5回目の失敗の分類が auth のときは Team ごとの「5回続けて失敗した」通知が出ず、通知は共通の抑止に従う認証エラーの通知だけになる（Q86）; 5回目の失敗の分類が auth 以外（rate_limited・network・other）のときは、Team ごとの抑止で「5回続けて失敗した」通知が1回出る（Q86）; API キーのファイルが無い・読めない・権限が 0600 でないときも認証エラーとして共通の抑止で通知が1回出て、その後に照会か送信が認証エラーになっても、どちらかが1回成功するまで通知は出ない（Q86）
- [ ] [Backend] linear_outbox に action・state_name を足し、status に skipped を足すマイグレーションと、状態の変更の行の書き込み（3.6.1、3.6.3、3.7）
  - 受け入れ基準: マイグレーションで既存の行が action=comment・state_name=NULL になる; source=linear のタスクの Execution が running になると action=set_state・state_name=In Progress の行が1つでき、同じ Execution で2回発生させても1行のまま; waiting_approval で PR Review、completed で Done、本人の中止（保留したタスクを含む）で Canceled の行ができる; failed・timed_out・blocked、Linear 起因の中止（linear_label_removed・linear_issue_cancelled・linear_issue_removed・linear_issue_moved）では set_state の行ができない; source=manual のタスクでは行ができない; 同じタスクに新しい set_state の行ができると、pending のままの古い set_state の行が skipped になり、comment の行は変わらない
- [ ] [Backend] 送信 Worker で Issue の状態を名前で選んで変える（3.6.2、3.6.3）
  - 受け入れ基準: テスト用の Linear サーバーで、種類 started の状態が In Progress と PR Review の2つある Team の Issue に、PR Review の行を送ると PR Review の ID で issueUpdate が1回送られる; 名前は完全一致で、大文字・小文字の違う状態を選ばない; Team の状態の一覧を読み直しても名前が無ければ issueUpdate を送らず、再試行せず failed（reason=state_not_found）になり notification_failed（channel=linear、reason=state_not_found）が記録される; すでに目的の状態の Issue には issueUpdate を送らず sent になる; 現在の状態の種類が completed か canceled で目的の状態と異なる Issue には issueUpdate を送らず sent になる; 一時的な失敗で attempts が増え、上限で failed（retries_exhausted）になる; 認証エラーで failed（auth）になり、コメントの送信と共通の抑止で通知が1回だけ出る; 状態の変更の成功で認証エラーの抑止が解ける; 応答の前に接続を切っても set_state の行の outcome_unknown は 0 のままで、次の送信でコメントの照合をせずに送り直す; 状態の変更の失敗でタスクの状態が変わらない; skipped の行が通知失敗の一覧に出ない
- [ ] [Backend] 状態の変更で循環しないことの確認テスト（3.5 の 4、3.6.4）
  - 受け入れ基準: 本人の中止でタスクが cancelled になり Issue を Canceled にした後の照会で、中止の経路に入ってもタスクと audit_logs が変わらない; Canceled の Issue に ai-ready を付け直した照会でタスクが作られない; In Progress・PR Review・Done に変えて更新時刻が進んだ後の照会で、ai-ready が付いたままの Issue にタスクが作られない; 保留中のタスクの Issue の状態が変わって再判定が走っても、新しいタスクも同じ理由のコメントも作られない
- [ ] [Backend] notification_failed の reason（channel=linear）に state_not_found を足す（02 の 5.3、3.4.2、3.8）
  - 受け入れ基準: 02 の 5.3 の列挙に state_not_found が載る; GET /api/notification-failures が reason=state_not_found の項目を返す; 状態の名前が見つからない失敗以外では state_not_found にならない
- [ ] （人間） Uslab の各 Team の Workflow state に In Progress・PR Review・Done・Canceled の名前の状態があることを Linear で確かめる。Linear 側の自動化（GitHub 連携の状態の自動変更など）を使うかを決める（3.6.4）
