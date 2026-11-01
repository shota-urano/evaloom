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

`make verify` は道具の単体テストと `runner.TestProbeToolsExecution` を実行する。Go、Git、Python 3が必要である。追加のPythonパッケージやモデル認証情報は不要である。

工程試験は本物のExecutorとclaim/result APIを使い、全14工程を通してDBの `succeeded` を確認する。fixtureのスクリプト、JUnit、commit、ローカルpush、偽APIへのPR作成も実行し、永続化された `verified_sha` と `pushed_sha` をリモートのSHAと照合する。工程の中身にはテスト用の実装を渡している。GateとGit Publisherの本番実装や仕様14の3.2以降の権限境界テストは、この試験の対象ではない。

Adapterは `probe.Sandbox` の注入を必須とする。`Check`、`Run`、`ReadFile` は同じSandbox環境を扱い、`Run` は中止時に子プロセスも停止してから戻る必要がある。Sandboxの実装が無い場合や確認に失敗した場合、Adapterは実行不能エラーを返す。ホスト実行への切り替えは持たない。工程試験のホスト実行は、生成済みfixture専用の `_test.go` 内のテストダブルだけで行い、隔離の証明には使わない。3.2の実環境テストは、同じ境界に本物の `sandbox.Container` を接続する。

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

## 3.3 Test Sandboxの隔離突破テスト

3.2と同じイメージ、プローブ、判定関数、Control Planeの観測用ハンドラを再利用する。イメージが未作成なら上記の`docker build`を実行したうえで、リポジトリのルートから次を実行する。

```sh
cd backend
EVALOOM_SECURITY_TEST_IMAGE=evaloom-security-boundary:local \
  GOCACHE=/tmp/evaloom-3n5.3-gocache \
  go test -tags=security_docker ./internal/securitytest \
  -run '^TestDockerTestSandboxIsolation$' -count=1 -v
```

`isolated/setup`はfixtureの`python3 setup.py`を`TestSetup / NetworkSetup`で実行する。続く`isolated/test`は`python3 test.py`を`TestRun / NetworkNone`で実行する。各工程の前後でHTTPアクセス件数と実SQLiteの`audit_logs`件数を照合する。本人用・Runner用APIに対する認証拒否も記録するため、HTTPの401や403を隔離成功とは扱わない。ホストから両APIのIPv4・IPv6接続と記録の作成を先に確認する。結果ファイルの欠落・不正、期待ID・kind・件数の不一致、実行終了コードの異常も失敗になる。

| 仕様14の3.3の行 | setupとテストの観測ID・照合 |
|---|---|
| Control APIへの接続 | `control-owner-v4/v6`、`control-runner-v4/v6`。Dockerホスト名・gatewayへの直接接続も試す。setupではループバック・Dockerホスト名・gatewayへのCONNECTも拒否されることを確認する。両工程でHTTPアクセス記録と`audit_logs`が増えないことを照合する |
| 管理DBの読み書き | `db-read`、`db-write`。破棄可能な実DBの読み取り・追記を試す。`logs`と`artifacts`の読み取りも試す |
| 秘密情報 | `runner-token`、`publisher`、`publisher-config`、`ssh-key`、`keychain`、`home-canary`、`personal-home`、`project-dotenv`、`worktree-dotenv`、`worktree-dotenv-exists`、`model-config`。すべてテスト用の秘密値を使う。コピー対象外の`.env`は元プロジェクトとworktree内の両パスを試す。Test Sandboxには専用モデル設定も渡さない。`environment`で許可外の変数と既知の秘密値も検出する |
| Dockerソケット等の管理インターフェース | `management-socket-*`。ホスト上に実在するテスト用Runnerソケットと、Docker・containerd・Docker Desktopの既知のソケットパスの存在を調べる。`SSH_AUTH_SOCK`も環境変数の許可対象外とする |
| 外部ネットワーク、テスト実行 | `registry-direct`、`external-direct`、`external-ip-direct`でレジストリ・github.com・公開IPへの直接TCP接続を拒否する。`proxy-absent`でローカルプロキシも存在しないことを確かめる |
| 外部ネットワーク、setup | 同じ3つの直接TCP接続を拒否する。`external-proxy`、`external-ip-proxy`、`registry-wrong-port`はCONNECTの明示的な403を必須とする。`registry-allowed`では許可したレジストリへのCONNECTの200を必須とし、プロキシの故障や全遮断による見かけの成功を防ぐ |

このfixtureでは`registry.npmjs.org:443`だけを`AllowedDestinations`に渡す。製品の許可先や既定値を決めるものではない。外部の到達性が無い状態をアクセス拒否と取り違えないよう、開始前に同じDockerデーモンのbridgeコンテナからレジストリ・github.com・公開IPの443へTCP接続する。これらが利用できない環境では実行不能として失敗し、スキップしない。setupの許可先へのCONNECTが失敗した場合も実行不能として扱う。3.2と同様、プロキシ障害、502、不正応答、403の証拠不足は拒否として受理しない。テスト実行にはプロキシを用意しないため、その工程では`proxy_connect`を使わない。

