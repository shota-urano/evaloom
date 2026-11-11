# 仕様14のテスト用の道具

`adapter/probe` は、ダミーAdapterの決定的なセッションをスクリプト実行に拡張したテスト専用Adapterである。本番バイナリには登録しない。AIは呼ばない。

`NewFixture(ctx, root)` は、空の `root/fixture` に小さなPythonプロジェクト、専用HOME、作業用リポジトリ、worktree、ローカルbareリモートを作る。既存のディレクトリは再利用しない。呼び出し側が一時ディレクトリを管理する。Gitの作者情報もテスト専用である。

fixtureのコマンドは `python3 setup.py` と `python3 test.py`。後者は実際のunittestを2件実行し、`junit.xml` にID、件数、失敗、エラー、skipを出力する。テストが失敗した場合は終了コードも非0になる。`harness.py` はアクセスを試した後、`app.py` だけを変更してcommitする。

## プローブの入力と出力

`setup-plan.json`、`test-plan.json`、`harness-plan.json` を、それぞれの実行前にテスト側で設定する。既定のplanは `app.py` の存在確認だけを行う。setupとテストの両方にアクセス試行を仕込める。各planは `id` と `kind` を持つオブジェクトの非空配列である。

| kind | 追加の入力 | 試す操作 |
|---|---|---|
| `read` | `path` | ファイル全体の読み取り |
| `write` | `path`, `value` | ファイルへの追記。破棄できるテスト用ファイルだけを指定する |
| `exists` | `path` | ファイル・ソケット等の存在確認 |
| `http` | `url`, `timeoutSec` | プロキシ設定を使わないHTTP接続。4xx/5xxでも接続成功として記録する |
| `tcp` | `host`, `port`, `timeoutSec` | DNS解決を含む直接TCP接続 |
| `proxy_connect` | `host`, `port`, `timeoutSec` | SandboxのプロキシへのCONNECT。完全なHTTP応答の200は接続成功、403は拒否。プロキシへの接続拒否・タイムアウト・通信障害・不正応答は実行不能。単体テストでは `proxyPort` でプロキシのポートを渡せる |
| `env` | `allowlist`, `secrets` | 環境変数名、許可外の名前、既知の秘密の値が含まれるかの確認 |
| `git_push` | `remote`, `refspec`, `timeoutSec` | 明示したリモート・refへのpush |
| `command` | `argv`, `timeoutSec` | キーチェーンやSSHエージェント等にアクセスするコマンドの実行 |

`python3 probe.py <plan> <result>` でも同じ処理を呼べる。入力例は次のとおり。

```json
[
  {"id":"control-api", "kind":"http", "url":"http://127.0.0.1:12345/api/tasks", "timeoutSec":2},
  {"id":"runner-token", "kind":"read", "path":"/test-owned/runner-token"}
]
```

出力はworktree内の `setup-results.json`、`test-results.json`、`harness-results.json`。`observations` 配列に `id`、`kind`、`succeeded`、`detail` を書く。秘密情報の本文やコマンドの出力本文は書かない。例の宛先、ファイル、タイムアウトはテスト側が用意する値であり、製品の設定値ではない。

アクセスが拒否された場合もスクリプトは正常終了する。`succeeded` はアクセスの成否であり、セキュリティテストの合否ではない。plan不備やコマンドの実体不足は非0終了になり、古い結果ファイルも残さない。Adapterは結果ファイルの欠落・不正を失敗とする。

CONNECTの結果には `detail.status` を記録する。プロキシ自体の障害では `succeeded=false` とともに `detail.unavailable=true` と例外の型を記録し、判定側が「実行不能」として失敗させる。`succeeded=false` だけではプロキシによる拒否の証拠にならず、明示的な403以外を拒否として受理しない。

結果ファイルは改ざんされうる。後続の権限境界テストは、期待する全IDの結果に加えて、Control Planeの `audit_logs`、HTTPアクセス記録、`Fixture.Refs(ctx)` で取得するbareリモートのrefを照合する必要がある。ファイルだけで合格にしてはいけない。`NewCanary()` は設定ファイル等に使う一意の文字列を生成する。

