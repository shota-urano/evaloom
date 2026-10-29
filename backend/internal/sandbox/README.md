# Runner Sandbox

`Sandbox.Run(ctx, SandboxSpec, command)`は仕様09の内部境界。`Process`の
`Wait / Stop / Kill`は既存の`runner.SandboxProcess`を満たす。
`Run`は作成までを同期で行い、実行は非同期で監視する。呼び出し側は必ず`Wait`する。
標準出力・標準エラーは構築時のwriterへ流す。Runner側で機密情報を除外して保存する。

## コンテナ方式

`NewContainer(ContainerConfig)`には次の値を呼び出し側から渡す。

- Docker互換CLIの絶対パスと、事前に用意したLinuxイメージ
- 環境変数の許可リストと、ホストCLIに必要な環境変数の明示値
- 保護する本人のHOME、Evaloomデータディレクトリ、Runnerトークンファイルの絶対パス
- 各制御コマンドの待機上限と、必要なら出力先

CPU数・メモリ総量・実行時間は各`SandboxSpec.Limits`から渡す。いずれも正の値が必須。
`RequiresMacOS`が真なら`sandbox_error`で拒否する。ProjectがmacOS専用ツールチェーンを
必要とするかは、信頼する呼び出し側が判断する。Project Registryや全体設定には列を追加しない。

ホストからのbind mountはworktree・GitDir・ConfigDirだけ。worktreeは読み書き可能、
GitDirはHarnessだけ読み書き可能でテストでは読み取り専用、ConfigDirはHarnessだけに
読み取り専用で渡す。Gitのworktree参照を維持するため、コンテナ側にも入力された絶対パスで
マウントする。呼び出し側は、Evaloomの作業用クローンのGit領域とExecution専用の設定を渡す。
GitDir全体を指定し、worktree内の`.git`が参照するパスもコンテナ内で解決できるようにする。

マウント元の実パスを解決し、本人のHOME全体やその祖先、データディレクトリや
トークンを含む領域、重複するマウントを拒否する。個々のマウント内のファイルは、信頼する
worktree・設定準備処理が管理する。秘密情報のコピーやハードリンクをその中に置いてはならない。
Dockerソケット等を追加する任意のマウント指定は提供しない。

ルートFSは読み取り専用で、capabilityを全削除し、`no-new-privileges`を指定する。
専用HOMEと`/tmp`にはホストと共有しないtmpfsを用意する。イメージの暗黙のVolumeも拒否する。
ホスト環境をCLIに継承せず、コンテナ内のコマンドも`/usr/bin/env -i`から起動して
イメージのENVを除外する。コンテナに渡る値は専用HOMEと許可リストに載る明示値だけ。
HOMEの上書きは許可しない。シェルによるコマンド文字列の展開は行わない。

ネットワークはゼロ値と`NetworkNone`のどちらも`--network none`になる。
それ以外は実行前に拒否する。宛先別の許可はevaloom-6n2.3で実装する。

CPUは`--cpus`、メモリは`--memory`で制限し、`--memory-swap`も同じ値にしてswapへの
超過を許可しない。イメージの自動取得は`--pull never`で禁止する。各CLI引数の仕様は
[Docker create](https://docs.docker.com/reference/cli/docker/container/create/)を参照。

## 終了と分類

実行時間はイメージ確認・作成を含む壁時計時間。期限またはcontextの中止では、名前を指定して
コンテナ全体を強制削除する。CLIだけを終了して完了とは扱わない。
通常終了でもruntimeのStateで終了コード・OOMを確認した後にコンテナを削除する。
worktreeは削除しない。`Stop`はコンテナ全体にTERM、`Kill`は強制削除を要求する。

`Error.Status()`と`Error.ErrorClass()`は既存の状態・エラー分類を返す。
時間超過は`timed_out / timeout`、中止は`cancelled / cancelled`、
作成・隔離確認・OOM・シグナル終了等は`failed / sandbox_error`になる。
時間超過と中止は`errors.Is`でも検出でき、既存Runnerの監視処理で分類できる。
通常のコマンド失敗は`ExitError`と終了コードを返し、setup・Adapter・Gate側が分類する。
runtimeの診断文はエラーに含めない。

制御コマンドが失敗しても、コンテナの不存在または停止を確認するまでは`Wait`を返さない。
デーモンに接続できず停止も確認できない場合は、制御コマンドの待機上限を間隔として
停止確認を続ける。Runnerはこの間、実行枠を解放しない。

## 検証と未確定事項

通常テストは偽CLIを使い、実コンテナには依存しない。受け入れ基準は、実際にCLIへ渡した
マウント・環境変数・資源制限の引数と、別プロセス群で動く偽workloadの停止で検証する。
実コンテナによる隔離突破試験はこのタスクでは追加していない。

仕様にruntime・イメージ名・環境変数許可リストの具体値、CPU・メモリの値、
制御コマンドの待機上限がないため、既定値を設けず内部設定として受け取る。
イメージには`/usr/bin/env`が必要で、Linux用かつVolume宣言がないことを検査する。
専用設定の作成・ハッシュ記録、実Adapterやsetup・Gate工程の接続は各担当タスクの範囲。
公開HTTP API・テーブル・列と、確定済みの仕様書は変更していない。
