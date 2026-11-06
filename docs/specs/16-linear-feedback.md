---
status: draft
範囲: Linear Issue への実行結果の返却（コメント）、受け入れ条件の記入依頼、送信の再試行、Evaloom 自身の更新による再起動（循環トリガー）の防止
---

# 16. Linear 連携：結果の返却 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: requirements 8.2 の工程15（結果返却）を Linear について行う。受信とタスク作成は [`15-linear-intake.md`](./15-linear-intake.md)

この文書は Phase 2 の下書きである。requirements で確定していない点は「案」とし、00-overview.md の要確認リスト（Q49, Q51, Q52）に載せている。

## 1. 目的

- Linear から来たタスクの実行結果を、元の Issue に返す（requirements 6.4、19章 Phase 2）。
- 受け入れ条件の無い Issue に、記入を求めるコメントを返す（requirements 6.3）。
- 本人の対応が必要なとき（承認待ちなど）を Linear にも知らせる（requirements 8.8）。
- Evaloom 自身の Issue 更新で、同じジョブを再び起動しない（requirements 6.4, 15.2 循環トリガー防止）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | タスク・Execution の状態遷移（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.4）、保留（[`15-linear-intake.md`](./15-linear-intake.md) 3.6）、承認の発生（[`17-approval.md`](./17-approval.md)） |
| 出力 | Linear Issue へのコメント、linear_outbox、`notification_sent` / `notification_failed` イベント |

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
4. 失敗したら attempts を増やし、間隔を空けて再試行する。上限回数（実装時に確定）を超えたら status=`failed` にし、`notification_failed` イベント（channel=`linear`）を書く。
5. 送信に失敗しても、タスクの状態遷移は取り消さない（[`12-notification.md`](./12-notification.md) 3.4 と同じ）。

送った結果が分からない（応答の前に接続が切れた）ときは、同じ dedupe_key（3.7）のコメントがすでに付いていないかを Linear から読んで確かめてから再送する。照合できるよう、コメントの本文の末尾に dedupe_key を入れる（案）（requirements 15.8 の「重複して実行する前に外部システムの状態を照合する」と同じ考え方）。

### 3.5 循環トリガーの防止（requirements 6.4, 15.2）

Evaloom は Linear の変化を Issue の定期照会で取得する（[`15-linear-intake.md`](./15-linear-intake.md) 3.1）。Evaloom が Issue にコメントを付けても、同じジョブを再び起動しないことを、次の3つで確かめる。

1. **起動の契機を `ai-ready` の付与に限る**: 起動は「前回の照会で `ai-ready` が付いていなかった Issue に、今回は付いている」ときだけである（[`15-linear-intake.md`](./15-linear-intake.md) 3.4）。Evaloom は Issue のラベルを変えない（Q51 案）。そのため Evaloom の操作で起動の条件が満たされることは無い。
2. **照会でコメントを読まない**: 照会は Issue の項目（ラベル、状態、Team、本文、更新時刻）だけを読み、コメントを読まない（[`15-linear-intake.md`](./15-linear-intake.md) 3.2）。Evaloom のコメントは判定の入力にならない。コメントで Issue の更新時刻が進んだとしても、判定はラベルの有無の変化だけを見るため起動しない。
3. **保留の再判定は新しいタスクを作らない**: 更新時刻を使うのは保留したタスクの再判定（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.3）だけである。Evaloom のコメントで更新時刻が進んで再判定が走っても、Issue の中身が変わっていなければ同じ保留の理由になり、何も起きない。同じ理由のコメントは dedupe_key（3.7）で2回出さない。

Webhook のときに考えていた「Comment のイベントを購読しない」と「操作者が Evaloom のユーザーと一致するイベントを無視する」は、ポーリングでは不要になった。照会の結果には操作者が含まれないため、操作者での見分けは行わない。

Evaloom 専用の Linear ユーザーの API キーを使う理由（Q49）は、次の2つになる。

- Linear の画面で、Evaloom のコメントと本人のコメントを見分けられる。
- API キーの持ち主を本人と分けておけば、キーを無効にしても本人の Linear の使い方に影響しない。

### 3.6 Issue の状態（Q51）

requirements 6.4 は Issue への情報の返却を求めているが、Issue の状態（Workflow state）を Evaloom が変えるかは決めていない。案は「Phase 2 ではコメントだけを返し、状態とラベルは変えない」とする。状態を変えると、Linear 側の自動化（状態の変更を契機にした別の処理）を介して循環が起きうるためである。

### 3.7 データモデル（Phase 2 で追加）

#### linear_outbox（追加）

17章に無い。Linear へのコメントを確実に1回送るために追加する。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| task_id | TEXT FK→tasks | |
| linear_issue_id | TEXT | |
| kind | TEXT | 3.2 の契機（`intake_blocked` / `missing_acceptance_criteria` / `blocked` / `waiting_approval` / `succeeded` / `failed` / `merged` / `cancelled`） |
| dedupe_key | TEXT | `<task_id>:<kind>:<対象の ID>`。`UNIQUE(dedupe_key)` で同じ契機のコメントを1回にする |
| body | TEXT | 送る本文（機密情報除外済み） |
| status | TEXT | `pending` / `sent` / `failed` |
| attempts | INTEGER | |
| next_attempt_at | TEXT NULL | |
| linear_comment_id | TEXT NULL | 送信後に Linear が返した ID |
| created_at / sent_at | TEXT | |

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

## 5. インターフェース

- 送信口: `Notifier`（[`12-notification.md`](./12-notification.md) 3.2）
- イベント: `notification_sent` / `notification_failed`（[`02-data-model.md`](./02-data-model.md) 5.3）。channel の値に `linear` を足す
- 本人用 API: `GET /api/notification-failures`（[`01-architecture.md`](./01-architecture.md) 5.1）が channel=`linear` の失敗も返す
- Linear GraphQL API: コメントの作成と、再送前のコメントの読み取り。クライアントは [`15-linear-intake.md`](./15-linear-intake.md) 5 と共通

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Linear API の一時的な失敗 | 間隔を空けて再試行。上限で failed と `notification_failed` |
| Linear API の認証エラー | 再試行せず failed。macOS 通知で本人に知らせる（案）。API キーの値はログに出さない |
| Issue が削除・アーカイブされていた | failed（reason=`other`）。タスクには影響させない |
| 送った結果が分からない | Linear のコメントを読んで、同じ dedupe_key のコメントが無いことを確かめてから再送する |

## 7. スコープ外

- Issue の状態・ラベルの変更（Q51 案）
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
