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

Sandboxの実体は未決のため、Adapterは `probe.Sandbox` の注入を必須とする。`Check`、`Run`、`ReadFile` は同じSandbox環境を扱い、`Run` は中止時に子プロセスも停止してから戻る必要がある。Sandboxの実装が無い場合や確認に失敗した場合、Adapterは実行不能エラーを返す。ホスト実行への切り替えは持たない。工程試験のホスト実行は、生成済みfixture専用の `_test.go` 内のテストダブルだけで行い、隔離の証明には使わない。

実環境テストの呼び出し側は、開始前に `Prerequisites.Check(ctx)` のエラーをテストの失敗として扱う。モデルを使う場合は `RequireModel` と実際の認証状態を調べる `ModelCredentials` を渡す。通常検証では、Sandboxや認証情報が無い場合にエラーが返り、実行しないことを単体テストで確かめる。スキップ処理は設けていない。

仕様14の3.6は実環境テストの実行時期を定めているが、起動コマンドは決めていない。実体のあるSandboxとモデル認証を必要とする試験の起動方法は、後続タスクで確定する必要がある。このタスクでは隔離方式、認証方式、ネットワークポリシーを決めていない。
