# Harness Contextの読み込み

正本は`docs/specs/07-context.md`の3.3・6節、`03-registry.md`の3.5節、
`02-data-model.md`の4.3・5.4節。

`Read(ctx, repo, commitSHA, paths)`は、Executionに固定したProfileのcommit SHAと、
読み込むファイルの相対パス一覧を受け取る。タグやHEAD、作業ツリーの定義を読み直さない。
`registry.ReadProfile`が解決した`HarnessProfile.CommitSHA`をそのまま渡せる。

仕様はProfile定義によるファイル選択を求めるが、現行の4.3のスキーマにはパス指定欄や
版からパスへの対応規約が無い。この読み込み処理では、呼び出し側から明示した一覧を
渡す。`promptVersion`や`skillVersions`から配置を推測せず、Profileのスキーマも変更しない。
Profile定義からの選択はContext Builderの実装時に接続する。

Project Contextの既存処理を`internal/gitcontext`へ移し、両方の読み込みで共用する。
固定commitのtreeから対象のblob IDを取得するため、`git show <SHA>:<path>`と同じ内容を、
改行やバイナリの変換なしで読める。パス順に、内容・小文字のSHA-256・読み取り元SHAを返す。
Gitのreplace参照と継承した`GIT_*`による別リポジトリへの切り替えも無効にする。

パスはリポジトリ内の正規化済み相対パスで、個別の通常ファイルを指定する。
ワイルドカードは展開せず、同名のファイルだけに一致する。重複は1件にまとめる。
ディレクトリ・シンボリックリンク・サブモジュールは読み込まない。
空の一覧でもSHAの存在を確認する。

SHAや指定ファイルの欠落、通常ファイル以外の指定、Gitの失敗時は、途中の結果を返さず
`ConfigError`を返す。`Status()`は`failed`、`ErrorClass()`は`config_error`である。
中止・期限超過の元エラーは`errors.Is`で確認できる。
Runnerは中止方針を先に適用し、それ以外の読み込み失敗をこの分類で報告する。
このパッケージはDB更新やSandboxへの配置を行わない。保存・context API・専用設定環境への
配置は、それぞれ仕様の別実装単位で扱う。
