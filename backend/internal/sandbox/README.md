# Runner Sandbox

`Sandbox.Run(ctx, SandboxSpec, command)`は仕様09の内部境界。`Process`の
`Wait / Stop / Kill`は既存の`runner.SandboxProcess`を満たす。
`Run`は作成までを同期で行い、実行は非同期で監視する。呼び出し側は必ず`Wait`する。
標準出力・標準エラーは構築時のwriterへ流す。Runner側で機密情報を除外して保存する。

## 段ごとの実行

`Prepare / Work / Check / Publish / Proposal`では、仕様09の`Mounts`を使う。
Runner側から`ContainerConfig.StagePaths`に契約の6つのゲストパスを渡し、
`StageMountRoots`に各ゲストパスの専用ホストルートを渡す。仕様のパスは例なので固定しない。
この2項目は内部の構築引数であり、Runner設定ファイルのキーではない。
許可ルートの構築方法は仕様に定義がないため、この境界で呼び出し側に委ねる。

契約6.2の表と一致しないマウントは起動前に拒否する。Workにはprofileを渡せず、
CheckとPublishのworkspaceは読み取り専用になる。Proposalはinputs・out・scratchだけを使う。
マウント元の実パスが対応するルート内にあることと、各領域が重複しないことを確かめる。
ホスト側の子マウントは共有しない。Runnerの既存実装はprofileや段のファイルをdataDir配下に
保存するため、それぞれの専用ルートの共有は許す。dataDir全体やその祖先、本人のHOME全体、
Runnerトークンを含む領域は拒否する。呼び出し側は、そのExecution・段のファイルだけを
各ルートに置き、マウントが完了するまでルートを差し替えない。

環境変数は仕様09の段別の許可リストを使う。既存の`AllowEnv`では拡張できない。
HOMEにはscratch内の新しいtmpfsを使い、TZはUTCに固定する。
WorkとProposalの専用設定は`ConfigDirRoot`内に限定し、ほかの段の領域との別名共有も拒否する。
書き込みには既存の`WithWorkingConfiguration`による検証済みvolumeを使う。
CPU・メモリ・実行時間とコンテナ全体の停止確認には、既存の実行・監視処理を使う。

段の組み立てとRunner設定からの接続は後続の`internal/stage`の範囲。
段別のネットワークは下記の境界で指定する。鍵と通話口は各担当タスクで接続し、
未対応の`Secrets`・`ChannelPath`を受け取ったら起動せずに拒否する。
macOS VMもQ147のとおり未対応として拒否する。実コンテナ・実VMでの検証は行っていない。
Proposalの作業ディレクトリは仕様で未定義なので、`--workdir`を指定しない。
必要な作業ディレクトリの指定方法は、提案を実行する呼び出し側との未確定の境界として残す。

以下の`Harness`・`Worktree`・`GitDir`の説明は、既存Adapter用に残した従来の境界を指す。

## コンテナ方式

`NewContainer(ContainerConfig)`には次の値を呼び出し側から渡す。

- Docker互換CLIの絶対パスと、事前に用意したLinuxイメージ
- 環境変数の許可リストと、ホストCLIに必要な環境変数の明示値
- マウント元ごとの専用許可ルート`WorktreeRoot / GitDirRoot / ConfigDirRoot`
- 保護する本人のHOME、Evaloomデータディレクトリ、Runnerトークンファイルの絶対パス
- 各制御コマンドの待機上限と、必要なら出力先

CPU数・メモリ総量・実行時間は各`SandboxSpec.Limits`から渡す。いずれも正の値が必須。
`RequiresMacOS`が真なら`sandbox_error`で拒否する。ProjectがmacOS専用ツールチェーンを
必要とするかは、信頼する呼び出し側が判断する。Project Registryや全体設定には列を追加しない。

ホストからのbind mountはworktree・GitDir・ConfigDirだけ。worktreeは読み書き可能、
GitDirは読み書き可能、ConfigDirは読み取り専用で渡す。Gitのworktree参照を維持するため、コンテナ側にも入力された絶対パスで
マウントする。呼び出し側は、Evaloomの作業用クローンのGit領域とExecution専用の設定を渡す。
GitDir全体を指定し、worktree内の`.git`が参照するパスもコンテナ内で解決できるようにする。

