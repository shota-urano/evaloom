# 通知要求と送信

正本は`docs/specs/12-notification.md`の3.1・3.2・3.4・6節と、
`04-task-lifecycle.md`の3.4・3.6節。

通知要求は既存の`task.TransitionExecutionTx`で保存する。
Runnerの結果報告、Context Builderのfailed・blocked、heartbeatのbootId変更、
起動時の途絶回収はこの関数を共有する。状態・要求・イベント・監査記録の
トランザクションが失敗した場合、通知要求も取り消される。
`cancelled`では要求を作らない。

送信には`notification.Notifier`を使う。`Notify(ctx, Notification) error`は
エラーを返せる。Control Planeは`Channel{Name, Notifier}`で送信先を登録し、
`Dispatcher`が登録先すべてに要求する。ある送信先が失敗しても残りを呼ぶ。
macOSでは`MacOS`を`macos`経路として登録する。ヘルパーのビルド・署名・配置と
許可拒否の結果受信は`backend/launchd/README.md`を参照する。
失敗した送信先ごとに`notification_failed`イベントを保存する。
payloadは`kind`・`channel`・`reason`だけとし、エラー詳細はControl Planeのログに出す。
`reason`は許可なし・起動失敗・時間切れのsentinelとdeadlineから分類し、
該当しないエラーは`other`にする。エラーの文字列で分類しない。
Dashboardへの表示は管理画面の別タスクで扱う。
送信先が未登録の間は要求を未処理のまま残す。

仕様に記載がない部分の実装判断は次のとおり。

- `evaloomd`は送信処理を起動時と250ミリ秒ごとに実行する。
  1回に最大100要求を処理し、終了時は処理の停止を待ってからDBを閉じる。
- `(execution_id, status)`の主キーによる既存の重複防止を維持する。
  同じExecutionが続行後に再びblockedになっても要求を追加しない。
  違う終端状態への遷移と、新しいExecutionにはそれぞれ要求を作る。
- マイグレーション0007で`notification_requests.attempted_at`を追加する。
  `BEGIN IMMEDIATE`で未処理の要求を1件選び、送信前にこの時刻をcommitする。
  Notifierの呼び出し中にはDB接続やトランザクションを保持しない。
  時刻のある要求は、失敗やプロセス再起動後も再送しない。
- 送信に成功した送信先ごとに`notification_sent`イベントを記録する。
  payloadの`status`と`kind`は要求のExecution状態、`channel`は登録名とする。
  同じ`status`の送信済みイベントがある要求も処理対象から除外する。
  送信成功後にイベントの保存が失敗しても`attempted_at`で再送を防ぐ。
- DBのcommitと外部への通知は原子的に実行できない。
  処理開始時刻の保存直後にプロセスが停止すると、通知が未送信のままになる
  場合がある。再送による重複を避けるため、この場合も再送しない。
  `attempted_at`は送信成功を意味せず、`notification_sent`とは区別する。
- 通知情報は要求のcommit後に読み、要求に保存した状態を使う。
  Task ID・件名・Execution ID、保存済みPR URL・エラー分類・理由を渡す。
  Block原因は最初のBlockから、停止工程は対象状態への最初の
  `status_changed.from`から取得する。本文・Context・ログは読み込まない。
  各通知方式が表示項目を選び、macOSは仕様12の3.3節の制限に従う。
- Notifierと結果イベント保存のエラーは送信処理の呼び出し元へ返す。
  結果イベントは、送信中にキャンセルされても最大5秒待って保存する。
  状態遷移は既にcommitされており、エラーで取り消さない。
