# Harness Contextの読み込み

正本は`docs/specs/07-context.md`の3.2・6節、契約の4.1・4.2・5.2・6.3節、
`docs/specs/09-sandbox.md`の3.7節。

`Read(ctx, repo, commitSHA, paths)`は、Executionに固定したProfileのcommit SHAと、
読み込むファイルまたはディレクトリの相対パス一覧を受け取る。タグやHEAD、作業ツリーの定義を読み直さない。
`registry.ReadProfile`が解決した`HarnessProfile.CommitSHA`をそのまま渡せる。

Runnerは固定した登録済み定義の`stages.work.instructions`・`skills`・`settings`を
`paths`に渡す。読み出したファイルだけを`harnessenv.Create`で専用設定環境に置く。
`POST /api/executions/{id}/context`にはディレクトリ名ではなく、配下のファイルごとに
kind・相対パス・SHA-256を送る。重複や重なる宣言は同じkindとパスにつき1件にする。

Git の読み込みは`internal/gitcontext`で行う。
固定commitのtreeから対象のblob IDを取得するため、`git show <SHA>:<path>`と同じ内容を、
改行やバイナリの変換なしで読める。パス順に、内容・小文字のSHA-256・読み取り元SHAを返す。
Gitのreplace参照と継承した`GIT_*`による別リポジトリへの切り替えも無効にする。

パスはリポジトリ内の相対パスで、契約に従って末尾の`/`や`./`を正規化する。
ワイルドカードは展開せず、同名のファイルまたはディレクトリだけに一致する。
ディレクトリは配下の通常ファイルを再帰的に読み、重複は1件にまとめる。
シンボリックリンク・サブモジュールは配下に含まれる場合もエラーにする。
空の一覧でもSHAの存在を確認する。

SHAや指定ファイルの欠落、通常ファイル以外の指定、Gitの失敗時は、途中の結果を返さず
`ConfigError`を返す。`Status()`は`failed`、`ErrorClass()`は`config_error`である。
中止・期限超過の元エラーは`errors.Is`で確認できる。
Runnerは中止方針を先に適用し、それ以外の読み込み失敗をこの分類で報告する。
このパッケージはDB更新やSandboxへの配置を行わない。それぞれ既存のcontext APIと
`harnessenv`が担当する。