CodexとClaude Codeの`ConfigWritable=true`には、`WithWorkingConfiguration`が作るExecution専用の
内部コンテキストも必要になる。照合済みの全ファイルをtarとして保持し、停止中の
コンテナ経由で専用Docker volumeへ転送する。設定パスにはホストのbind mountを使わず、
このvolumeだけをマウントする。既存の許可ルート・機密領域・重複の検査は省略しない。
転送後は、起動前のコンテナからvolumeの全ファイルを読み戻し、記録済みのパス・ハッシュと
照合する。余分・欠落・内容の不一致・symlinkなどの非通常ファイルは`config_error`にする。
起動準備中にもホストコピーを照合し、差し替えや改変は`config_error`で拒否する。
照合後にホストの名前が変わっても、転送済みの内容とマウントするvolumeは変わらない。
Adapterは認証確認と本実行でvolumeを共有し、全プロセスの停止後に削除する。
認証だけは記録済みハッシュと比較せず、対象パスの通常ファイルとして別に検査する。
Adapterは停止確認後に`WriteBackAuthentication`を呼び、認証1つだけを回収してから
volumeを削除する。回収用コンテナはネットワークなし・読み取り専用マウントで作り、
起動しない。Test Sandboxのマウントは従来どおりである。

許可ルートは構築時に実パスへ解決する。各マウント元の実パスが対応するルート内
（ルート自身を含む）にあることを必須とし、ルート外を指すシンボリックリンクも拒否する。
HOME配下でも専用の許可ルート内にある管理領域は使える。本人のHOME全体やその祖先、
データディレクトリ・トークンと重なる領域、重複するマウントは引き続き拒否する。
呼び出し側は本人の秘密領域を含まない専用ルートを渡し、マウント完了までそのディレクトリを
差し替えない。個々のマウント内のファイルは、信頼する
worktree・設定準備処理が管理する。秘密情報のコピーやハードリンクをその中に置いてはならない。
Dockerソケット等を追加する任意のマウント指定は提供しない。

ルートFSは読み取り専用で、capabilityを全削除し、`no-new-privileges`を指定する。
専用HOMEと`/tmp`にはホストと共有しないtmpfsを用意する。イメージの暗黙のVolumeも拒否する。
ホスト環境をCLIに継承せず、コンテナ内のコマンドも`/usr/bin/env -i`から起動して
イメージのENVを除外する。コンテナに渡る値は専用HOMEと許可リストに載る明示値だけ。
HOMEの上書きは許可しない。シェルによるコマンド文字列の展開は行わない。

ネットワークはゼロ値と`NetworkNone`のどちらも`--network none`になる。
`NetworkHarness`はWorkとProposalで、`ContainerConfig.ModelAPI`に渡した
選択済みAdapterの`ModelAPIDestinations()`だけを許可する。呼び出し側の
`AllowedDestinations`はAdapterの宣言で置き換え、Profileから宛先を足せない。
既存のHarnessでは、従来どおりAdapter launcherの`AllowedDestinations`を使う。
PrepareとPublishは`NetworkDeclared`を使い、claimで固定したProfileの
`network.allow`を`AllowedDestinations`として渡す。空ならネットワークなしで動く。
宛先がある場合は`ContainerConfig.NetworkEnvironment`の
`ValidateNetworkDestination`で、登録済みの全AdapterのモデルAPI一覧と照合する。
既存の`registry.Environment`がこの境界を満たす。モデルAPIがあれば起動前に
`config_error`とし、検査元がなければ起動を拒否する。両項目は内部の構築引数であり、
Runner設定ファイルのキーではない。Checkはネットワークなしだけを受け付け、DNSも使えない。
setup とテストの Sandbox（`TestSetup`・`TestRun`・`NetworkSetup`）は software-engineer へ移したため無い
（`docs/specs/19-routing-and-stages.md` 3.11）。宛先は小文字のDNSホスト名と
ポートの完全一致（例は`model.example:443`）。ワイルドカード・IPリテラル・ローカル名を
拒否し、Harnessでは`github.com`とそのサブドメインも登録できない。