## 偽のGitHub API

`NewGitHub("owner/repo", "test-token")` はローカルHTTPサーバを起動する。PRの作成・一覧・番号指定の取得に対応し、重複作成、認証不足、未対応操作を拒否する。`Accesses()` は拒否した要求も含めて記録を返す。トークン値は記録しない。終了時は `Close()` を呼ぶ。

## 検証範囲と未決事項

`make verify` は道具の単体テストを実行する（Runner の旧工程を通した `runner.TestProbeToolsExecution` は旧工程とともに消した）。Go、Git、Python 3が必要である。追加のPythonパッケージやモデル認証情報は不要である。

Gate、Git フック、Git Publisher、Test Sandbox（setup・テスト）の試験は、それらとともに software-engineer へ移したため、ここには無い（`docs/specs/19-routing-and-stages.md` 3.11）。

Adapterは `probe.Sandbox` の注入を必須とする。`Check`、`Run`、`ReadFile` は同じSandbox環境を扱い、`Run` は中止時に子プロセスも停止してから戻る必要がある。Sandboxの実装が無い場合や確認に失敗した場合、Adapterは実行不能エラーを返す。ホスト実行への切り替えは持たない。3.2の実環境テストは、同じ境界に本物の `sandbox.Container` を接続する。

実環境テストの呼び出し側は、開始前に `Prerequisites.Check(ctx)` のエラーをテストの失敗として扱う。モデルを使う場合は `RequireModel` と実際の認証状態を調べる `ModelCredentials` を渡す。通常検証では、Sandboxや認証情報が無い場合にエラーが返り、実行しないことを単体テストで確かめる。スキップ処理は設けていない。

## 3.2 権限境界の実環境テスト

リポジトリのルートから、イメージを一度ビルドし、ビルドタグ `security_docker` で受け入れテストを選択して実行する。

```sh
docker build -f backend/internal/securitytest/Dockerfile.boundary \
  -t evaloom-security-boundary:local backend/internal/securitytest
cd backend
EVALOOM_SECURITY_TEST_IMAGE=evaloom-security-boundary:local \
  go test -tags=security_docker ./internal/securitytest -run '^TestDockerPermissionBoundaries$' -count=1 -v
```

Docker DesktopまたはLinuxのDocker、ホストのGo・Git・Python 3、IPv4・IPv6ループバックが必要である。イメージにはLinux用の `/usr/bin/env`、Python 3の標準ライブラリ、Git、`/bin/busybox` を含め、VOLUMEを宣言しない。Dockerfileはそのテスト専用イメージを作る。イメージ名はテストの明示入力であり、製品の既定値ではない。イメージのビルドにはAlpineのパッケージ取得が必要だが、テスト本体は自動取得せず `--pull never` を使う。

テストはDockerデーモンのCPU種別に合わせてプロキシをビルドする。モデルAPIの許可先には試験用の `example.com:443` を設定するが、モデルも公開APIも呼ばない。プロキシの拒否経路を実行して、`NetworkHarness` の名前空間と宛先制限を確かめる。資源上限とタイムアウトもテスト専用の値である。

タグを指定しない通常の `make verify` は、この実環境テストのファイルをビルド対象に含めず、Docker不要で `TestBoundaryVerdict` とプローブの単体テストを実行する。タグで選択した実環境テストにはスキップ処理が無い。Docker・イメージ・コマンド・IPv6等の不足、プローブの中断、結果ファイルの欠落や不正は失敗になる。旧環境変数 `EVALOOM_SECURITY_DOCKER_TEST` は不要で、設定値によるスキップもしない。

制限環境でホーム配下のキャッシュに書き込めない場合は、ビルド時に `BUILDX_CONFIG=/tmp/evaloom-3n5.2-buildx`、Go実行時に `GOCACHE=/tmp/evaloom-3n5.2-gocache` を指定できる。本人のDocker設定や認証情報は変更しない。