正の確認としてworktreeの読み書き、setup出力のテスト工程からの読み取り、実際のunittestの2件実行とJUnitの失敗・エラー・skipがゼロであることも確認する。bareリモートのrefも各工程の前後で照合する。

`negative_control_allow_secret_env/setup`と`negative_control_allow_secret_env/test`では本物のContainerの許可リストにダミーの秘密変数だけを追加する。同じplanと判定関数で実際の漏出と`environment`違反を検出し、他の観測や信頼する記録に異常が無いことを確認する。

通常の`make verify`ではDocker試験をビルド対象から外し、`TestIsolationVerdict`で両工程のplanを使って拒否の受理、全項目のアクセス成功や正の確認の失敗、観測の欠落・kind不一致、秘密変数の漏出、プロキシの実行不能、結果が全拒否でもHTTP・監査記録が増える場合の拒否を検証する。3.2の実環境テストと通常テストは変更していない。

仕様の穴は既存Sandboxと同じで、Projectごとの許可レジストリ一覧の保存先とRegistryからの接続方法が未定義である。このテストは既存の内部`AllowedDestinations`境界を使い、公開API・テーブル・列・確定済みの仕様書を追加・変更していない。`.env`のコピー処理自体の検証はworktree担当のテストにあり、この試験は秘密ファイルをworktreeへコピーしないfixtureでの隔離を検証する。

## 3.4 ホスト側Git操作の防御テスト

`runner.TestHostGitDefense` は通常の `make verify` で実行する。プローブAdapterから `git_attack.py` を実行し、作業用クローンのGit領域に次の仕込みを置く。各ケースは独立したfixtureを使う。

| ケース | 仕込みと検証 |
|---|---|
| `default-hooks` | 既定のhooksディレクトリに `post-checkout`、`pre-push`、`reference-transaction` を置く |
| `included-hooks` | include先の `core.hooksPath` で同じ3つのフックを指定する |
| `worktree-fsmonitor` | 有効な `config.worktree` の `core.fsmonitor` に実行ファイルを指定する |
| `included-filters` | include先に `filter.Defense.Driver.clean/smudge/required` を設定し、追跡対象の `.gitattributes` で適用する |

仕込みは実行されたらGit共有領域の `defense-marks` に印を作る。プローブは実装ファイルの変更をcommitし、作業用クローンのrefと設定も変更してから、フック・実行系設定を有効にする。Runnerのworktree作成・checkout・fetch、実際のGate 2・3、Git Publisherの `Push` とリモートSHA照合を進め、各段階で印が無いことを確認する。push先には破棄可能なbareリモートをGit daemon経由で使い、外部サービスには送らない。

信頼する側では、HTTP result APIで永続化された `succeeded`、Gate 2・3の `pass`、検証SHAに対する2件のテスト成功を照合する。リモートはpush前に全refが不変で、push後は検証済みSHAのブランチだけが追加されたことを確認する。本人のリポジトリを模した元リポジトリは全refと設定ファイルを前後で比較し、作業用クローンへの変更が及ばないことを確認する。プローブのJSONだけで合格にはしない。

すべての防御確認が済んでから、防御ラッパーを使わないGitで同じ仕込みを実行する。checkout・commit・push、status、worktree作成・addによって各印が実際に作られることも必須とし、仕込みが動作しないために合格するケースを防ぐ。

本物のHarness/Test Sandboxを使う試験は、3.2と同じ既存イメージを指定して実行する。

```sh
cd backend
EVALOOM_SECURITY_TEST_IMAGE=evaloom-security-boundary:local \
  GOCACHE=/tmp/evaloom-3n5.4-go-cache \
  go test -tags=security_docker ./internal/runner \
  -run '^TestDockerHostGitDefense$' -count=1 -v
```

Docker版は通常版と同じ判定を使い、プローブを本物の `sandbox.Container` のHarnessで実行する。Gate 2の基準値・検証テストも本物のTest Sandboxで実行する。決定的なGit設定の書き込みだけを試すためネットワークは `NetworkNone` とし、Harnessにはworktree、作業用クローンのGit領域、専用設定だけを渡す。Test Sandboxにはworktreeだけを渡す。モデル認証情報とプロキシは不要である。イメージ未指定、Docker・コマンドの不足、結果ファイルの欠落・不正は失敗になり、スキップもホスト実行への切り替えも行わない。

通常版のSandbox部分は生成したfixture専用のプロセスダブルであり、隔離の証明には使わない。両版ともホスト側のGit、Gate 2・3、Publisherのpushは実装本体を使う。他の工程にはテスト用の境界実装を渡す。実行系設定全体の列挙と個々の抑止は `safegit` のテストが扱い、この試験はRunnerとPublisherを通す経路を検証する。新たな製品の設定値、公開API、テーブル、列は追加していない。