## 宛先別のネットワーク制御

許可先がある実行では、信頼するプロキシを2つの専用コンテナで起動する。

```text
Sandbox --network container:<ingress>
  -> ingressの127.0.0.1:18080（ingress自身は--network none）
  -> 実行専用Docker volume上のUnixソケット
  -> egress（--network bridge、Unixソケットだけで待ち受け）
  -> 許可したDNSホスト:ポート
```

Sandboxには外向きのインターフェースもホストへのルートもない。
Sandbox内のループバックはホストのループバックとは別であり、
`host.docker.internal`・ゲートウェイIPを指定してもホストへ出られない。
プロキシはホストのループバック上には置かない。internal bridgeだけではホストへの
経路の扱いに依存するため、この方式を選んだ。

Unixソケットのvolumeとプロキシ実行ファイルはSandboxにマウントしない。
プロキシ側にはworktree・GitDir・ConfigDir・認証情報を渡さない。
`ContainerConfig.ProxyImage / ProxyBinary`は呼び出し側の明示指定が必須で、
イメージの自動取得や既定値はない。`ProxyBinary`は従来の3つと段別の全マウント許可ルートの外にある、
信頼するLinux向け静的実行ファイルを渡す。Dockerデーモンのアーキテクチャに合わせて、
backendで次のようにビルドする（出力先も明示する）。

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/trusted/evaloom-sandbox-proxy ./cmd/evaloom-sandbox-proxy
```

Sandboxへは大文字・小文字の`HTTP_PROXY / HTTPS_PROXY`と空の`NO_PROXY`を渡す。
これらはSandbox側で設定し、呼び出し側の環境変数より優先する。
HTTP forward proxyとHTTPS CONNECTに対応する。プロキシに対応しないクライアントの
直接接続は失敗する。認証は付与せず、Q8どおりEvaloom専用の設定環境を使う。

egressは接続のたびにDNSを引き、返された全アドレスが公開IPであることを確かめる。
ループバック・プライベート・リンクローカル・特殊用途・IPv6移行用アドレスを拒否する。
RunnerのインターフェースにあるIPも起動時に列挙してegressへ渡し、公開IPを持つホストへの
接続を拒否する。この列挙が失敗した場合も段を始めない。
確認後はIPを指定して接続し、DNSの再解決によるホストへの迂回を防ぐ。
許可外へのリダイレクトも、次のリクエストで拒否する。

両プロキシのネットワーク名前空間と起動状態をinspectし、ingressからegressまでの経路で
許可外宛先が明示的に拒否されることを確かめてからSandboxを作る。
確認できなければ`sandbox_error`で終了する。起動後にプロキシが停止しても外への
迂回経路はない。各プロキシにも`Limits`と同じCPU・メモリ上限を指定するため、
実行全体には最大でコンテナ3個分の資源枠が必要。リクエスト・トンネルの上限時間は
`Limits.Time`で、実行の壁時計期限には準備時間も含む。
`Wait`はSandboxと両プロキシの停止、専用volumeの削除を確認してから返る。

CPUは`--cpus`、メモリは`--memory`で制限し、`--memory-swap`も同じ値にしてswapへの
超過を許可しない。イメージの自動取得は`--pull never`で禁止する。各CLI引数の仕様は
[Docker create](https://docs.docker.com/reference/cli/docker/container/create/)を参照。

## 終了と分類

実行時間はイメージ確認・作成を含む壁時計時間。期限またはcontextの中止では、名前を指定して
コンテナ全体を強制削除する。CLIだけを終了して完了とは扱わない。
通常終了でもruntimeのStateで終了コード・OOMを確認した後にコンテナを削除する。
attach用CLIが先に終了した場合も、Stateの確認には実行期限を適用する。
確認が待機していても、期限へ達したら強制削除を開始する。
worktreeは削除しない。`Stop`はコンテナ全体にTERM、`Kill`は強制削除を要求する。

`Error.Status()`と`Error.ErrorClass()`は既存の状態・エラー分類を返す。
時間超過は`timed_out / timeout`、中止は`cancelled / cancelled`、
作成・隔離確認・OOM・シグナル終了等は`failed / sandbox_error`になる。
時間超過と中止は`errors.Is`でも検出でき、既存Runnerの監視処理で分類できる。
通常のコマンド失敗は`ExitError`と終了コードを返し、Adapter側が分類する。
runtimeの診断文はエラーに含めない。

制御コマンドが失敗しても、コンテナの不存在または停止を確認するまでは`Wait`を返さない。
デーモンに接続できず停止も確認できない場合は、制御コマンドの待機上限を間隔として
停止確認を続ける。Runnerはこの間、実行枠を解放しない。

## 検証と未確定事項

通常テストは偽CLIを使い、実コンテナには依存しない。受け入れ基準は、実際にCLIへ渡した
マウント・環境変数・資源制限の引数と、別プロセス群で動く偽workloadの停止で検証する。
プロキシの通常テストでは、実HTTPパーサとUnixソケットを通し、末端のDNS・接続だけを
ローカルfixtureに置き換える。許可先へのHTTP・CONNECT、許可外の拒否、DNS rebindingを
検証する。偽runtimeでは5種類の段と従来のHarnessのネットワーク構成、名前空間共有、ファイルの分離、
起動確認の失敗時の拒否と後始末を検証する。Dockerデーモンは不要。
PrepareとPublishのモデルAPI拒否と、WorkとProposalの宛先追加の防止も起動引数で検証する。

実Dockerによる試験は明示的に有効化したときだけ実行する。

```sh
cd backend
EVALOOM_SANDBOX_DOCKER_TEST=1 EVALOOM_SANDBOX_TEST_IMAGE=alpine:3.20 \
  go test ./internal/sandbox -run '^TestDockerNetworkIsolation$' -count=1 -v