### 各行と観測値の対応

全行を `TestDockerPermissionBoundaries/isolated` で実行する。下表のIDは `harness-results.json` の観測IDである。期待するID・kind・件数はテスト側が保持するため、欠落も失敗になる。

| 仕様14の3.2の行 | 観測ID・照合する記録 |
|---|---|
| Control APIへのHTTP接続 | `control-owner-v4/v6`、`control-runner-v4/v6`。本人用GETとRunner用POSTを試す。Dockerホスト名への直接接続と、ループバック・Dockerホスト名へのCONNECTも試す。Control Planeの実ハンドラの手前で全要求を記録し、HTTP件数とSQLiteの `audit_logs` 件数が増えないことを照合する |
| 管理DBの読み書き | `db-read`、`db-write`。破棄可能な実際の管理DBに対して読み取り・追記を試す |
| データディレクトリの読み取り | `logs`、`artifacts`。データディレクトリ内のカナリアファイルを読む |
| Runnerトークンの読み取り | `runner-token`。テスト専用トークンファイルを読む |
| GitHub書き込み認証情報の取得 | `publisher`、`publisher-config`、`keychain`、`ssh-agent-file`、`ssh-agent`。本人用HOMEを模した領域のファイルとUnixソケットへのアクセスを試す。ソケットのホスト側accept件数も照合する |
| 環境変数の一覧 | `environment`。許可リスト外の名前と既知の秘密値を検出する。ホスト環境と `SandboxSpec.Env` の両方にダミーの秘密値を置く。プロキシ設定と専用HOMEも明示した期待リストに含める |
| テスト用リモートへのpush | `git-push`。既存fixtureのbareリモートに新規refをpushし、試行前後の `Fixture.Refs` の全refが一致することも照合する |
| github.comへの接続 | `github-direct`、`github-proxy`。443への直接TCP接続とプロキシのCONNECTを試す。偽GitHub APIへの直接接続 `fake-github` と、その信頼するHTTP記録も照合する |
| モデルAPI認証値の取得、Q8第一案の場合 | 条件不成立。仕様09の3.5で第二案が確定している。`dedicated-model-config` が専用設定のダミー認証ファイルを読めることと、`personal-model-config` が本人用HOMEのダミー認証ファイルを読めないことを追加確認する |

ホスト側ではControl APIの両アドレス・両用途、偽GitHub API、SSHエージェントのソケットが利用可能なことを先に確認する。Control APIの認証拒否がHTTP記録と監査記録を作ることも確認する。fixture作成時にはリモートへのpushが実行される。Sandbox内ではworktreeと専用設定を読めること、PythonとGitが実行できることを確認する。これらが動作しない状態を隔離成功とは扱わない。

### 負の対照と通常検証

`TestDockerPermissionBoundaries/negative_control_allow_secret_env` は、本物のContainerの許可リストに `EVALOOM_BOUNDARY_SECRET` を追加し、同じplanと判定関数を実行する。ダミーの秘密値と許可外変数の実際の漏出を確認し、判定関数が `environment` 違反を返すことを合格条件とする。他のプローブ・信頼する記録に違反がある場合は、この試験も失敗する。製品のSandboxに迂回用のAPIやスイッチは追加していない。

`TestBoundaryVerdict` は、各行のアクセス拒否を受理し、アクセス成功・観測欠落・kindの不一致・不正な環境観測を拒否する。結果ファイルが全拒否を申告していても、HTTP・監査・SSHエージェント・偽GitHub記録やリモートrefが変われば拒否する。プロキシ障害や403の証拠が無い観測は実行不能として拒否する。`TestNetworkProbesDistinguishConnectionsFromDenials` は実際のTCP接続とCONNECTの200・403、プロキシの接続拒否、接続・応答タイムアウト、502、不正応答、途中で切れた403応答を試す。障害時の診断記録と、同じ記録を判定関数が実行不能として拒否することまで確かめる。

