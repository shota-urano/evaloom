# Git Publisher の PR 処理

正本は `docs/specs/11-git-publisher.md` の3.1〜3.4・5〜6節。
`Publish` は既存の `Push` を呼び、リモートの SHA が一致した後に
`PublicationRecorder.RecordPushed` の保存成功を待って PR 処理へ進む。
PR 作成に失敗しても、返り値には確認済みの `PushedSHA` が残る。

同じリポジトリの同じ head ブランチについて、開いている PR を検索する。
検索では base を限定しない。既存の PR が別の base を指していても、
新しい PR を作らないためである。0本なら Project の Base Branch に向けて作成し、
1本なら Execution の結果をコメントする。複数本や検索失敗時は
`publish_failed` で停止する。外部操作は自動で再送しない。
作成後の記録に失敗した場合も、continue の再実行で同じ PR を検索する。

PR 本文とコメントは同じ `Report` から作る。Gate 2 の実行数は
`test_runs.total - skipped`、skip 数は `skipped` とする。
保存した Finding の `count_only_check` があれば「件数による確認」、
`test_report_missing` があれば「照合できない」と書き、それ以外は
「ID による照合」と書く。本人が続行を選んだ Block の内容は
`ContinuedBlocks` で受け取り、ハーネスからの申告では埋めない。

GitHub との外部境界は `PullRequests`。実装の `GitHub` は REST API で
[PR の検索・作成](https://docs.github.com/en/rest/pulls/pulls)と
[通常の PR コメント](https://docs.github.com/en/rest/issues/comments#create-an-issue-comment)を行う。
GitHub App のインストールアクセストークンを提供する関数と HTTP client を
Trusted Runner から渡す。App の鍵やトークンを worktree から読み出さない。
API の転送には従わず、応答本文・認証情報・通信エラーの自由文を返さない。
テストは偽の `PullRequests` と HTTP transport、およびローカルの Git remote を使う。

## 再実行前の PR 確認

`CheckPreviousPR` は Runner の準備工程2で呼ぶ内部境界である。
mode と前回 PR 番号を受け取り、`PreviousPullRequest.State` で現在の状態を取得する。
GitHub の実装は PR 番号を指定して取得し、`state` と `merged` の両方を確認する。
番号が無い場合だけ「まだ無い」と扱う。404、認証失敗、通信失敗、欠けた応答は
`publish_failed` で停止し、外部エラーの自由文を返さない。

正常時の `RerunPlan.NewBranch` は `fresh` の場合だけ true となる。
後続の worktree 工程が仕様06に従ってブランチを作成または再利用する。
停止時の `RerunError` は、報告すべき Execution 状態、error_class、Block 原因、
仕様にある理由コードを保持する。`ErrorClass` はこの分類を引き継ぐ。
マージ済みの場合は新しいタスクの作成を案内する。
停止する場合は worktree 準備やハーネスへ進まず、PR のクローズ・マージ、
ブランチ削除、push は行わない。テスト名 `TestCheckPreviousPRSpec3_4Table` の
8ケースが、仕様3.4の各行・各列に対応する。

## 未定義の接続部分

- 仕様11の3.2は、PR 作成前に `pushed_sha` の保存を求める。既存の
  `POST /api/executions/{id}/result` は基準値の測定を除いて状態遷移を必須とし、
  publishing 中のメタデータ単独保存を受け付けない。この仕様の穴は
  `PublicationRecorder` で分離した。新しい公開 API、要求フィールド、
  状態遷移、テーブル、列は追加していない。
- 記録側は Execution の識別情報と配送用 context を保持し、`RecordPushed` で
  `pushed_sha` と `pushed` イベント、`RecordPR` で `pr_number`・`pr_url` を保存する。
  新規作成時だけ `pr_created` イベントを送る。保存できるまでは成功を返さない。
  イベントの payload は仕様02の5.3に従う。この記録側の HTTP 接続は未実装。
- claim の応答には、実際に使ったハーネスの版と、本人が続行を承認した
  Block の全内容が揃っていない。これらは `Report` の入力として受け取り、
  取得方法を創作しない。現時点の Runner の工程組み立て・実行ループへの接続は
  既存の `Push` と同じく呼び出し側に属する。
- GitHub App の鍵からインストールアクセストークンを取得する処理は、
  認証情報の分離の実装単位に属する。ここではトークン提供関数で分離する。
- 現在の claim 応答は `fresh` で `previous=null` を返すため、前回 PR 番号の
  有無を判定できない。仕様05の工程2も対象を `continue` と記す一方、仕様11の
  3.4は `fresh` でも状態確認を求めている。`CheckPreviousPR` はどちらの mode でも
  前回 PR 番号を渡せる入力境界とし、取得経路は創作しない。呼び出し側はタスクの
  前回 PR を確認してから番号または nil を渡す。fresh の `previous=null` を
  そのまま「前回 PR が無い」という根拠にしてはならない。
  `continue` も直前の Execution の `pr_number` しか返さないため、直前の実行で
  PR が未記録なら、さらに前の実行で作成した PR の確認経路が不足する。
- 開いた PR を理由に準備工程2で `blocked(git_state)` とするとき、既存の result API は
  Block 時点の HEAD を必須とするが、この時点では worktree 準備前であり、claim も
  fresh の前回 worktree 情報を渡さない。判定は `RerunError` に分離し、報告時の HEAD の
  取得と工程2への接続は呼び出し側に残す。仮の SHA、API、列は追加しない。

PR のクローズ・マージは実装しない。
