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
| `proxy_connect` | `host`, `port`, `timeoutSec` | SandboxのプロキシへのCONNECT。200は接続成功、403は拒否、それ以外の応答は実行不能。単体テストでは `proxyPort` でプロキシのポートを渡せる |
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

結果ファイルは改ざんされうる。後続の権限境界テストは、期待する全IDの結果に加えて、Control Planeの `audit_logs`、HTTPアクセス記録、`Fixture.Refs(ctx)` で取得するbareリモートのrefを照合する必要がある。ファイルだけで合格にしてはいけない。`NewCanary()` は設定ファイル等に使う一意の文字列を生成する。

## 偽のGitHub API

`NewGitHub("owner/repo", "test-token")` はローカルHTTPサーバを起動する。PRの作成・一覧・番号指定の取得に対応し、重複作成、認証不足、未対応操作を拒否する。`Accesses()` は拒否した要求も含めて記録を返す。トークン値は記録しない。終了時は `Close()` を呼ぶ。

## 検証範囲と未決事項

`make verify` は道具の単体テストと `runner.TestProbeToolsExecution` を実行する。Go、Git、Python 3が必要である。追加のPythonパッケージやモデル認証情報は不要である。

工程試験は本物のExecutorとclaim/result APIを使い、全14工程を通してDBの `succeeded` を確認する。fixtureのスクリプト、JUnit、commit、ローカルpush、偽APIへのPR作成も実行し、永続化された `verified_sha` と `pushed_sha` をリモートのSHAと照合する。工程の中身にはテスト用の実装を渡している。GateとGit Publisherの本番実装や仕様14の3.2以降の権限境界テストは、この試験の対象ではない。

Adapterは `probe.Sandbox` の注入を必須とする。`Check`、`Run`、`ReadFile` は同じSandbox環境を扱い、`Run` は中止時に子プロセスも停止してから戻る必要がある。Sandboxの実装が無い場合や確認に失敗した場合、Adapterは実行不能エラーを返す。ホスト実行への切り替えは持たない。工程試験のホスト実行は、生成済みfixture専用の `_test.go` 内のテストダブルだけで行い、隔離の証明には使わない。3.2の実環境テストは、同じ境界に本物の `sandbox.Container` を接続する。

実環境テストの呼び出し側は、開始前に `Prerequisites.Check(ctx)` のエラーをテストの失敗として扱う。モデルを使う場合は `RequireModel` と実際の認証状態を調べる `ModelCredentials` を渡す。通常検証では、Sandboxや認証情報が無い場合にエラーが返り、実行しないことを単体テストで確かめる。スキップ処理は設けていない。

## 3.2 権限境界の実環境テスト

リポジトリのルートから、イメージを一度ビルドし、明示的に有効化して実行する。

```sh
docker build -f backend/internal/securitytest/Dockerfile.boundary \
  -t evaloom-security-boundary:local backend/internal/securitytest
cd backend
EVALOOM_SECURITY_DOCKER_TEST=1 EVALOOM_SECURITY_TEST_IMAGE=evaloom-security-boundary:local \
  go test ./internal/securitytest -run '^TestDockerPermissionBoundaries$' -count=1 -v
```

Docker DesktopまたはLinuxのDocker、ホストのGo・Git・Python 3、IPv4・IPv6ループバックが必要である。イメージにはLinux用の `/usr/bin/env`、Python 3の標準ライブラリ、Git、`/bin/busybox` を含め、VOLUMEを宣言しない。Dockerfileはそのテスト専用イメージを作る。イメージ名はテストの明示入力であり、製品の既定値ではない。イメージのビルドにはAlpineのパッケージ取得が必要だが、テスト本体は自動取得せず `--pull never` を使う。

テストはDockerデーモンのCPU種別に合わせてプロキシをビルドする。モデルAPIの許可先には試験用の `example.com:443` を設定するが、モデルも公開APIも呼ばない。プロキシの拒否経路を実行して、`NetworkHarness` の名前空間と宛先制限を確かめる。資源上限とタイムアウトもテスト専用の値である。

`EVALOOM_SECURITY_DOCKER_TEST=1` が無い場合だけスキップする。有効化後のDocker・イメージ・コマンド・IPv6等の不足、プローブの中断、結果ファイルの欠落や不正は失敗になる。通常の `make verify` はDocker不要で、`TestBoundaryVerdict` とプローブの単体テストを実行する。

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

`TestBoundaryVerdict` は、各行のアクセス拒否を受理し、アクセス成功・観測欠落・kindの不一致・不正な環境観測を拒否する。結果ファイルが全拒否を申告していても、HTTP・監査・SSHエージェント・偽GitHub記録やリモートrefが変われば拒否する。`TestNetworkProbesDistinguishConnectionsFromDenials` は実際のTCP接続とCONNECTの200・403を試し、壊れたプロキシ応答をアクセス拒否として記録しないことを確かめる。

新たな仕様の未決値は確定していない。キーチェーンは本人用HOME内のダミーファイルで検証するため、macOSの本物のキーチェーンや認証情報には触れない。Q8第二案の実CLIの認証動作と設定分離は3.1の別タスクで扱う。