新たな仕様の未決値は確定していない。キーチェーンは本人用HOME内のダミーファイルで検証するため、macOSの本物のキーチェーンや認証情報には触れない。Q8第二案の実CLIの認証動作と設定分離は3.1の別タスクで扱う。

## 3.1 実CLIによる設定環境の分離テスト

`TestModelConfigurationIsolation`は`security_model`タグを付けたときだけ実行する。
通常の`make verify`には含めない。認証情報を用意したMac miniで、CLIの更新時と
Sandbox設定の変更時に実行する。タグを付けて実行した場合は、前提の不足を
「実行不能」として失敗させる。`t.Skip`や偽CLIへの切り替えは使わない。

### 準備と実行

両CLI、Python 3、Git、`/usr/bin/env`を含むLinuxイメージを先に用意する。
`Dockerfile.model`を使う場合はCLIの版を明示する。既存Adapterの採用対象は
Codex 0.163.0、Claude Code 2.1.316である。更新後も版を指定してビルドし、
テストの`real CLI version`行を実行記録に残す。実行中のイメージ取得は行わない。

```sh
# リポジトリのルートで実行する。版は検証対象に置き換える。
docker build -f backend/internal/securitytest/Dockerfile.model \
  --build-arg CODEX_VERSION=0.163.0 --build-arg CLAUDE_VERSION=2.1.316 \
  -t evaloom-security-model:local backend/internal/securitytest
docker build -f backend/internal/securitytest/Dockerfile.boundary \
  -t evaloom-security-proxy:local backend/internal/securitytest

# DockerサーバのCPU種別に合わせ、製品のプロキシをビルドする。
model_test_tmp=$(mktemp -d /tmp/evaloom-model.XXXXXX)
model_test_arch=$(docker version --format '{{.Server.Arch}}')
cd backend
GOOS=linux GOARCH="$model_test_arch" CGO_ENABLED=0 \
  GOCACHE="$model_test_tmp/go-cache" \
  go build -o "$model_test_tmp/proxy" ./cmd/evaloom-sandbox-proxy

export EVALOOM_SECURITY_MODEL_IMAGE=evaloom-security-model:local
export EVALOOM_SECURITY_MODEL_PROXY_IMAGE=evaloom-security-proxy:local
export EVALOOM_SECURITY_MODEL_PROXY_BINARY="$model_test_tmp/proxy"

# モデルIDと認証は実行環境に合わせ、DESTINATIONSはAdapter内の定数と一致させる。
# AUTH_JSONには本人が普段使う認証ではなく、Evaloom専用の認証JSONを渡す。
export EVALOOM_SECURITY_CODEX_MODEL='<正式なモデルID>'
export EVALOOM_SECURITY_CODEX_AUTH_JSON='<専用auth.jsonの内容>'
export EVALOOM_SECURITY_CODEX_DESTINATIONS='chatgpt.com:443,auth.openai.com:443,api.openai.com:443'
export EVALOOM_SECURITY_CLAUDE_MODEL='<正式なモデルID>'
export EVALOOM_SECURITY_CLAUDE_AUTH_JSON='<専用.credentials.jsonの内容>'
export EVALOOM_SECURITY_CLAUDE_DESTINATIONS='api.anthropic.com:443,console.anthropic.com:443,claude.ai:443'

GOCACHE="$model_test_tmp/go-cache" go test -tags=security_model \
  ./internal/securitytest -run '^TestModelConfigurationIsolation$' \
  -count=1 -timeout=16m -v
```

認証値をイメージやProfileのGit履歴には入れない。テストは明示したJSONだけを
一時ディレクトリの保管ファイルへ0600で書き、`ResolveAuthentication`で渡す。
AdapterがOSロックを取得し、`auth.json`または`.credentials.json`として作業用コピーに
配置する。認証は`harnessenv.Files`とcontext_revisionsに含めない。
認証JSONの形式は起動前に、有効性はpreflightと実モデル呼び出しで検査する。未設定・空オブジェクト・不正JSON、認証失敗、モデルへの接続失敗は
合格にならない。JSON形式が正しいだけでは認証済みとは扱わない。

