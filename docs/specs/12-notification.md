---
status: confirmed
confirmed_rev: 052abd9
範囲: タスクの完了・失敗・Blocked・タイムアウトの macOS 通知と、通知経路を後から足せる送信口
---

# 12. 通知 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Control Plane からの本人への通知（requirements 8.8）

## 1. 目的

- 本人の対応が必要なときと、タスクの完了・失敗を知らせる（requirements 8.8）。
- Gate 2・3 で既存テストの欠けや SHA の不一致を見つけ、push せずに止まったとき、本人に通知する（条件15, 18）。
- 将来 Slack やスマートフォンへの通知経路を足せる構成にする（requirements 8.8）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | タスク・Execution の状態遷移（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.4） |
| 出力 | macOS 通知、`notification_sent` イベント、送信に失敗したときは `notification_failed` イベント |

## 3. 処理詳細

### 3.1 通知する場面

| 契機 | 通知の種類 | 本文に含めるもの |
|---|---|---|
| Execution が `succeeded` | 完了 | タスク ID、件名、PR URL |
| Execution が `failed` / `interrupted` | 失敗 | タスク ID、件名、エラー分類、理由の要約 |
| Execution が `timed_out` | 失敗 | タスク ID、件名、どの工程で止まったか |
| Execution が `blocked` | 対応が必要 | タスク ID、件名、Block 原因（[`02-data-model.md`](./02-data-model.md) 5.6） |

`cancelled` は本人の操作の結果なので通知しない（案）。

通知をクリックしたら管理画面のタスク詳細を開く（3.3 の方式で実現する）。タスクを持たない通知（3.5）は、クリックで Dashboard（`/`）を開く。

### 3.2 送信口

```go
type Notifier interface {
    Notify(ctx context.Context, n Notification) error
}
```

- Control Plane は登録された Notifier すべてに送る。Phase 1 は macOS 通知の実装だけを登録する。
- 新しい経路（Slack 等）は Notifier の実装を足すだけで追加できるようにする。

### 3.3 macOS 通知の実現方式（Q17）

requirements 21章で詳細設計事項とされている。`evaloomd` は launchd のユーザーエージェントとして動くため、本人のログインセッションに通知を出せる方式を選ぶ必要がある。候補（AppleScript の通知、通知用のヘルパーアプリ、コマンドラインの通知ツール）を実機で確かめて決める。

判断基準:

1. launchd から起動したプロセスから通知が表示されること
2. 通知の本文に機密情報を入れないこと（タスクの件名とエラー分類までにする）
3. 外部への送信を伴わないこと

**決定（2026-10-27、本人が実機で確認）: 通知用のヘルパーアプリを自作する。** 本人が「通知をクリックしたらタスク詳細を開けたい」と判断したため、クリックで管理画面を開けない AppleScript の通知（クリックするとスクリプトエディタが開く）は採らない。外部ツールへの依存を避けるため、コマンドラインの通知ツールも採らない。

| 項目 | 決めたこと |
|---|---|
| 作り | Swift の macOS アプリ（Info.plist に `LSUIElement` を指定し、Dock に出さない）。`UNUserNotificationCenter` で通知を出す |
| 呼び出し方 | `evaloomd` の macOS 用 Notifier が `/usr/bin/open -n -a <ヘルパーのパス> --args <件名> <本文> <タスク詳細の URL>` で起動する。ヘルパーは通知を出したあと一定時間待ってから終了する |
| クリック時 | ヘルパーが通知の応答を受け取り、`NSWorkspace` でタスク詳細の URL（`http://127.0.0.1:<port>/tasks/<taskId>`）を既定のブラウザで開く。タスクを持たない通知では Dashboard の URL（`http://127.0.0.1:<port>/`）を開く。開いてよい URL は 3.6 の許可範囲に限る |
| 置き場所 | `~/Applications/` 配下。`/tmp` に置いたヘルパーは許可を求める画面が出ないまま拒否されたため |
| 署名 | アドホック署名（`codesign --sign -`）。Developer ID は不要だった |
| 初回の許可 | 本人がシステム設定の「通知」でヘルパーの通知を1回許可する。許可していない間は、登録はエラーにならないが表示されない（`requestAuthorization` が許可なしを返す）。この状態を Notifier のエラーとして扱い、3.4 の失敗の記録に回す |
| ビルドと配置 | evaloomd の launchd 登録と同じ手順でビルド・署名・配置する。手順は backend/launchd/README.md に書く |

実機確認の条件: macOS 27.0.1。一時的な LaunchAgent（gui ドメイン）から上記の `open` で起動したヘルパーの通知が表示され、クリックで指定の URL がブラウザで開いた。

### 3.4 重複と失敗

- 同じ Execution の同じ状態について通知は1回だけ送る。送ったことを `notification_sent` イベントで記録し、再起動後に二重に送らない。
- 通知の送信に失敗しても、タスクの状態遷移は取り消さない。失敗は execution_events と Control Plane のログに残し、管理画面の Dashboard のエラー状況に出す。
- execution_events には `notification_failed` イベントを書く（[`02-data-model.md`](./02-data-model.md) 5.3）。payload は `kind`・`channel`・`reason` だけにし、エラーの文字列は入れない。エラーの詳細は Control Plane のログにだけ出す（2026-10-27 本人が決定）。

