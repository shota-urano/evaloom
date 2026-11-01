# Claude Code Adapter

仕様の正本は`docs/specs/08-harness-adapter.md`。共通の`adapter.Adapter`を実装する。
Codex Adapterと同じ起動・セッション・中止の構造を使い、既存のCodex実装は変更しない。
ホスト上でCLIを直接起動する処理は持たない。自動テストは偽CLIと偽コンテナruntimeを使う。

## 起動と結果

採用対象はローカルで`--version`と`--help`を確認したClaude Code 2.1.316。
同じHarness Sandboxのworktreeと専用設定環境で、次の順に実行する。

1. `/bin/sh -c 'exec "$@"' evaloom-claude-preflight claude --setting-sources user,project --settings <settings JSON> auth status --json`
2. `claude --setting-sources user,project --settings <settings JSON> -p --output-format stream-json --verbose --no-session-persistence --permission-mode dontAsk --permission-prompts none --model <modelId> -- <prompt>`

preflightの非ゼロ終了は`config_error`にする。CLIが無い場合も固定のシェル式を
通して終了コード127を得る。設定とプロンプトは位置引数として渡し、シェル式に埋め込まない。
個人の認証情報を探したりコピーしたりしない。Q7・Q8に従い、呼び出し側が
Evaloom専用のサブスクリプション認証を専用設定環境へ用意する。

`CLAUDE_CONFIG_DIR`を専用ディレクトリに向ける。HOMEは既存Containerが空のtmpfsで
用意し、本人のHOMEをマウントしない。設定ソースは`user,project`に限定する。
`user`は専用設定環境、`project`はworktreeの設定・指示・Skillsを読むために使う。
未追跡のローカル設定を追加で採用しない。`NativeInstructionFiles`には`CLAUDE.md`を
宣言する。`AGENTS.md`は宣言せず、Context Builderが固定して組み立てた内容を
`req.Context.Instructions`経由でプロンプトへ渡す。

`Configure`が返す設定JSONをオブジェクトとして検査し、再シリアライズしてCLIへ渡す。
ファイルパス・null・配列・不正JSONは起動前に`config_error`にする。
`dontAsk`と`--permission-prompts none`により、許可されていない操作への応答を待たない。
プロンプトには固定済みExecution Context、JSONで区切ったTask、commitまで行う指示を渡す。

構造化出力の最終`result`イベントからsummaryとusageを取る。summaryが無ければ空文字、
usageが無ければnullにする。途中のassistantメッセージのusageやキャッシュ使用量は加算しない。
非ゼロ終了、不正JSON、終了イベントの欠落・重複、`is_error=true`、失敗subtypeは
`execution_failed`にする。未知のイベントは許容し、Adapterで`test_failed`は付けない。
stdout・stderrは行単位で`Logs`へ渡す。分割された書き込み、長い行、改行のない最終行も保持する。
保存にはRunnerの既存の機密情報除外を使う。Logsの消費側は1つとする。

## 既存実装との接続

呼び出し側は次を明示する。HTTP API・DB・共通契約の項目は追加していない。

- `harnessenv.Create`または`Runner.PrepareHarnessEnvironment`へ
  `ConfigVariable="CLAUDE_CONFIG_DIR"`と固定済みファイルを渡す。
  `ConfigDir()`と`Env()`を`adapter.SandboxEnv`へ渡す。
- `ContainerLaunch`へ既存の`ContainerConfig`と、Executionごとの信頼された
  `SandboxSpec`解決関数を渡す。GitDir、ネットワーク、資源制限はRunnerが指定し、
  `CLAUDE_CONFIG_DIR`を環境変数の許可リストへ含める。
- `Config.Configure`を必ず渡す。HarnessのTool PermissionsとProfileの
  `toolConfiguration`を照合し、採用CLIの設定JSONへ変換する。
- `Config.CancelGrace`へRunnerの中止猶予時間を渡す。既定値は追加していない。
- `registry.Register("claude_code", claudecode.New(config))`で登録する。
  RunnerとControl Planeは既存の登録表から名前で解決する。
