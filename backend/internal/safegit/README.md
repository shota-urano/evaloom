# ホスト側 Git の実行境界

Trusted Runner と、今後実装する Git Publisher は `safegit.Output` を使う。
HTTP API、DB スキーマ、仕様書は変更していない。

## 無効化する設定

| 設定 | 実装と根拠 |
| --- | --- |
| `core.hooksPath` | コマンドごとに作る非公開の空ディレクトリへ向ける。checkout、commit、push、ref 更新などのフックを実行させない。 |
| `hook.*.enabled` | false にする。新しい Git の設定式フックも対象にする。 |
| `core.fsmonitor` | false にする。status や index 読み取りで任意の監視コマンドを起動させない。 |
| `core.sshCommand`, `ssh.variant` | `ssh -oBatchMode=yes` と `ssh` に固定する。リポジトリが指定した SSH コマンドを使わない。本人の信頼済み SSH 設定は引き続き利用する。 |
| `core.gitProxy` | 空にし、さらに `GIT_PROXY_COMMAND=` で上書きする。最初の一致を使う複数値設定なので、後置した `-c` だけでは防げない。 |
| `credential.helper`, `credential.*.helper`, `credential.interactive`, `core.askPass` | URL ごとの複数値 helper を空値でリセットし、対話を禁止する。askpass は `/usr/bin/false` に固定する。認証の外部プログラムをリポジトリに選ばせない。 |
| `filter.*.clean`, `.smudge`, `.process`, `.required` | コマンドを空にし、required を false にする。add、checkout、status などで変換コマンドを起動させない。変換前のバイト列を扱う。 |
| `diff.external`, `diff.*.command`, `diff.*.textconv`, `interactive.diffFilter` | `/usr/bin/false` に固定する。通常の diff/show/log には `--no-ext-diff --no-textconv` も指定し、Git 内蔵の差分を取得する。外部変換を明示的に要求した場合は失敗させる。 |
| `merge.*.driver` | `/usr/bin/false` に固定する。checkout の三方向マージなどで、任意のドライバを実行させず、競合として残す。 |
| `core.pager`, `core.editor`, `sequence.editor` | `--no-pager` を付ける。pager は cat、editor は `/usr/bin/false` に固定する。メッセージを指定しない commit ではエディタを開かず失敗する。 |
| `gpg.program`, `gpg.openpgp.program`, `gpg.x509.program`, `gpg.ssh.program` | `/usr/bin/false` に固定する。署名・検証プログラムをリポジトリに選ばせない。 |
| `commit.gpgSign`, `tag.gpgSign`, `push.gpgSign`, `log.showSignature`, `merge.verifySignatures` | false にする。設定による自動署名・署名検証を停止する。 |
| `remote.*.uploadpack`, `remote.*.receivepack` | config の上書きでは最初の値を排除できないため、fetch/clone/ls-remote の `--upload-pack`、push の `--receive-pack` で Git 標準プログラムに固定する。 |
| `remote.*.vcs`, `protocol.*.allow` | vcs 設定を検出したら実行前に拒否する。`GIT_ALLOW_PROTOCOL=file:git:http:https:ssh` で ext と任意名の remote helper を拒否する。URL 書き換え後にも適用される。 |
| `core.alternateRefsCommand` | `/usr/bin/false` に固定し、alternate の ref 列挙に任意コマンドを使わせない。 |
| `remote.*.promisor` | false にし、`GIT_NO_LAZY_FETCH=1` を指定する。オブジェクト読み取り中の暗黙の fetch を停止する。 |
| `submodule.recurse`, `fetch.recurseSubmodules`, `push.recurseSubmodules` | 再帰を無効にする。fetch/clone/push にはコマンドオプションも指定し、検査していないサブモジュールの設定を読み直させない。 |
| `maintenance.auto`, `gc.auto`, `gc.autoDetach`, `init.templateDir` | 自動保守を停止し、テンプレートを空ディレクトリに固定する。間接的な操作とテンプレートからのフック持ち込みを止める。 |

設定の意味と優先順位は [Git 公式の git-config 文書](https://git-scm.com/docs/git-config) に基づく。
名前付き設定は `include`、`includeIf`、有効な `config.worktree` も調べる。
`includeIf` は現在の条件にかかわらず対象ファイルを読む。worktree 作成時に
子プロセスの gitdir やブランチが変わっても、その設定のコマンドを使わせない。
相対 include のパスでは、シンボリックリンクと `..` の意味を保持する。
include の読み取りに失敗したら、Git 操作を開始しない。

継承した `GIT_*` と `SSH_ASKPASS` を取り除き、必要な環境変数だけを再設定する。
system/global config を読まず、親ディレクトリのリポジトリ探索も制限する。
Git の alias、外部サブコマンド、グローバルオプションは受け付けない。
プロセスのキャンセル時には Git と transport helper のプロセスグループを終了する。

## 呼び出し元の責任

- 引数と PATH は信頼できるアプリケーション側で組み立てる。
- Sandbox の書き込みプロセスを停止してから呼ぶ。設定の列挙と Git 実行の間に、別のプロセスが設定や include 先を変更する競合は、このラッパーだけでは排除できない。
- Git 領域へのアクセス制限、パス、remote の宛先、ref と commit の検証は、それぞれの担当処理で行う。
- 作業用クローンから登録URLへfetchするときは、`ValidateFetchURL`で取得元の変更設定を事前に拒否する。同名remote、`url.*.insteadOf`、`fetch.bundleURI`をincludeや有効なconfig.worktreeも含めて検査する。旧形式のremote定義とパスの検査は呼び出し元が行う。検査からfetch終了までSandboxの書き込みを停止しておく。
- Git Publisher の認証方式は Q29 の担当範囲で決める。リポジトリ内の credential helper を復活させて認証しない。

## 検証

`git_test.go` は一時リポジトリ、linked worktree、bare remote、ローカル TCP/HTTP
サーバを使う。実際に副作用ファイルを作るフックや実行設定を仕込み、
ラッパー経由では副作用がなく、通常の Git では副作用があることを比較する。
Git Publisher 本体は未実装のため、push は共通ラッパーから bare remote に対して検証する。
