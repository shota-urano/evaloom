# SQL migrations

このディレクトリの SQL はバイナリに埋め込まれ、`store.Open` が起動時に適用する。
ファイル名は `0001_registry.sql` のように、正の整数と `_` と名前を組み合わせる。
番号は重複させず、既存の最大番号より大きい番号を追加する。適用済みのファイルは変更しない。

未適用のファイルは番号の昇順に実行する。各ファイルの SQL と
`schema_migrations` の適用履歴は、同じトランザクションで保存する。
SQL に `BEGIN`、`COMMIT`、`ROLLBACK` やトランザクション外での実行が必要な操作を含めない。
テーブルの再作成に備え、基盤が専用接続で外部キー検査を一時停止し、
各ファイルのコミット前に `PRAGMA foreign_key_check` で全参照を検証する。
適用に成功しても失敗しても、基盤が外部キー検査を再開する。
失敗したファイルの変更は取り消し、起動処理にエラーを返す。
それ以前に適用したファイルは保持し、次回起動時に未適用分から再開する。

業務テーブルは各 SQL ファイルで追加する。`schema_migrations` は基盤が作成する。

`evaloomd` の起動例は `go run ./cmd/evaloomd -db /existing/data/evaloom.db`。
DB の保存先には明示したファイルパスを使い、親ディレクトリは事前に作成する。

0026は登録の5表を仕様02の形に移す。Projectには旧`default_role`の割り当てを引き継ぎ、
ラベルを`routes`へ移す。旧設定の照会先Teamが1つなら`settings.linear.teamId`に引き継ぐ。
旧ラベルを使っていて接頭辞の設定行が無い場合は、旧既定値`repo:`を設定行に保存する。

旧Projectの既定の役割に割り当てが無い場合、同じラベルを異なるTeamで使っていた場合、
複数の旧Teamを単一の全体設定へ移せない場合は、0026全体を取り消して起動を失敗させる。
架空のHarnessやProfile、振り分け先を補って移行はしない。
`runners.worktree_root`は削除し、Runnerの設定ファイルに作業領域のディレクトリを持たせる。

0027は仕様02のタスクと実行の7表を作る。旧Executionの`running`は`working`、
`verifying`は`checking`、旧Taskの`verifying`は`running`へ移す。
旧エラー分類の`execution_failed`は`work_failed`、`test_failed`は`check_failed`へ移す。
`harness_session_id`は`work_session_id`、`source_commit_sha`は`source_revision`へ引き継ぐ。
保存済みのProfileの固定値は引き継ぐが、旧表に無かったclaim時点のparamsと契約版は、
現在の登録値で補わずNULLのままにする。SSEイベントの採番は削除済みIDも含めて継続する。

移行先を一意に決められない旧値（`planning`、`merging`、旧取り込み保留原因、
廃止したイベント種別、`project`の文脈など）が残っていれば、0027全体を取り消す。
元の行を削除したり、振り分けの提案や段の実行結果を推測で作ったりしない。
JSONの内容、振り分け先未定を許すタスク状態、Profileに応じた受け入れ条件の必須判定は、
仕様どおりアプリ側の検証で扱う。