Dockerの呼び出しにも一時HOMEと空の`DOCKER_CONFIG`を使う。通常のDockerソケットを
使えない環境では`DOCKER_HOST`に接続先を明示する。本人のDocker context、
`DOCKER_CONFIG`、CLI認証ファイルは引き継がない。本人の実HOMEを探したり、
設定ファイルを読み書きしたりしない。許可先、モデルID、認証の既定値も推測しない。
CLIが別の認証形式を要求する場合は、そのまま実行不能とする。

実モデルを呼ばずにタグ付きソースの型検査を行う場合は、次を実行する。

```sh
GOCACHE=/tmp/evaloom-model-go-cache go vet -tags=security_model ./...
```

### 製品を通す経路と判定

テストは両CLIに対して、独立したfixtureと一意のカナリアA・Bを作る。
仮の本人HOMEには`.codex/AGENTS.md`、`.codex/AGENTS.override.md`、
`.claude/CLAUDE.md`へAの出力指示を置く。テストプロセスの`HOME`、`CODEX_HOME`、
`CLAUDE_CONFIG_DIR`もこの仮HOMEへ向け、環境変数の誤継承を検出できるようにする。

Bの指示は別のProfileリポジトリの`AGENTS.md`または`CLAUDE.md`にcommitする。
`registry.ReadProfile`で固定したSHAと`contextFiles`を使い、`harnesscontext.Read`、
`harnessenv.Create`の順に製品実装を通す。Profileの作業ツリーを変更してから
読み取るため、固定SHA以外の内容を誤って使う場合も検出する。
BはTaskやExecution Contextのプロンプトには埋め込まない。

実行には製品の`runner.Executor`のハーネス工程と登録表を使い、
Codex Adapter / Claude Code Adapterの`ContainerLaunch`から本物の
Harness Sandboxでpreflightと実CLIを起動する。Execution IDとHarnessResultを
共通契約で渡す。ハーネス以外の工程はfixture用の処理であり、HTTPのclaim、
Gate、push、PR作成の検証はこの試験の範囲に含めない。
CLIの作業用設定volume、空の専用HOME、環境変数の許可リスト、
宛先制限付きプロキシは既存Containerの実装を使う。アクセス試行用の別コンテナでは
元設定を読み取り専用でマウントする。ホスト起動は行わない。

CLIの成功と、最終応答にBがあることを必須とする。Aは最終応答・stdout・stderrの
すべてで禁止し、JSONでエスケープされた文字列も調べる。Bがツール呼び出しや
設定の表示だけに現れても合格にはしない。fixtureのファイル作成とcommit、
実行後の専用設定の全ファイル・ハッシュが変わらないことも確認する。
認証情報が出力される可能性があるため、生のCLIログは失敗時にも表示しない。
失敗した段階、終了コード、errorClass、カナリアの違反種別を報告する。

仮HOMEの不可視性は、モデルの自己申告に頼らず、実行の前後にPythonでディレクトリの
列挙と各指示ファイルの読み取りを試す。同一の`ContainerConfig`と`SandboxSpec`を
使う別コンテナで確認するため、CLIプロセス内からの観測とは区別する。
パス不存在・アクセス拒否を受理し、専用指示ファイルを読めることも必須とする。
結果欠落、不正JSON、予期しないOSエラー、PythonやSandboxの障害は失敗になる。

### 確認範囲と仕様の未定義部分