## 3.5 Gateの受け入れテスト

`runner.TestGateAcceptance`は通常の`make verify`で7ケースを実行する。Python、Node.js、Gitが必要になる。プロセスダブルは生成したfixtureだけを実行する。本物のSandboxを使う試験は、リポジトリのルートから次の手順で実行する。

```sh
docker build -f backend/internal/securitytest/Dockerfile.gates \
  -t evaloom-security-gates:local backend/internal/securitytest
cd backend
EVALOOM_SECURITY_TEST_IMAGE=evaloom-security-gates:local \
  GOCACHE=/tmp/evaloom-3n5.5-go-cache \
  go test -tags=security_docker ./internal/runner \
  -run '^TestDockerGateAcceptance$' -count=1 -v
```

`TestDockerGateAcceptance`は同じ7ケースと判定を使う。Harnessにはworktree、fixtureのGit共有領域、専用設定を渡す。基準値の取得とGate 2のTest Sandboxにはworktreeだけを渡す。両方ともネットワークを許可しない。イメージ未指定、Docker・Python・Git・Node.jsの不足、プローブ結果やJUnitの欠落は失敗になる。スキップやホスト実行への切り替えは行わない。

| ケース | 変更と確認 |
|---|---|
| `delete_test` | 既存テストファイルを削除してcommitする。残った1件を実行し、`blocked(gate2_missing_tests)`になる |
| `describe_only` | 新規ファイルに`describe.only`を追加してcommitする。実際の`node:test`で新規テストを実行し、既存2件のIDが欠けることで`blocked(gate2_missing_tests)`になる |
| `exclude_test` | fixtureのテスト設定から既存ファイルを外してcommitする。残った1件を実行し、同じGate 2のBlockになる |
| `increase_skip` | 既存テストを`it.skip`に変更してcommitする。skipが0から1に増え、同じGate 2のBlockになる |
| `commit_after_gate2` | 実装変更をcommitし、Gate 2が成功してからテスト用の工程フックで追加commitする。検証SHAを保持したまま`blocked(gate3_mismatch)`になる |
| `untracked_after_gate2` | Gate 2が成功してから未追跡ファイルを作る。HEADは検証SHAと同じでも`blocked(gate3_mismatch)`になる |
| `implementation_only` | 実装ファイルだけを変更する。既存2件が成功し、Gate 2・3を通過してpushとPR作成が成功する |

fixtureは`node:test`の実行結果をJUnitで出力する。`gate_test.py`は設定から対象ファイルを列挙し、`describe.only`がある場合にNodeの`--test-only`を有効にする。ケース別の固定XMLは生成しない。Nodeの版によって非対象ファイルがJUnitに現れる形式は異なるため、focusケースは件数だけで判定せず、保存されたID一覧から既存2件の欠落と新規テストの実行を確認する。

プローブAdapter、Runnerの工程制御、基準値の取得、Gate 2・3、Publisherの`Publish`と`Push`は実装本体を使う。テスト用フックはGate 2の判定完了後、Gate 3の実行前にだけ変更を差し込む。DBへの直接書き込みはclaim前の入力準備と、テスト用Recorderによるpush直後の保存に使う。それ以外の結果は実際のHTTP result/artifacts APIで保存する。保存されたテストID・件数・SHA、ExecutionとTaskの状態、GateとBlockの関連、Block原因、通知要求を照合する。

Blockedの6ケースではPublisherの呼び出しがゼロ、偽GitHubへのアクセスがゼロ、bareリモートの全refが実行前後で同じことを確認する。成功ケースでは変更ファイルが実装1つだけであること、リモートの追加ブランチ・保存された`pushed_sha`・`verified_sha`・ハーネスのHEADがすべて同じこと、ベースブランチが変わらないこと、偽GitHubでPRが作成され、そのhead/baseと本文の検証SHAが正しいことを確認する。

GitHubには既存の偽HTTP APIを使い、テスト専用の`PullRequests`実装で接続する。既存の仕様上の穴として、push直後からPR作成前までに`pushed_sha`を永続化するRunner APIが無い。既存の`PublicationRecorder`境界に渡すテスト用Recorderは、既存DBの`executions.pushed_sha`と`branch_name`への書き込みを確定してから成功を返す。偽PR APIは作成要求の受信時点でDBを読み直し、状態が`publishing`、ブランチが対象と同じ、`pushed_sha`と`verified_sha`が空でなく一致していることを検証してからPRを作る。この確認が成功ケースで1回行われることも必須とし、最終result APIで初めてSHAを保存する実装では合格しない。最終状態とPR情報は引き続き既存result APIで保存する。新しい公開API、テーブル、列、製品の設定値は追加していない。