```

イメージは事前取得済みで`/bin/busybox`と`/usr/bin/env`が必要。
試験内でプロキシをLinux用にビルドし、ホストの127.0.0.1にカナリアlistenerを置く。
3種別からループバック・Dockerホスト名・gateway・DesktopのホストIPへの直接接続と、
プロキシ経由の接続を試し、listenerのアクセス数も確認する。
GitHubと1.1.1.1への接続も拒否されることを確かめる。
公開HTTPS宛先への正の確認も実行する場合は、さらに
`EVALOOM_SANDBOX_TEST_ALLOWED_DESTINATION=example.com:443`を明示する。
有効化していない場合だけ`t.Skip`し、有効化後の前提不足や試験失敗はFAILにする。

作業用の設定コピーは、モデルAPIと実CLIを使わず実Dockerでも検証できる。

```sh
cd backend
EVALOOM_SECURITY_TEST_IMAGE=evaloom-security-boundary:local \
  go test -tags security_docker ./internal/sandbox -run '^TestDockerWorkingConfiguration$' -count=1 -v
```

指定イメージは事前に用意し、`/usr/bin/env`、`/bin/sh`、`cat`、`touch`が必要になる。
この試験は読み取り専用のルートFS、専用volumeへの設定転送、CLI状態の引き継ぎ、
元設定の不可視・不変、終了後のvolume削除を確認する。

仕様にruntime・イメージ名・環境変数許可リストの具体値、CPU・メモリの値、
制御コマンドの待機上限がないため、既定値を設けず内部設定として受け取る。
マウント元ごとの許可ルートの設定方法も仕様にないため、同じ内部設定で明示的に受け取る。
イメージには`/usr/bin/env`が必要で、Linux用かつVolume宣言がないことを検査する。
専用設定の作成・ハッシュ記録、実Adapterの接続は各担当タスクの範囲。
公開HTTP API・テーブル・列と、確定済みの仕様書は変更していない。
モデルAPIの接続先は既存Adapter内の定数を使い、公開APIや保存列を足さない。
選択済みAdapterと登録済み全Adapterの一覧をSandboxへ渡す方法は、上記の内部インターフェースで
境界を切っている。claimで固定したProfileの宛先と合わせて渡す接続は段の組み立て側に残る。