| `reason` | 当てはまる失敗 |
|---|---|
| `permission_denied` | 本人が通知を許可していない（ヘルパーが許可なしを返した） |
| `launch_failed` | ヘルパーを起動できない（パスが無い・`open` が失敗した） |
| `timeout` | ヘルパーの結果を待つ間に時間切れになった |
| `other` | 上のどれにも当てはまらない失敗 |

### 3.5 Dashboard を開く通知（Phase 2、2026-11-08 決定、Q77）

タスクに結び付かない出来事を知らせる通知は、クリックで管理画面の Dashboard（`/`）を開く。Dashboard のエラー状況に、その出来事の状況が出ているためである。

| 契機 | 通知の種類 | 本文に含めるもの | クリックで開く URL |
|---|---|---|---|
| Linear の照会が5回続けて失敗した、または照会が認証エラーになった（[`15-linear-intake.md`](./15-linear-intake.md) 6章。認証エラーは下の共通の抑止に従う、Q84） | Linear の照会の失敗 | 失敗の分類（`auth` / `rate_limited` / `network` / `other`）と、続けて失敗した回数 | `http://127.0.0.1:<port>/` |
| Linear コメントの送信が認証エラーで failed になった（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.4.3、2026-11-08 決定、Q84） | Linear コメントの送信の失敗 | 失敗の分類（`auth`） | `http://127.0.0.1:<port>/` |

- 本文には、表の項目のほかに API キーの値、Linear の応答の本文、Team ID を入れない。
- 1回の照会で複数の Team が失敗しても、通知は1回にまとめる。
- 認証エラーの通知の抑止は、取り込み側（Linear Poller）と送信側（送信 Worker）で共通の1つを持つ（2026-11-08 決定、Q84。[`16-linear-feedback.md`](./16-linear-feedback.md) 3.4.3）。同じ API キーが無効になって照会と送信の両方が認証エラーになっても、表の2つの通知のうち先に検出した側の1回だけを出す。照会か送信のどちらかが1回成功したら抑止を解く。送信 Worker の1回の処理で複数の行が認証エラーになっても1回にまとめる。
- 5回続けて失敗した通知と認証エラーの通知の分け方は案とし、Q86 に載せる。
- Linear コメントの送信の失敗の通知の本文に、Issue の識別子とコメントの本文を入れない（16 の 3.4.3）。
- この通知は Execution を持たないため、execution_events の `notification_sent` / `notification_failed`（3.4）には記録しない。送信に失敗したら Control Plane のログにだけ残す。照会の失敗そのものは Dashboard の取り込みの状況に出ている（[`15-linear-intake.md`](./15-linear-intake.md) 6章）。
- 呼び出し方は 3.3 と同じ（`open -n -a <ヘルパーのパス> --args <件名> <本文> <URL>`）で、3つ目の引数に Dashboard の URL を渡す。

### 3.6 ヘルパーが開いてよい URL の許可範囲（Q77）

ヘルパーは、渡された URL が次の条件をすべて満たすときだけ通知を出す。満たさなければ通知を出さず、結果を `invalid_arguments` として終了する。クリックの応答を受けたときも、通知に保存した URL を同じ条件で検査し直し、満たさなければ開かない。

| 項目 | 条件 |
|---|---|
| スキーム | `http` だけ |
| ホスト | `127.0.0.1` だけ（`localhost` などの別名は拒否する） |
| ポート | 指定があり、1〜65535 |
| ユーザー名・パスワード | 無い |
| クエリ・フラグメント | 無い |
| パス | `/tasks/<taskId>`（`<taskId>` は空でない）か、`/` だけ。ほかのパスは拒否する |

- 許可範囲を変えるときは、この表と evaloomd の Notifier が渡す URL を一緒に変える。
- 配置済みのヘルパーは、作り直して配置し直すまで古い許可範囲のままになる。Dashboard の URL を受け付けないヘルパーは 3.5 の通知を `invalid_arguments` で拒否する。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| Phase 1 で通知する場面 | タスクの完了と失敗（承認の仕組みが無いため）、本人の対応が必要なとき | requirements 8.8 |
| Phase 1 の通知先 | macOS 通知 | requirements 8.8 |
| 将来の通知先 | Slack、スマートフォン（追加できる構成にする） | requirements 8.8 |
| Gate 2・3 で止まったとき | 本人に通知する | requirements 8.8、条件15, 18 |

Linear コメントによる通知（requirements 8.8）は、Linear API キーの利用が Phase 2（requirements 6.6）のため Phase 1 では行わない案とする（Q18）。

## 5. インターフェース

- イベント: `notification_sent`、`notification_failed`（[`02-data-model.md`](./02-data-model.md) 5.3）
- 通知先の設定: `GET/PATCH /api/settings`（[`01-architecture.md`](./01-architecture.md) 5.1）
- 通知失敗の一覧: `GET /api/notification-failures`（本人用。[`01-architecture.md`](./01-architecture.md) 5.1、Q41）。Dashboard のエラー状況はこの API で表示する