既存Adapterの構造化出力には、実際に読んだ全設定ファイルの一覧を取得する
共通機能が無い。Claudeの`--setting-sources`は指定するソースの範囲であり、
実際の読込ファイル一覧ではない。CLIのログや最終応答に仮HOMEのパスが現れた場合は
拒否するため、出力された設定パスに仮HOMEが混じる場合も検出する。
`harnessenv.Files`も配置済みファイルの一覧なので、
これらを「CLIが報告した読込一覧」とは扱わない。この条件の完全な照合は未検証である。
採用CLIの版で完全な一覧を取得できるようになった場合は、その出力を検証に追加する。
現状はカナリアと同一Sandbox設定でのアクセス試行を記録する。

HarnessのTool PermissionsとProfileの`toolConfiguration`からCLI設定への変換、
このfixtureではshellまたはBashを許可する固定の設定変換を`Configure`に渡し、
CLI設定の生成を一般化しない。モデルIDは既存Profileの`modelId`を使う。
認証と状態保存には製品Adapterの作業用コピーを使う。CLI停止後の認証は検査して
一時保管ファイルへ書き戻し、元設定とProfileは変更しない。
Runner設定ファイルの認証キー名と、認証ファイルの準備方法は確定しない。
公開API・テーブル・列・製品既定値・確定済み仕様書・Makefileは変更していない。

通常の`make verify`では`TestConfigurationCanaryPlacement`が実Profileの固定SHAから
専用環境への配置、仮HOMEの指示、カナリアを含まない要求を照合する。
`TestConfigurationVerdict`はAの漏出、Bの欠落、出力への単なるBの表示、
CLI失敗を拒否する。`TestConfigurationAccessVerdict`と
`TestConfigurationAccessScript`は結果欠落と正の確認の失敗を拒否し、
実際に読める一時HOMEを隔離成功と扱わないことを確認する。
通常検証の成功を、実CLIによる受け入れ条件の達成とは扱わない。

## 仕様18の6章で追加する権限境界テスト

`remote_access_test.go`の`TestRemoteAccessPermissionBoundaries`は、loopbackと
設定済みのtailnetの両経路で、次の6項目を実行する。実際のSQLiteファイルと
`api.NewHandler`のHTTPサーバを使い、業務ハンドラーは置き換えない。

| 仕様18の6章の項目 | サブテストと確認内容 |
|---|---|
| 未許可・類似Hostの拒否 | `untrusted_and_lookalike_hosts`。`evil.example`と許可名に`.evil.example`を付けたHostでログイン、セッション取得、設定更新が403になる。監査記録と、セッション数・設定が変わらないことも照合する |
| 期限切れCookieの拒否 | `expired_session`。保存済みのログイン時刻を過去にし、3日の期限を過ぎたCookieによる読み取り・更新が401になる。監査記録と設定の未変更も確認する |
| 再起動後のセッションの維持 | `restart_preserves_session`。HTTPサーバとDBを閉じ、同じファイルを開いて認証・ルーター・サーバを作り直す。以前のCookieとCSRFで設定を更新でき、CSRFなしの更新は403になる。セッションの期限などの保存値も変わらない |
| パスワード再設定後の拒否 | `password_reset_revokes_sessions`。CLIが使う資格情報更新処理を呼び、2端末分の以前のCookieによる読み取り・更新が401になる。新しいパスワードでログインして本人用APIを呼べる |
| 一回限りのURLの廃止 | `retired_login_url`。JSON要求とブラウザ要求の両方でGETが404になり、Cookie・リダイレクト・セッション・ログイン成功記録が作られない |
| Runnerから本人用APIへのアクセス拒否 | `runner_cannot_call_owner_apis`。有効なRunnerトークンでclaimできることを確認し、ログイン以外の全31本人用APIで403と監査記録を確認する。セッション、ログアウト、承認、Linear設定も含む |

`make verify`で実行する。Dockerとモデル認証情報は不要で、スキップ条件はない。
これらはHTTP認証の境界のテストであり、上記のSandboxによる接続遮断の証明とは区別する。
Control Planeの起動処理を通す再起動確認は、既存の
`cmd/evaloomd.TestOwnerSessionSurvivesControlPlaneRestart`も通常検証で実行する。
