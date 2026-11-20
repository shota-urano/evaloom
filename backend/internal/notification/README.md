# 通知要求と送信

正本は`docs/specs/12-notification.md`の3.1・3.2・3.4・6節と、
`04-task-lifecycle.md`の3.4・3.6節。

終端通知の要求は既存の`task.TransitionExecutionTx`で保存する。
Runnerの結果報告、Context Builderのfailed・blocked、heartbeatのbootId変更、
起動時の途絶回収はこの関数を共有する。状態・要求・イベント・監査記録の
トランザクションが失敗した場合、通知要求も取り消される。
`cancelled`では要求を作らない。
判断待ちは、状態遷移から切り離し、commit済みの`owner_decisions`の`open`行を要求として読む。
`blocked`や`waiting_approval`への遷移だけでは通知しない。
過去の実装が`notification_requests`に残した、この2状態の要求も送信対象から除く。
同じ報告や回答で作った承認は、`decision_opened`の`notificationBatch`でまとめる。
最初に作った判断待ちの詳細を開き、送信済み記録は各行に残す。
更新や再開の後に別の判断待ちができたら、同じExecutionでも新たに通知する。
`withdrawn`と`answered`の行は送信対象にしない。

送信には`notification.Notifier`を使う。`Notify(ctx, Notification) error`は
エラーを返せる。Control Planeは`Channel{Name, Notifier}`で送信先を登録し、
`Dispatcher`が登録先すべてに要求する。ある送信先が失敗しても残りを呼ぶ。
macOSでは`MacOS`を`macos`経路として登録する。ヘルパーのビルド・署名・配置と
許可拒否の結果受信は`backend/launchd/README.md`を参照する。
通知要求の本文は仕様3.1に従う。macOSの本文はQ17と実装単位の受け入れ基準に従い、
件名・ID・分類（判断待ちの種類と検証済みの原因を含む）だけから組み立て直す。
出力ラベル・残りの件数・停止した段・理由・組み立て済みの本文はmacOSへ渡さない。
失敗した送信先ごとに`notification_failed`イベントを保存する。
payloadは`kind`・`channel`・`reason`だけとし、エラー詳細はControl Planeのログに出す。
`reason`は許可なし・起動失敗・時間切れのsentinelとdeadlineから分類し、
該当しないエラーは`other`にする。エラーの文字列で分類しない。
Dashboardへの表示は管理画面の別タスクで扱う。
送信先が未登録の間は要求を未処理のまま残す。

仕様に記載がない部分の実装判断は次のとおり。

- `evaloomd`は送信処理を起動時と250ミリ秒ごとに実行する。
  1回に最大100要求を処理し、終了時は処理の停止を待ってからDBを閉じる。
- 終端通知は`(execution_id, status)`の主キーで重複を防ぐ。
  判断待ちは、Executionの状態ではなく判断待ちのIDで重複を防ぐ。
- マイグレーション0007で`notification_requests.attempted_at`を追加する。
  `BEGIN IMMEDIATE`で未処理の要求を1件選び、送信前にこの時刻をcommitする。
  Notifierの呼び出し中にはDB接続やトランザクションを保持しない。
  時刻のある要求は、失敗やプロセス再起動後も再送しない。
- 送信に成功した送信先ごとに`notification_sent`イベントを記録する。
  payloadの`status`と`kind`は要求のExecution状態、`channel`は登録名とする。
  同じ`status`の送信済みイベントがある要求も処理対象から除外する。
  送信成功後にイベントの保存が失敗しても`attempted_at`で再送を防ぐ。
- Executionのある判断待ちは、`notification_sent`の`kind=decision`と
  `decisionId`で送信済みを記録する。振り分けの判断待ちは、仕様12のQ148に従い
  `audit_logs`の`notification.sent`と`target_type=owner_decision`で記録する。
  どちらも送信前にDBで記録の有無を確かめ、送信に成功したら記録する。
  再起動後もこのDBの記録で送信済みの行を除外する。
  同一プロセスの複数の判断待ち送信処理は、DBの照会から記録までロックで直列化する。
  送信や記録に失敗した後の繰り返しを避けるため、同じDispatcherでは再試行しない。
  この抑止は永続化しておらず、再起動後の重複判定には使わない。
- DBのcommitと外部への通知は原子的に実行できない。
  処理開始時刻の保存直後にプロセスが停止すると、通知が未送信のままになる
  場合がある。再送による重複を避けるため、この場合も再送しない。
  `attempted_at`は送信成功を意味せず、`notification_sent`とは区別する。
- 通知情報は要求のcommit後に読み、要求に保存した状態を使う。
  成功通知は最初のoutputのlabelと残りの件数を読み、urlは読み込まない。
  判断待ちはID・件名・種類・原因だけを読み、質問本文・選択肢・target・finding・
  summaryは読み込まない。クリック先は`/decisions/<id>`とする。
- Notifierと結果イベント保存のエラーは送信処理の呼び出し元へ返す。
  結果イベントは、送信中にキャンセルされても最大5秒待って保存する。
  状態遷移は既にcommitされており、エラーで取り消さない。

仕様の穴は、判断待ちの送信成功から成功記録の保存までの間に停止した場合の重複である。
仕様12の3.4とQ148は「送ってから記録する」方式なので、この間の停止や記録の保存失敗では、
再起動後に同じ判断待ちを再送する可能性が残る。送信前の予約の保存先は仕様に無いため、
新しいテーブル・列・監査action・公開インターフェースは追加しない。
記録の保存に失敗したら、判断待ちID・通知先・送信成否とDBエラーを呼び出し元へ返し、
Control Planeのログに出す。questionとrouteのテストで、保存失敗時のログ、
同じDispatcherでの再試行の抑止、DBを開き直した後の再送、成功記録保存後の重複防止を確かめる。

仕様12の3.1は出力ラベルや停止した段を本文に含める一方、Q17とmacOS実装単位の
受け入れ基準は本文を件名・ID・分類までに限定している。この差は仕様の穴として残し、
macOSでは後者を適用する。確定済み仕様書と、ほかの送信先へ渡す通知要求は変更しない。
