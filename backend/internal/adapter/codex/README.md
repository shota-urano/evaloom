# Codex Adapter

仕様の正本は`docs/specs/08-harness-adapter.md`。このパッケージは共通の
`adapter.Adapter`を実装し、ホスト上でCLIを直接実行する処理を持たない。
テストでは偽の`codex`実行ファイルを使い、モデルAPIには接続しない。

## 起動と結果

採用対象はローカルでヘルプを確認した`codex-cli 0.163.0`。
次の順に、同じ作業用の設定コピーを使うHarness Sandboxで実行する。

1. `/bin/sh -c 'exec "$@"' evaloom-codex-preflight codex [設定上書き] login status`
2. `codex [設定上書き] exec --json --color never --ephemeral --sandbox danger-full-access --model <modelId> --cd <worktree> -- <prompt>`

最初の処理で、CLIの存在、Codexによる設定ファイルの読み込み、保存済み認証を
確認する。非ゼロ終了は`config_error`にする。固定のシェル式にはCLI引数を
位置引数として渡す。プロンプトや設定をシェルのソースに埋め込まない。
認証ファイルをホストのHOMEから探したり、コピーしたりしない。
Q7・Q8の決定に従い、呼び出し側がEvaloom専用のログイン情報を設定環境に用意する。

プロンプトには固定済みのExecution Context、JSONとして区切ったTask、
commitまで行う指示を渡す。Model IDは要求から取得する。
Codex自身の制限には`danger-full-access`を指定し、実際の制限は既存の
Harness Sandboxで適用する。Git共有領域へのcommitもこのSandboxの許可範囲になる。
この起動方法をホスト直接実行のために使わない。

JSONLの`item.completed`のうち最後の`agent_message.text`をsummaryに使う。
最終メッセージが無ければ空文字にする。`turn.completed.usage`から入力・出力トークンを
取得し、欠けていればnullを保つ。キャッシュのトークン数を重ねて加算しない。
未知のイベントは許容する。不正JSON、終了イベントの欠落、失敗イベント、
非ゼロ終了は`execution_failed`にする。Adapterで`test_failed`は付けない。

stdout・stderrを行単位で`Logs`へ渡す。長い行、分割された書き込み、改行のない
最終行も保持する。Logsは1つの消費側で読む。出力は生の値なので、保存には
Runnerの既存ログ収集・機密情報除外を使う。
`Result`はSandboxの全書き込みプロセスが停止するまで待つ。
その後、作業用の設定コピーを削除してから結果を返す。
HEAD・commit一覧・diff・PR URLは生成しない。

`Cancel`または呼び出し元contextの中止で停止を要求し、指定された猶予時間を
過ぎたらSandbox全体を強制終了する。終了を確認してから`cancelled`を返す。
contextの期限と正の`limits.timeoutSec`は`timeout`にする。0は要求単独の期限を
追加しない。Runner・Sandbox側の制限は引き続き適用する。
worktreeの削除は行わない。Sandboxの作成・隔離・終了確認の失敗は既存の
`sandbox_error`で返し、ホスト実行へ切り替えない。
終了コードが128以上でも、OOMやruntime障害が確認されなければCLIの異常終了として扱う。
`login status`の非ゼロ終了は`config_error`、`exec`の非ゼロ終了は`execution_failed`になる。

`ContainerLaunch`は内部の`Container.RunControlled`で準備中の中止に対応する。
起動後の中止はAdapterがTERM、`CancelGrace`の待機、Sandbox全体の強制終了の順に行う。
Sandbox独自の実行時間上限は、準備の開始から引き続き適用する。
既存の`Container.Run`を使う他の処理では、contextによる即時停止を維持する。

## 既存実装との接続

呼び出し側は次の値を明示する。新しいHTTP APIや永続化項目は追加しない。

- `harnessenv.Create`または`Runner.PrepareHarnessEnvironment`で固定済みファイルを配置し、
  `ConfigVariable="CODEX_HOME"`にする。`Environment.ConfigDir()`と`Environment.Env()`を
  `adapter.SandboxEnv`へ渡す。専用設定の内容・パスの選択は既存処理の責任とする。
- `Config.ResolveConfig`で、渡された元設定ディレクトリに対応する`Environment`を返す。
  Adapterはこの環境が保持する記録済みのパス・ハッシュを使い、同じ許可ルート内に
  独立したコピーを作る。元設定はSandboxにマウントしない。
  認証確認の直前に全ファイルを照合し、改変・余分・欠落があればCLIを起動せず
  `config_error`にする。解決関数が無い場合も起動しない。
  コピーの`CODEX_HOME`だけを差し替え、`ContainerLaunch`が`ConfigWritable=true`にする。
  `WithWorkingConfiguration`で照合済みの内容を保持し、Containerはその内容だけを
  標準入力のtarとしてExecution専用のDocker volumeへ転送する。ホストの設定パスを
  書き込み可能なbind mountには使わない。認証確認と本実行は同じvolumeを使う。
  CLIの初回起動前にvolumeから全ファイルを読み戻し、記録済みのパス・ハッシュと照合する。
  元設定はマウントしない。既存のマウント安全検査は設定コピーのパスにも適用する。
  設定パスの実体・全ファイルは起動準備中にも再確認し、ディレクトリ差し替えや
  symlinkへの変更は`config_error`にする。最終確認後にホストのパスが変わっても、
  Dockerが使うのは転送済みの内容であり、差し替え先を再解決してマウントしない。
  認証確認後のCLIの書き込みは本実行へ引き継ぐが、context_revisionsへ追加しない。
  成功・失敗・中止・起動失敗のいずれでも、Sandbox停止後にvolumeとホストのコピーを
  削除する。移動された生成ディレクトリは保持したハンドルで内容を消し、専用ルート内で
  同じ実体を探して削除する。元の名前に置かれた無関係なファイルは削除しない。
