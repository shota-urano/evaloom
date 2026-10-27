# 手動タスク作成API

正本は `docs/specs/04-task-lifecycle.md` 3.1・6節、
`02-data-model.md` 3.1・3.2、`03-registry.md` 3.6。
`POST /api/tasks` は既存の本人用認証・CSRF・Origin・Host検証を通す。

仕様にない部分と案の実装判断は次のとおり。

- 作成成功は201、再送は200。どちらもタスクの各列をcamelCaseで返す。
  `idempotency_key` は応答に含めない。再送には保存済みタスクの現在の状態を返す。
- `Idempotency-Key` は必須とし、空白だけの値と複数ヘッダを400で拒否する。
  キーはDBの一意制約に合わせて全Project共通とし、有効期限を設けない。
  JSONと入力形式を検証した後、同じキーの既存行があれば、入力内容の違いや
  現在のDefault Harness割り当てにかかわらず最初のタスクを返す。
- 未登録の `projectId` は400 `validation_failed`、詳細は `projectId/not_found`。
  titleとbodyも空白だけの入力を拒否し、非空の本文や受け入れ条件は加工せず保存する。
- shortNameは案の英小文字・数字・ハイフンの1〜40文字を採用する。
  省略またはnullの場合は `task`。明示的な空文字は400。
- タスクIDは同じ作成トランザクションで既存IDの数値部分の最大値に1を加え、
  最小3桁で表す。例は `task-001`、`task-1000`。削除APIは追加しない。
  Execution IDは既存のULIDライブラリで生成する。
- 作成時の監査記録は `owner/task.create/task/<task ID>` とし、
  Project ID・Execution ID・タスクの遷移前後を残す。
  Executionの `status_changed` イベントにもタスクの `received` → `queued` と
  `reason=task_created` を残す。本文とIdempotency-Keyは記録しない。
- 既存ストアの単一接続による直列化の中で、キー検索・割り当て確認・作成・
  `QueueExecutionTx` による遷移・記録を行う。再送は行を追加しない。
  どの書き込みで失敗しても全体をロールバックし、共通形式の500を返す。
