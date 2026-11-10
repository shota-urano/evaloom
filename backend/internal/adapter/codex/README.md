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
Q7・Q8の決定に従い、`Config.ResolveAuthentication`でEvaloom専用の認証の保管パスを
解決する。同じHarnessには常に同じ絶対パスを返す。Adapterが読み取り前にOSロックを
取り、作業用コピーの`auth.json`へ配置する。認証はProfileとcontext_revisionsに含めない。
詳細は[`harnessenv/README.md`](../../harnessenv/README.md)の認証ファイルの節を参照する。

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
最終行も保持する。Logsは1つの消費側で読む。出力は認証回収までメモリ内に保持し、
更新前後の認証値を除外してから渡す。認証を回収できなければCLI出力を伏せる。
このためCLIログは停止後に届く。保存にはRunnerの既存ログ収集・機密情報除外も使う。
`Result`はSandboxの全書き込みプロセスが停止するまで待つ。
その後、成功・失敗・中止・タイムアウトに関係なく、volumeの認証ファイル1つを検査して
保管場所へ原子的に書き戻す。書き戻しを終え、コピーの削除とOSロックの解放後に結果を返す。
書き戻し失敗ではExecution結果を変えず、認証内容を含まない理由だけをログに残す。
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

- `harnessenv.Create`で固定済みファイルを配置し、
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
  解決関数を渡す。GitDir、ネットワークポリシー、資源制限は信頼されたRunnerが与える。
  `NetworkHarness`の許可先はAdapter内の定数で置き換える。Codexは
  `chatgpt.com:443`、`auth.openai.com:443`、`api.openai.com:443`だけを渡す。
  `NetworkNone`は全遮断のままにし、setup用ポリシーは拒否する。
  `CODEX_HOME`を環境変数の許可リストに含める。HOMEの隔離は既存Containerが行う。
- `Config.ResolveToolPermissions`でclaim時点のHarnessの`toolPermissions`をJSONオブジェクトとして返す。
  変換はAdapterが行う。許可するキーは`deny`だけで、`{}`は追加の禁止なしを表す。
  Profileの`toolConfiguration.allowedTools`と`deny`の両方が空なら起動する。
  どちらかに要素があれば、Codex 0.163.0では表現できないため`config_error`にする。
  未知キー、配列でない値、文字列でない要素、空文字、null、解決関数の欠落も起動前に拒否する。
- `Config.CancelGrace`にはRunnerの中止猶予時間を渡す。既定値は追加しない。
- `registry.Register("codex", codex.New(config))`で登録する。Runnerは登録名で解決する。
- ログを消費し、停止後の結果を受け取る。最後に専用設定環境を`Close`する。
  Runner の旧工程（Git 情報と diff の取得）は software-engineer へ移した（`docs/specs/19-routing-and-stages.md` 3.11）。

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

共通のHarnessRequestにはHarnessのTool Permissionsが無いため、要求項目は増やさず、
`Config.ResolveToolPermissions`をclaimの固定値を受け取る内部境界として使う。
起動コマンドとclaimの接続は後続タスクevaloom-1b9が行う。権限の形と変換規則は
仕様08の3.8と仕様00のQ72に従い、Adapter内で検証する。
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
2. Profileの`config.toml`とGit作者情報、Git管理外の認証の保管ファイルを用意し、
   `ResolveConfig`と`ResolveAuthentication`に接続する。元設定は保持し、書き込み可能な作業用コピーを
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

`permissions_test.go`の`TestCodexToolPermissionsLaunchArguments`が空の権限での起動を、
`TestCodexToolPermissionsRejectInvalidBeforeCLI`が非空・不正な権限でのCLI未起動と
`config_error`を検証する。`TestContainerLaunchOnlyAdapterDestinations`はSandboxに渡す
宛先の全件一致、GitHubの除外、呼び出し元の一覧の置き換え、実行間の独立性を検証する。
