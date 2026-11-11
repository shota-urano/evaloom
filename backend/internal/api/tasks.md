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
  現在の振り分け先の設定にかかわらず最初のタスクを返す。
- 振り分け先は `projectId` と `harnessId` のちょうど一方。省略と空文字は同じ扱いとし、
  どちらも無い・両方あるときは400、詳細は `body/exactly_one_destination`（routes API と同じ）。
- 未登録の振り分け先は422 `precondition_failed`、詳細は `projectId/not_found` または
  `harnessId/not_found`。`default_profile_id` の無いスペシャルハーネスは
  `harnessId/default_profile_missing` の422。
- 受け入れ条件は、振り分け先の Profile（projects.profile_id または
  harnesses.default_profile_id）の定義が `acceptanceCriteria: required` のときだけ
  空白のみを `acceptanceCriteria/required` の400にする。それ以外は空文字列で保存する。
  titleとbodyは空白だけの入力を拒否し、非空の本文や受け入れ条件は加工せず保存する。
- 応答の `harnessId` は直接の振り分けのときだけ値を持ち、それ以外は空文字列
  （`projectId` と同じ表し方）。
- shortNameは案の英小文字・数字・ハイフンの1〜40文字を採用する（02 3.3 で列が無くなったため保存しない。
  管理画面が送るので入力の検証だけ残している）。
  省略またはnullの場合は `task`。明示的な空文字は400。
- タスクIDは同じ作成トランザクションで既存IDの数値部分の最大値に1を加え、
  最小3桁で表す。例は `task-001`、`task-1000`。削除APIは追加しない。
  Execution IDは既存のULIDライブラリで生成する。
- 作成時の監査記録は `owner/task.create/task/<task ID>` とし、
  Project ID または Harness ID・Execution ID・タスクの遷移前後を残す。
  Executionの `status_changed` イベントにもタスクの `received` → `queued` と
  `reason=task_created` を残す。本文とIdempotency-Keyは記録しない。
- 既存ストアの単一接続による直列化の中で、キー検索・振り分け先とProfileの確認・作成・
  `QueueExecutionTx` による遷移・記録を行う。再送は行を追加しない。
  どの書き込みで失敗しても全体をロールバックし、共通形式の500を返す。

# タスク一覧API

`GET /api/tasks` は管理画面の Tasks 一覧（`13-management-ui.md` 3.2）のために追加した。
仕様は一覧 API の存在（`01-architecture.md` 5.1）だけを定めるので、次を実装判断とする。

- 絞り込みは `?status=`（`02-data-model.md` 5.1 の値）と `?projectId=` の完全一致。
  どちらも省略または空文字なら絞り込まない。不正な値と複数指定は400。
- 並びはタスクIDの数値部分の降順（新しい順）。`task-1000` は `task-999` より前に来る。
  ページングは共通の `limit` / `cursor`。カーソルは直前ページ最後のタスクIDを持つ。
- 各要素はタスクの列に加えて `latestExecution`（seqが最大のExecutionの
  `id`・`seq`・`mode`・`status`・`errorClass`。無ければnull）を返す。
  PR URL は software-engineer へ移したため返さない（`docs/specs/19-routing-and-stages.md` 3.11）。
