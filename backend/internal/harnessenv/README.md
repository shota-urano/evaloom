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

Codex Adapterには`Config.ResolveConfig`で元の`Environment`を渡す。
Adapterは`Environment.WorkingCopy`で同じ許可ルート内に独立したコピーを作り、
起動前に`Files`で記録済みの全パス・ハッシュと照合する。
`sandbox.WithWorkingConfiguration`で照合済みの内容を保持し、Execution専用のDocker
volumeへ転送する。`ConfigWritable=true`はそのvolumeだけを書き込み可能にし、
`CODEX_HOME`を同じコンテナ内のパスへ向ける。ホストのコピーと元設定はマウントしない。
認証確認と本実行は同じvolumeを使う。CLIが書いた状態は記録に追加せず、停止確認後に
volumeとホストのコピーを削除する。名前と保持したディレクトリの実体が違う場合、
`Files`は`config_error`を返す。`Close`は保持したハンドルで生成物の内容を消し、
専用ルート内で移動された実体も削除する。無関係な置換先を削除したり、symlinkを
たどって元設定を削除したりしない。
Claude Codeは従来どおり元設定を読み取り専用で使い、Test Sandboxには設定を渡さない。

## 仕様で未定義の部分と検証範囲

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