#### 通知失敗の一覧 API（Q41）

- 対象: `execution_events` のうち `type = notification_failed` の行すべて。Execution・タスクの状態や、再実行の前か後かで絞らない。
- Phase 2 では、Execution の無い契機の Linear コメントの送信の失敗も対象にする（2026-11-08 決定、Q82）。linear_outbox のうち status=`failed` かつ execution_id が NULL の行である（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.4.2）。Execution のある契機の Linear の失敗は execution_events の `notification_failed` として返し、linear_outbox の行からは返さない。
- 並び順: 新しい順（`execution_events.id` の降順）。linear_outbox の行が混ざる Phase 2 では、記録した時刻（execution_events.created_at、linear_outbox.failed_at）の新しい順にし、同じ時刻の中は出所と ID で順を決める（2026-11-08 決定、Q82）。
- ページネーション: 01 の 5.3 の共通規約（`?limit=` 既定 50・上限 200、`?cursor=`、応答の `nextCursor`）に従う。linear_outbox の行が混ざる場合、cursor は上の並び順の位置を表す値にし（2026-11-08 決定、Q82）、2つの出所をまたいでも欠けも重複も出さない。
- 応答:

```json
{
  "items": [
    {
      "eventId": 123,
      "executionId": "...",
      "taskId": "...",
      "taskTitle": "...",
      "kind": "failed",
      "channel": "macos",
      "reason": "permission_denied",
      "createdAt": "2026-11-01T00:00:00Z"
    }
  ],
  "nextCursor": null
}
```

- `kind`・`channel`・`reason` は イベントの payload の値をそのまま返す（3.4）。エラーの文字列は返さない。
- linear_outbox の行から返す項目は、`eventId` と `executionId` を null にし、`outboxId` に linear_outbox.id を入れる。`kind` は linear_outbox.kind、`channel` は `linear`、`reason` は linear_outbox.failure_reason、`createdAt` は linear_outbox.failed_at を返す。execution_events から返す項目の `outboxId` は null にする。コメントの本文は返さない。
- 既読や解決済みの印は Phase 1 では持たない（記録は消さず、一覧に出し続ける）。

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| 通知の送信失敗 | 状態遷移は取り消さない。`notification_failed` イベントと Control Plane のログに記録して Dashboard に出す |
| 通知方式が使えない環境（ログインセッションが無い等） | 送信をあきらめ、記録する。再送はしない（案） |

## 7. スコープ外

- Linear コメント（Phase 2）
- Slack、スマートフォン（将来）
- 承認待ちの通知（Phase 2）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) イベント種別、Block 原因
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 通知の要求元
- [`10-verification-gates.md`](./10-verification-gates.md) Blocked の発生元
- [`13-management-ui.md`](./13-management-ui.md) Dashboard のエラー状況

## 9. 実装単位

- [ ] [Backend] Notifier インターフェースと、状態遷移からの通知要求（重複防止を含む）
  - 受け入れ基準: succeeded / failed / interrupted / timed_out / blocked で1回ずつ通知が要求される; cancelled では要求されない; 再起動後に同じ通知が二重に送られない
- [ ] [Backend] macOS 通知の実装（Q17 で採用した方式）
  - 受け入れ基準: launchd から起動した evaloomd から、本人のデスクトップに通知が表示される; 通知の本文にタスクの件名・ID・分類以外の内容が入らない
- [ ] [Backend] 通知失敗の記録
  - 受け入れ基準: Notifier がエラーを返してもタスクの状態が変わらず、失敗が execution_events に残る
- [ ] [Backend] ヘルパーが開いてよい URL の許可範囲の検査とテスト（3.5、3.6、Q77）
  - 受け入れ基準: `http://127.0.0.1:<port>/tasks/<taskId>` と `http://127.0.0.1:<port>/` を受け付ける; `/tasks/`（taskId が空）と `/settings`、`https`、`localhost`、`127.0.0.2`、ポートの無い URL・0・65536、ユーザー名付き、クエリ付き、フラグメント付きの URL を拒否する; 拒否した URL では通知を出さず結果が invalid_arguments になる; クリック時に保存した URL が条件を満たさなければ開かない; Linear の照会の失敗の通知で evaloomd が渡す URL が `http://127.0.0.1:<port>/` で、本文が分類と回数だけになる; 上記の受け付けと拒否をテストで確かめる
- [ ] （人間） 通知ヘルパーを作り直して Mac mini に配置し直す（3.3 の手順、backend/launchd/README.md）。配置済みのヘルパーは 3.5 の通知を拒否するため
- [ ] [Backend] 通知失敗の一覧 API（Q41）
  - 受け入れ基準: 複数の Execution に記録した notification_failed がすべて新しい順に返る; notification_failed 以外のイベントは返らない; limit を超える件数は nextCursor で続きを取れ、欠けも重複も無い; 応答に taskId と taskTitle が入り、エラーの文字列は入らない; Runner のトークンでは 403 になる
