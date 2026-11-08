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

## 認証情報の分離

`NewGitHubApp` はホスト側から秘密鍵を受け取り、RS256 の JWT を署名して、
Q29 の App ID / Installation ID でインストールアクセストークンを取得する。
取得時は対象リポジトリ1件と contents: write、pull_requests: write、metadata: read
を指定する。応答の対象・権限・期限も確認する。JWT の時刻条件は
[GitHub のプロトコル](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app)
に従う。鍵・JWT・トークンを保存、ログ出力、親プロセスの環境変数へ設定しない。
REST と Git のどちらも、別のリポジトリには認証情報を渡さない。

呼び出し側は同じ `GitHub` を `PublishConfig.PullRequests` と `PushConfig.Git` に渡す。
`PrepareGit` で認証を済ませてから、既存の中止・HEAD・作業ツリーの最終確認を行う。
GitHub への push はこの境界を必須とする。匿名のテスト用 remote には従来の
`git://` を使える。SSH 形式の Project URL は保存せず HTTPS へ変換する。

認証付き Git は `safegit.HTTPSOutput` を使う。一時的な bare リポジトリから
元のオブジェクト領域だけを読み、worktree の config、include、HTTP 設定、
credential helper、フックを参照しない。トークンは Git 子プロセスだけの環境変数から
HTTP 認証ヘッダーへ渡す。URL・引数・ファイルには書かない。ホストの HOME、
SSH agent、プロキシ、Git/curl のトレース設定は継承せず、TLS 証明書を検証する。
リダイレクトに従わず、失敗時は Git の標準出力・標準エラーを返さない。

`TestPublisherCredentialsNeverReachWorktreeSandboxLogsOrDB` は、偽の App 鍵と
固有の偽トークンを使う。REST は偽 transport、Git はローカル TLS + 実際の
`git http-backend` に接続し、push された SHA と SQLite の保存結果を確認する。
正常な PR 作成・コメント、鍵取得失敗、認証拒否、push 拒否、Git のリダイレクト、
REST 通信失敗、中止を通す。鍵・JWT・トークン・Basic 認証値について、worktree と
共有 `.git` の全ファイル、Sandbox 呼び出しの引数と環境、ログ、SQLite の全テーブルと
DB/WAL/SHM ファイルを検索する。Sandbox は既存 Container の偽 runtime を使い、
Harness / setup / test の呼び出しを観測する。実 OS の隔離突破試験は仕様14の別タスク。

## 読み取り用の認証

`NewGitHubAppReader` は、仕様11の3.6に従い、対象リポジトリ1件と
`contents: read`・`metadata: read`・`pull_requests: read` だけを要求する。
書き込み用の `NewGitHubApp` とはトークンを共有しない。
`Reader.ForStage` を工程ごとに呼び、返された `ReadSession` を工程内だけで使う。
工程1は `Output` の `ls-remote`、工程2は `State`、工程3は
`worktree.ReadTransport` として使える。`ForStage` の除外登録関数は必須で、
トークンとGitのBasic認証値を、使用前にRunnerのログ除外対象へ登録する。

工程3の新規作成では `Execution.PrepareReadRepository` に `Reader` と
`worktree.Repositories` を渡す。返されたRepositoriesで `RecordRemoteBase` と
`CreateWorktree` を行い、工程終了後は保持しない。認証情報の発行やcloneが
失敗した場合は `failed`（`config_error`）を報告する。
後続のログは `Runner.NewLog` の除外値に `Execution.LogSecrets()` を渡す。
起動コマンドから各工程への組み立ては、別タスク `evaloom-1b9` が担当する。

Gitは `safegit.HTTPSReadOutput` を使う。認証を伴う処理は新しい一時領域から
実行し、既存クローンのHTTP設定・include・helper・フックを読まない。
fetchは一時リポジトリで取得した後、トークンを渡さないGit処理で
追跡refとオブジェクトを作業用クローンへ取り込む。
cloneはcheckoutとtemplateを無効にし、登録済みのorigin URLだけを保存する。
SSH形式のURLも通信時はHTTPSへ変換する。

`TestReadTokenPermissionsAndAuthenticatedStage3Isolation` はトークン発行要求、
認証必須のGit HTTPサーバーに対するclone・fetch・ls-remote、設定ファイルと
worktree、Sandboxの環境、Runnerのログ・eventsを確認する。
`TestStage3ReadTokenFailuresReportConfigError` は認証失敗や不正な応答を拒否し、
実際のControl PlaneとSQLiteで `failed`・`config_error` を確認する。
Sandboxの環境確認には既存Containerと偽runtimeを使う。OSの隔離試験は別タスクである。

仕様書ではQ70が案と表記されている。本タスクの指示と受け入れ基準に従って
3.6の方式を実装しており、仕様書は変更していない。

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
- 秘密鍵の配置ディレクトリは仕様11で `~/.config/evaloom/` と確定しているが、
  ファイル名、ホスト設定からの指定方法、Runner の起動時の組み立ては未定義。
  `NewGitHubApp` の鍵取得関数で分離し、設定フィールドや既定のファイル名は追加しない。
  鍵取得関数はホスト側で実装し、Sandbox のマウントから読まないこと。
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

PR のクローズは実装しない。

## 承認済みのマージ（仕様17の3.5.2）

`merge.go` の `PullRequest` は PR の head SHA・base ブランチ・open/merged を読み、
`Merge` は `PUT /repos/{repo}/pulls/{n}/merge` に `sha`（approvals.content_version）と
`merge_method`（consume の応答の `mergeMethod`）を付けて1回だけ送る。自動で再送しない。
応答の分類は、200 で `merged=true` と マージ commit の SHA があれば成功、409 は
`ErrMergeContentChanged`（head が sha と違う）、その他の 4xx は `ErrMergeRejected`、
送信後の通信失敗・5xx・読めない応答は `ErrMergeUnknown`（結果が分からない、3.5.3）。
送信前の失敗（トークン取得失敗など）はマージされていないため、そのエラーを返す。
Runner 側の手順は `internal/runner/operation.go`。
