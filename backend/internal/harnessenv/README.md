# 専用設定環境

正本は`docs/specs/08-harness-adapter.md`の3.4節、`07-context.md`の3.3節、
`09-sandbox.md`の3.2・5節、`02-data-model.md`のcontext_revisions定義である。

`harnessenv.Create`には、Executionに固定したProfileのcommit SHAと、
`harnesscontext.Read`で取得したファイルを渡す。専用ルート内に実行ごとの
ディレクトリを作り、ファイルを0600、ディレクトリを0700で配置する。
本人のHOME、環境変数、ハーネスリポジトリの作業ツリーは読み込まない。
パス・重複・SHA・内容のハッシュを検証し、失敗したら途中の設定を削除する。

`Environment.Files`は配置した全ファイルを列挙し、実際の内容からSHA-256を計算する。
未選択ファイル、欠落、内容の変化、symlinkなどの通常ファイル以外を拒否する。
設定不備は既存の`harnesscontext.ConfigError`で返す。
中止は`errors.Is(err, context.Canceled)`で確認できる。

`Runner.PrepareHarnessEnvironment`は作成と照合を行い、Projectの固定済みファイルと
現在のworktree HEADを添えて、既存の`POST /api/executions/{id}/context`へ送る。
Control Planeは既存のトランザクションで、各ファイルの相対パス・内容・SHA-256・
Profileのcommit SHAを`layer=harness, kind=harness_config`として保存する。
本人用API・テーブル・列は追加しない。Runnerは管理DBを直接開かない。
保存の応答を確認するまで、呼び出し側に設定環境を返さない。
HTTP・保存・応答の検証に失敗した場合も、作成したディレクトリを削除する。
応答が失われた場合は同じ入力で再実行でき、既存APIの再送処理でrevision IDを保つ。

## Sandboxとの接続

呼び出し側は以下を明示する。

- `Config.Root`は`ContainerConfig.ConfigDirRoot`で許可した専用ルートにする。
- `Config.ConfigVariable`は採用したCLI版に合わせて`CODEX_HOME`または
  `CLAUDE_CONFIG_DIR`を渡し、同じ名前を`ContainerConfig.AllowEnv`に含める。
- Harnessの`SandboxSpec.ConfigDir`に`Environment.ConfigDir()`を、
  `SandboxSpec.Env`に`Environment.Env()`を渡す。
- 返されたExecution ContextをHarnessRequestに使う。
- Sandboxが停止した後、失敗・中止の場合も`Environment.Close()`を呼ぶ。

HOMEは既存のContainerが実行ごとに作る空の`/evaloom-home` tmpfsを使う。
ホストからのマウントはworktree・Git領域・専用設定ディレクトリに限り、
専用設定は既存の読み取り専用マウントを使う。環境変数は`env -i`で初期化する。
ホストのHOMEや秘密情報を環境変数だけで隠したと扱わない。

両Adapterには`Config.ResolveConfig`で元の`Environment`を渡す。
Adapterは`Environment.WorkingCopy`で同じ許可ルート内に独立したコピーを作り、
起動前に`Files`で記録済みの全パス・ハッシュと照合する。
`sandbox.WithWorkingConfiguration`で照合済みの内容を保持し、Execution専用のDocker
volumeへ転送する。`ConfigWritable=true`はそのvolumeだけを書き込み可能にし、
`CODEX_HOME`または`CLAUDE_CONFIG_DIR`を同じコンテナ内のパスへ向ける。ホストのコピーと元設定はマウントしない。
認証確認と本実行は同じvolumeを使う。CLIが書いた状態は記録に追加せず、停止確認後に
認証を書き戻してからvolumeとホストのコピーを削除する。名前と保持したディレクトリの実体が違う場合、
`Files`は`config_error`を返す。`Close`は保持したハンドルで生成物の内容を消し、
専用ルート内で移動された実体も削除する。無関係な置換先を削除したり、symlinkを
たどって元設定を削除したりしない。
Test Sandboxには設定を渡さない。

## 認証ファイル

両Adapterは、`Config.ResolveAuthentication`でHarnessごとの保管パスを受け取る。
呼び出し側は、同じHarnessの全Executionに同じ絶対パスを返す。保管場所はRunnerの
データディレクトリ内のGit管理外に用意し、通常ファイル・0600・64 KiB以下の
JSONオブジェクトにする。設定ファイルのキー名は追加していない。

`AcquireAuthentication`は、保管ファイルの隣の`<保管ファイル名>.lock`にOSの
排他ファイルロックを取得してから認証を読む。ロックファイルは0600で作り、
名前を変更・削除しない。別プロセスのRunnerにも排他制御が働く。
待機中はcontextの中止・期限切れに対応する。認証ファイル自体をロックしないため、
書き戻しのrename後も後続Executionとの排他制御を保つ。

