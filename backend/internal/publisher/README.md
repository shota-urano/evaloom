# Git Publisher の PR 処理

正本は `docs/specs/11-git-publisher.md` の3.1〜3.3・5〜6節。
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

この変更は仕様11の3.4の前回 PR の状態確認や、PR のクローズ・マージを実装しない。
