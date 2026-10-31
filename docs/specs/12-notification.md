---
status: confirmed
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

通知をクリックしたら管理画面のタスク詳細を開く（3.3 の方式で実現する）。

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
| クリック時 | ヘルパーが通知の応答を受け取り、`NSWorkspace` でタスク詳細の URL（`http://127.0.0.1:<port>/tasks/<taskId>`）を既定のブラウザで開く |
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
