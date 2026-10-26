# Harness APIの実装判断

正本は`docs/specs/03-registry.md`の3.4・5〜6節、
`02-data-model.md`のharnesses・audit_logs、`08-harness-adapter.md`の3.2節。
以下は仕様で未確定の部分の実装判断。

- エンドポイントは仕様のGET・POST `/api/harnesses`とPATCH `/api/harnesses/{id}`。
  POSTは201、GET・PATCHは200。全列をcamelCaseで返し、一覧には既存のID昇順の
  `items`・`nextCursor`形式を使う。個別GETは仕様にないため追加しない。
- POSTの必須項目は`id`・`name`・`adapter`・`repoPath`・`repoUrl`。
  IDは既存のslug検証を使う。Phase 1の`specialty`は`developer`、
  `supportedTasks`は`["development"]`、`contractVersion`は`"1"`だけを受け付け、
  省略時もこの値にする。対応モデルと利用可能ツールの省略値は`[]`。
  配列要素は空白以外を含む文字列とする。Model Registryは作らない。
- Tool Permissionsは省略時に`{}`とし、JSONオブジェクトとして保存する。
  内部キーの定義や実行への反映は追加しない。JSON数値は精度を維持して読み書きする。
- PATCHの省略項目は保持し、指定した配列・オブジェクトは丸ごと置き換える。
  null、未知の項目、ID・日時の変更は既存の400とdetails形式で拒否する。
  空のPATCHも再検証・監査記録を行う。共通のPATCH処理はProject APIから抽出した。
- Adapter名は既存の`adapter.Registry.Lookup`で検証する。
  未登録名は400 `validation_failed`、detailsは`adapter / invalid`。
  これは仕様01の入力検証エラーと既存のreasonに合わせた選択。
  登録表を`NewHandler`の第4引数に渡せる。省略時は空の登録表を使い、全ての名前を拒否する。
  この時点のリポジトリにはCodex・Claude Codeの具体的なAdapter実装がないため、
  架空の実装は登録しない。具体実装の追加時に、起動側からその登録表を渡す。
  SQLの固定2値制約は新しいマイグレーションで外し、追加登録した名前も保存できるようにした。
  既存のマイグレーションは変更しない。
- HarnessにはRunner IDがないため、登録済みの全RunnerのWorktree Rootに対して
  既存の`registry.ValidateRepository`を適用する。Runner未登録時は検証できないため
  422 `precondition_failed`、`worktreeRoot / not_found`で拒否する。
  Base Branch列はないため、既存検証のHarness用の省略動作を使う。
  path・urlに関するfield・reason、実パス正規化、検証期限は既存実装を使う。
- 同じHarness Registry内の同一実パスは409とする。Project Registryとの共有は禁止しない。
  保存トランザクション内でパス文字列とディレクトリの同一性を確認する。
  単一DB接続で重複確認と保存を直列化する。検証中にHarnessの値やRunnerのRoot一覧が
  変わった場合は409で拒否する。Git検証は全てトランザクションの外で行う。
- 保存と監査記録は同じトランザクションで行う。actorは`owner`、target_typeは`harness`、
  target_idはHarness ID。actionはProject APIにならって`harness.register`・`harness.update`。
  detailの`before`・`after`に全設定を記録し、登録時のbeforeはnullにする。
  URLの認証情報・クエリ・フラグメントを監査記録から除く処理はProject APIと共用する。
  監査記録に失敗したらHarnessもロールバックする。

Harness Profileの版とProjectへの割り当てはこの実装に含めない。