`Authentication.Install`は作業用コピーだけに`auth.json`または`.credentials.json`を
配置する。`Create`はProfileからの両認証パスを拒否し、元設定へ置かせない。
`Files`は認証の対象パスに通常ファイルがあることを確認するが、認証の内容・パス・
ハッシュを返さない。認証の転送には別の`AuthenticationFile`を使い、記録APIへ渡さない。
認証以外の全ファイルは従来どおり記録したハッシュと照合する。

Sandboxの全書き込みプロセスが止まった後、Adapterは`WriteBackAuthentication`を呼ぶ。
Containerの場合は、同じvolumeを読み取り専用でマウントする停止中の回収用コンテナを
作る。このコンテナは起動せず、認証ファイル1つだけをtarで読み戻す。
受信量を制限し、通常ファイル・対象パス・64 KiB以下・JSONオブジェクトを検査する。
symlink、hardlink、余分なエントリ、不正な内容は書き戻さない。
ホストのコピーを直接使うテスト用Launchでは、保持したディレクトリから同じ検査で読む。

内容が変わった場合だけ、保管場所と同じディレクトリの一時ファイルへ0600で書き、
`fsync`・closeの成功後にrenameで置き換える。途中の失敗では元ファイルを残す。
成功・失敗・中止・タイムアウトの全結果でこの処理を行い、書き戻し後にコピーを削除して
ロックを解放する。中止済みcontextでも、回収には既存の制御コマンドの期限を使う。
書き戻しの失敗ではExecution結果を変更せず、内容・パスを含まない理由だけをログへ渡す。
CLIのstdout・stderrは認証回収までメモリ内に保持する。回収後、更新前と回収後の
認証JSON全体・入れ子の文字列値・JSONでエスケープした値・複数行の各行を、
Runnerと共通の`logredact`で除外してから`Logs`へ渡す。summaryにも同じ除外を適用する。
保管場所への書き込みが失敗しても、回収できた値は除外する。回収できない・検査に
通らない場合はCLI出力を`[REDACTED]`に置き換え、内容を含まない書き戻し失敗の理由だけを
別途追加する。生の出力を一時ファイルへ保存しない。Runnerは従来のヘッダー・秘密鍵・
既知の秘密値の除外も行い、Adapterによる除外をartifactの`redacted`へ引き継ぐ。
このため、CLIログは実行中には届かず、停止と認証回収の後に届く。

## 仕様で未定義の部分と検証範囲

仕様には認証更新後の値を除外するために許容するログ転送の遅延・メモリ保持量の
規定が無い。更新前後の秘密値を保存・配信前に除外するため、この実装は停止後に
転送する。新しい上限値・タイムアウト・設定キーは追加していない。

仕様にはリポジトリのパスからCLI内の配置先への変換規則が無い。
この実装は相対パスをそのまま保ち、`promptVersion`やSkillsの版から配置先を推測しない。
Profile定義の選択には既存の`contextFiles`を使い、新しい設定項目は追加しない。
Gitの作者情報やCLI固有の設定ファイルも生成しない。

実際のCLIで設定先の環境変数が有効か、選択したパスを読み込むかの確認は、
各Adapterと仕様14の実CLIカナリア試験の範囲である。このタスクでは実行していない。
Runner起動コマンドでの工程全体の接続も既存どおり別の作業とし、
この処理は呼び出せるRunner内部メソッドとして提供する。

受け入れテストは`TestDedicatedHarnessConfigurationSandboxBoundary`で実際の
Container呼び出し引数・マウント元・有効な環境変数を検証する。隔離ランタイムは
テスト用プロセスに置き換えるため、実コンテナでのアクセス拒否試験とは区別する。
`TestHarnessEnvironmentPersistsEveryPlacedFile`は固定commitの読み取りから
専用ディレクトリ、認証付きHTTP、SQLite保存まで通し、全ファイルと記録を照合する。

認証の検証は`authentication_test.go`と両Adapterの同名ファイルが担当する。
`TestAuthenticationOSLockPrecedesReadAndIsCancellable`は別OSプロセスでロックを確認し、
`TestAuthenticationAtomicWriteFailures`は部分書き込み・sync・renameの失敗を注入する。
両Adapterの`TestContainerAuthenticationRecoveredAfterEveryOutcome`は偽runtimeを通して
volumeの更新から停止・認証回収・書き戻しまでを検証する。
実Dockerの認証回収は`security_docker`の`TestDockerAuthenticationWriteback`で検証できる。
通常の`make verify`では実Docker・実CLI・モデルAPIの試験を実行しない。

`runner/authentication_logs_test.go`の
`TestAuthenticationExcludedBeforeRunnerFilesArtifactsAndSSE`は、両Adapterの偽CLIから
更新前後の認証JSONとトークン単体を実際に出力する。成功・失敗・中止・タイムアウト・
preflight失敗・不正JSON・サイズ超過・symlinkを通し、Runnerのローカルファイル、
保存artifact、HTTPのSSEフレームに認証が残らないことを確認する。認証回収前に
ファイルやイベントへ出力が届かないことも確認する。