- `ContainerLaunch`に既存の`ContainerConfig`と、Executionに対応する`SandboxSpec`の
  解決関数を渡す。GitDir、ネットワークの許可先、資源制限は信頼されたRunnerが与える。
  `CODEX_HOME`を環境変数の許可リストに含める。HOMEの隔離は既存Containerが行う。
- `Config.Configure`でHarnessのTool PermissionsとProfileの`toolConfiguration`を照合し、
  採用CLIで表現可能な設定だけをTOMLの`key=value`形式で返す。
  不明なツール・表現できない権限はエラーにする。この関数を省略した場合も起動しない。
- `Config.CancelGrace`にはRunnerの中止猶予時間を渡す。既定値は追加しない。
- `registry.Register("codex", codex.New(config))`で登録する。Runnerは登録名で解決する。
- ログを消費し、停止後の結果を`Execution.HarnessResult`へ渡す。
  既存の`Runner.HarnessResultHandler`がworktreeからGit情報を取得し、diff artifactを保存する。
  最後に専用設定環境を`Close`する。

`TestCodexAdapterCommitReachesRunnerHarnessResult`は、実際のlinked worktreeに対する
偽CLIのcommitから、既存Runnerのdiff保存までを検証する。
`TestContainerLaunchUsesPreparedHarnessSandbox`はContainerの実際の起動引数と
マウント・専用HOME・CODEX_HOMEの受け渡しを偽runtimeで検証する。
`TestContainerLaunchErrorClassification`はpreflightが0、execが130で終了する場合と、
preflightの異常終了、OOM、runtime障害を検証する。
`TestContainerLaunchCancellationSequence`は準備中・preflight実行中・exec実行中の中止、
呼び出し元contextの中止と期限切れ、TERMへの応答、猶予後の強制終了を検証する。
`TestContainerRejectsConfigurationReplacementAfterAdapterVerification`はイメージ確認・
コンテナ作成・設定転送の各段階に差し替え、symlink、内容改変を挿入し、CLI未起動の
`config_error`と移動されたコピー・volumeの削除を確認する。
`TestContainerWorkingVolumeLifetime`は成功・CLI失敗・volume作成失敗・転送失敗・
コンテナ作成失敗で後始末を確認する。Dockerへの設定転送には、停止中のコンテナへの
[標準入力からのtar転送](https://docs.docker.com/reference/cli/docker/container/cp/)を使う。

## 仕様の穴と接続範囲

共通のHarnessRequestにはHarnessのTool Permissionsが無く、そのJSONの内容と
Codexのツール権限との対応も仕様には無い。新しい要求項目や独自のツール名は追加せず、
`Config.Configure`を信頼された設定変換の境界にする。変換処理の提供が必要である。
Profileの`executionConfiguration.args`は、許可する追加引数が未定義なので、
今回は空配列だけを受け付ける。設定を上書きするフラグを暗黙に許可しない。

Git作者名・メールアドレスの値は選んでいない。Runnerが専用のGit設定で与える。
テストだけで`Fixture <fixture@example.invalid>`を使う。
常駐Runnerの起動コマンドは既存どおり全工程の接続待ちであり、この変更で工程全体や
認証情報の準備を実装しない。登録と内部インターフェースから利用できる形で提供する。

## 実CLIでの手動確認

モデル利用を伴う以下の確認は自動テストでは実行していない。

1. 採用CLI版を含むLinuxイメージ、既存Containerのネットワーク設定、Evaloom専用の
   サブスクリプション認証を用意する。本人が普段使うHOMEをマウントしない。
2. 専用設定環境に固定した`config.toml`・`auth.json`・Git作者情報を用意し、上記の
   インターフェースに接続する。元設定は保持し、書き込み可能な作業用コピーを
   `CODEX_HOME`に使う。実CLIの状態保存と認証更新はこのコピー内で確認する。
3. 小さなfixtureプロジェクトのlinked worktreeで、ファイルの追加とcommitを要求する。
   JSONLの完了、worktreeの`git rev-list <base>..HEAD`が1件以上、Runnerのdiffを確認する。
4. 長時間の処理中に`Cancel`を呼び、Sandbox停止・`cancelled`・worktree保持を確認する。
   短い期限でも実行して`timeout`を確認する。
5. 仕様14の3.1に従って個人設定のカナリアAと専用設定のカナリアBを使い、Aが出力されず
   Bだけが出力されること、実際の`AGENTS.md`の読み込みを確認する。
   CLIを更新する際も同じ確認を行う。

起動方法とJSONL形式の参照先は
[OpenAI公式の非対話実行ドキュメント](https://learn.chatgpt.com/docs/non-interactive-mode)と
[CLIリファレンス](https://learn.chatgpt.com/docs/cli/reference)。
