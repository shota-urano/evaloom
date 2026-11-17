# Claude Code Adapter

仕様の正本は`docs/specs/08-harness-adapter.md`。既存の`adapter.Adapter`を実装し、
Runnerが渡すSandbox内でのみCLIを起動する。公開HTTP API・DB列・要求JSONの項目は追加しない。

## 起動と結果

ローカルのClaude Code 2.1.323の`--version`・`--help`・`auth status --help`で
以下の引数を確認した。コンテナ内の採用版でも実モデル試験を行う。

1. `/bin/sh -c 'exec "$@"' evaloom-claude-preflight claude --setting-sources user,project --settings <settings JSON> auth status --json`
2. `claude --setting-sources user,project --settings <settings JSON> -p --output-format stream-json --verbose --no-session-persistence --permission-mode dontAsk --permission-prompts none --model <model> -- <prompt>`

設定とプロンプトは位置引数として渡し、シェルのコードへ埋め込まない。
固定済みExecution Contextと、信頼しないタスクのJSONを区切って渡す。
要求JSONと本人の回答はRunnerがExecution Contextの後に連結する。
Adapterからcommit・push・PR作成を指示しない。作業領域の解釈はスペシャルハーネスが行う。

`Start`は監視・中止用のローカル識別子を返す。`Result.SessionID`は構造化出力の
`session_id`だけを返し、CLIがセッションを始める前の失敗では空文字を返す。
複数の異なる`session_id`は不正出力とする。
最終`result`のsummaryとusageを返すが、作業の段の概要の成果物はRunnerが
`out/summary.md`から集める。質問の検査・結果の組み立ても既存Runnerが行う。

番号が増える複数resultと欠番を許す。番号のない旧形式は1件だけを許す。
途中のresultのsummary・usageは加算せず、最終resultで置き換える。
未知のイベントは許す。成功には終了コード0、CLIのセッションID、正常な最終resultが必要。

| 状況 | errorClass |
|---|---|
| CLIなし、認証確認の失敗、不正な権限・設定コピー | `config_error` |
| 本実行の非ゼロ終了・シグナル終了、不正JSON、結果・セッションIDなし、失敗result | `work_failed` |
| Sandboxの作成・隔離・終了確認の失敗、OOM、runtime障害 | `sandbox_error` |
| 明示中止 | `cancelled` |
| contextまたは要求の期限超過 | `timeout` |

中止時はTERMを送り、Runnerが渡す`CancelGrace`を待ち、未終了ならSandbox全体を強制終了する。
`Result`は全書き込みプロセスの停止と認証の回収を待つ。作業領域は残す。
同じセッションの再開は実CLIで未検証なので`Capabilities().Resume`はfalse。
Runnerは仕様08の3.4に従って、回答付きの新しいセッションへ切り替える。

## 設定・認証・権限

既存の`harnessenv.Environment`から検証済みの作業用コピーを作り、
`CLAUDE_CONFIG_DIR`を向ける。Sandboxが空のHOMEを用意し、本人のHOMEはマウントしない。
Q142の指示ファイルは専用設定直下の`CLAUDE.md`。
`NativeInstructionFiles`にも同じ名前を宣言する。

認証は`ResolveAuthentication`が返すEvaloom専用の保管場所から取得する。
個人の認証を探したりコピーしたりしない。OSロック、`.credentials.json`の設置、
全結果での書き戻し、認証値のログからの除外には既存の
[`harnessenv`](../../harnessenv/README.md)とSandboxを使う。
通常のログはCLIの実行中から逐次渡す。認証の再取得に失敗した後のCLI出力は渡さない。

許可は`ResolveToolPermissions`が返すclaim時点の`stages.work.toolPermissions`だけから取る。
`allow`と`deny`を`permissions.allow`・`permissions.deny`へそのまま写し、
`--settings`で認証確認と本実行に渡す。項目なし・`{}`は両方とも空配列にする。
未知キー、配列でない値、文字列でない要素、空文字を起動前に拒否する。
同じ検査を既存の`registry.ToolPermissionsChecker`経由でProfile登録にも使う。
旧`toolConfiguration.allowedTools`からは許可を取らない。

`ContainerLaunch`は既存の`Container.RunControlled`を使う。
モデルAPIの許可先の候補は`api.anthropic.com:443`・`console.anthropic.com:443`・
`claude.ai:443`。実モデル試験で認証確認と本実行を確かめるまで、最小一覧の確認済みとは扱わない。
`NetworkNone`は全遮断。ホストでの直接実行へ切り替える処理は持たない。

## 検証

ルートで`make verify`を実行する。既存の偽CLI・偽runtimeの回帰試験では、
引数、作業領域の変更、中止と強制終了、3.8のエラー分類、認証の回収を検証する。
これらを実CLIでの受け入れ試験の代わりにはしない。

`live_test.go`の`TestClaudeCodeLive`は本物の`New`→`ContainerLaunch`→
`Container.RunControlled`→Claude Code→モデルAPIを通す。起動・プロセス・出力処理を
差し替えず、作業領域のファイル変更、専用CLAUDE.mdの読み込み、中止、
存在しないモデルによる`work_failed`を検証する。

実行には次の環境変数を指定する。CLI入りLinuxイメージとproxyイメージは事前に導入し、
専用認証の保管ファイルは0600にする。認証更新は本番と同じ経路で書き戻される。
テスト用のCPU・メモリ・時間はテスト内だけで指定し、運用の既定値にはしない。

- `EVALOOM_CLAUDE_LIVE_TEST=1`
- `EVALOOM_CLAUDE_TEST_IMAGE`に採用CLI版入りLinuxイメージ
- `EVALOOM_CLAUDE_TEST_PROXY_IMAGE`にLinuxのproxy用イメージ
- `EVALOOM_CLAUDE_TEST_PROXY_BINARY`に既存`sandboxproxy`のLinux実行ファイルの絶対パス
- `EVALOOM_CLAUDE_TEST_AUTH_FILE`にEvaloom専用のサブスクリプション認証ファイルの絶対パス
- `EVALOOM_CLAUDE_TEST_MODEL`に試験に使うモデル

指定後にルートの`make verify`を実行する。Docker接続用の`DOCKER_HOST`などは
runtime CLIにだけ渡し、作業のSandboxへは渡さない。
有効化後に環境・認証が不足していれば失敗にする。
無効時は実CLI試験をスキップする。

公式資料は[CLIリファレンス](https://code.claude.com/docs/en/cli-reference)と
[SDKのメッセージ型](https://code.claude.com/docs/en/agent-sdk/typescript#sdkresultmessage)を参照。