- 停止後の結果を`Execution.HarnessResult`へ渡し、既存の`Runner.HarnessResultHandler`
  でHEAD・commit一覧・diff artifactを取得する。CLIの自己申告は使わない。
  Sandbox停止後に専用設定環境を`Close`する。

`ContainerLaunch`は既存の`Container.RunControlled`を使う。準備中の中止は即座に処理し、
起動後はAdapterがTERM、`CancelGrace`の待機、Sandbox全体の強制終了の順に処理する。
`Result`は全書き込みプロセスの終了を待つ。明示中止は`cancelled`、contextの期限切れと
正の`limits.timeoutSec`は`timeout`になる。0の場合、要求単独の期限を追加しない。
worktreeは保持する。Sandboxの作成・終了確認・OOM・runtime障害は既存の`sandbox_error`を使う。
CLIの終了コードが128以上でも、Sandbox障害が確認されなければ実行失敗として扱う。

## 検証

- `claude_test.go`の`TestClaudeCodeNonInteractiveCommit`が、偽CLIの実引数・環境と
  linked worktreeへのcommitを検証する。
- `container_test.go`の`TestContainerLaunchErrorClassification`がContainerLaunch経由で
  実行終了128・130・137・255、preflightの130、OOM、runtime障害を区別する。
- 同ファイルの`TestContainerLaunchCancellationSequence`が準備中・preflight中・実行中の中止、
  contextによる中止と期限切れ、TERMへの応答、猶予前の強制削除が無いことを検証する。
- [`../../runner/claude_adapter_test.go`](../../runner/claude_adapter_test.go)の
  `TestClaudeCodeAdapterCommitReachesRunnerHarnessResult`が既存RunnerによるGit情報とdiff保存を検証する。

## 仕様の穴

HarnessRequestにはHarnessのTool Permissionsが無く、そのJSONとCLI権限の対応も未定義である。
Codex Adapterと同じく`Config.Configure`を信頼された変換の境界にする。
呼び出し側は、固定済みの専用設定・Project設定を採用CLIのスキーマで検証し、
各ソースの許可規則を制限するdeny規則も返す必要がある。printモードは不正設定を
黙って無視することがあるため、認証確認だけに設定検証を任せない。
未知のツール、表現できない権限、設定不備は変換エラーとして起動を止める。

Profileの`executionConfiguration.args`は許可範囲が未定義なので空配列だけを受け付ける。
Git作者情報、猶予時間、資源制限の実運用値は選んでいない。
常駐Runnerの全工程接続と認証の準備は、既存Codex Adapterと同じく呼び出し側に残る。

## 実CLIでの手動確認

以下はモデル利用を伴うため、自動テストでは実行していない。

1. 採用CLI版を含むLinuxイメージ、既存Containerのネットワーク設定、Evaloom専用の
   サブスクリプション認証、Git作者情報を用意し、上記のインターフェースへ接続する。
   設定は既存Containerが読み取り専用でマウントするため、状態保存や認証更新に書き込みが
   必要な場合の運用も確認する。ホスト実行へ切り替えない。
2. 小さなfixtureのlinked worktreeでファイル追加とcommitを要求する。
   最終result、`git rev-list <base>..HEAD`が1件以上、Runnerのdiff artifactを確認する。
3. 長時間の実行中に`Cancel`を呼び、停止・`cancelled`・worktree保持を確認する。
   短い期限でも実行して`timeout`を確認する。
4. 仕様14の3.1に従い、個人設定のカナリアAが出ず、専用設定のカナリアBだけが出ること、
   Projectの`CLAUDE.md`と固定した`AGENTS.md`が渡ることを確認する。CLI更新時も繰り返す。

起動方法は[公式CLIリファレンス](https://code.claude.com/docs/en/cli-reference)、
result形式は[公式SDK型定義](https://code.claude.com/docs/en/agent-sdk/typescript)、
設定ソースと指示ファイルの対応は[公式設定ガイド](https://code.claude.com/docs/en/agent-sdk/claude-code-features)を参照。
