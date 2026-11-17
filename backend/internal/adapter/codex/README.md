# Codex Adapter

仕様の正本は`docs/specs/08-harness-adapter.md`。共通の`adapter.Adapter`を実装し、
Runnerが渡す`Launch`を通じて隔離環境内でCLIを起動する。ホストで直接実行する経路は持たない。

## 起動

採用版は`codex-cli 0.163.0`。ローカルの`codex exec --help`と
[OpenAI公式の非対話実行資料](https://learn.chatgpt.com/docs/non-interactive-mode)、
[CLIリファレンス](https://learn.chatgpt.com/docs/cli/reference)で引数とJSONLの形を確認した。

1. `/bin/sh -c 'exec "$@"' evaloom-codex-preflight codex login status`
2. `codex exec --json --color never --ephemeral --skip-git-repo-check --sandbox danger-full-access --model <model> --cd <workspace> -- <prompt>`

認証確認と本実行は、同じ作業用の設定コピーを使う。設定・CLI・認証の不備は認証確認で検出する。
固定のシェル式には引数を位置引数として渡し、プロンプトをシェルのソースに埋め込まない。
`--skip-git-repo-check`により、Gitリポジトリ以外の作業領域も使える。
実際のアクセス制限はWork Sandboxが受け持つ。上記の引数でホスト実行へ切り替えてはならない。

プロンプトは固定済みのExecution Contextと、外部由来のタスク内容として区切ったJSONで作る。
Runnerは要求JSONと回答をExecution Contextの入力に含める。Adapterからcommitや公開の指示は足さない。
概要・質問の作成方法はProfileの指示に従い、Runnerが`out/summary.md`と`out/question.json`を読む。
Adapterは作業領域、commit、差分、HEADを解釈しない。

## セッションと結果

`Start`は非同期処理を操作するローカルのハンドルを返す。
`Status`・`Logs`・`Cancel`・`Result`にはこのハンドルを渡す。
結果の`SessionID`にはJSONLの`thread.started.thread_id`を返し、Runnerが
`executions.work_session_id`に保存する。CLIがセッションを返す前の失敗では空文字を返す。
成功には終了コード0、有効なセッションID、`turn.completed`、不正出力と失敗イベントが無いことが必要である。
未知のイベントは許容し、セッションIDの欠落・競合、不正JSON、終了イベントの欠落は`work_failed`にする。

`item.completed`の最後の`agent_message.text`は互換インターフェースの`Summary`に返す。
Runnerの成果物となる概要は`out/summary.md`であり、このCLIメッセージでは決めない。
`turn.completed.usage`から入力・出力トークン数を返し、不明ならnullを維持する。

同じセッションの再開は、作業用コピーを戻した実CLIでの検証が済んでいないため、
`Capabilities().Resume`はfalseにする。`--ephemeral`を使い、Runnerは質問への回答を含む
新しいセッションへ進む。Profileが`resume: required`ならRunnerが`work_failed`にする。

| 状況 | errorClass |
|---|---|
| CLI・設定・認証が不正、権限を変換できない、設定コピーの照合失敗 | `config_error` |
| 本実行が非ゼロ終了、出力やセッションが不正 | `work_failed` |
| Sandboxの作成・隔離・終了確認の失敗 | `sandbox_error` |
| contextの期限または要求の上限時間を超過 | `timeout` |
| `Cancel`または呼び出し元contextによる中止 | `cancelled` |

Adapterは`check_failed`を付けない。終了コード128以上でも、OOMやruntime障害が確認されなければ
CLIの異常終了として扱う。中止ではTERMを送り、Runnerが渡した`CancelGrace`の後も動いていれば
Sandbox全体を強制終了する。`Result`は全書き込みプロセスの停止と後始末を待つ。作業領域は残す。

## 設定・認証・権限

`ResolveConfig`はProfileから作った読み取り専用の`harnessenv.Environment`を返す。
Adapterは既存の`WorkingCopy`でコピーを作り、`CODEX_HOME`をコピーへ向ける。
Profile由来のファイルを起動直前に記録済みハッシュと照合し、元の設定はマウントしない。
`ContainerLaunch`は既存の`Container.RunControlled`へ接続し、確認済みの内容を専用volumeへ転送する。
専用HOME、マウント検査、資源制限は既存のSandboxに従う。

`ResolveAuthentication`はEvaloom専用の認証保管パスを返す。同じ認証を使う処理には同じ絶対パスを返す。
Adapterは既存のOSロックを取り、認証ファイルを作業用コピーの`auth.json`に配置する。
個人のHOMEから認証を取得しない。起動後の認証回収、原子的な書き戻し、質問時のコピー保持は
既存の`harnessenv`と`sandbox`を使う。詳細は[`harnessenv/README.md`](../../harnessenv/README.md)を参照する。
認証内容とハッシュはcontext_revisionsに含めない。

stdout・stderrは行単位で認証値を除外し、実行中から`Logs`へ渡す。
各コマンド終了後に認証値を読み直し、以降の除外対象に加える。回収できなければ以降のCLI出力を伏せる。
Runner側でも保存・配信前に機密情報を除外する。認証を書き戻せなくても、終了済みの実行結果は変えない。

`ResolveToolPermissions`はclaim時に固定した`stages.work.toolPermissions`を返す。
Profile登録と起動前の両方が同じ`CheckToolPermissions`を使う。
`allow`と`deny`だけを受け付け、両方が空のときだけ起動する。旧`toolConfiguration.allowedTools`は使わない。
未知キー、配列以外、空文字や文字列以外の要素、非空の規則は拒否する。
CLIの版にツール単位の制御が無いため、架空の許可設定は作らない。

モデルAPIの宛先はパッケージ内で`chatgpt.com:443`、`auth.openai.com:443`、`api.openai.com:443`に限定する。
`ContainerLaunch`は呼び出し元の任意の宛先で拡張しない。Runnerの段実行ではAdapterの宣言を使う。
登録は`registry.Register("codex", codex.New(config))`で行い、新しいHTTP APIやDB項目は追加しない。

## 検証

通常のGoテストは既存のLaunch境界で偽CLI・偽runtimeを使い、引数・環境・ファイル変更、
エラー分類、中止、設定照合、認証の書き戻しを確認する。モデルAPIには接続しない。
Profile登録テストは実際のAPIとCodex Adapterの組み合わせで422の拒否と空権限の登録を確認する。

実CLIによる受け入れ試験は`internal/securitytest/codex_model_test.go`の
`TestCodexWorkspaceChangeAndCancellation`に置く。`security_model`タグを指定し、
[`securitytest/README.md`](../../securitytest/README.md)の3.1と同じイメージ・プロキシ・
Codex専用認証・モデルIDを用意して実行する。

```sh
go test -tags=security_model ./internal/securitytest -run '^TestCodexWorkspaceChangeAndCancellation$' -count=1 -v
```

この試験は本番の`ContainerLaunch`と実Codexを使い、ファイル内容、commitされていないこと、
CLIが返すセッションID、中止結果と作業領域の保持を確認する。必要な設定が無ければ失敗とし、
偽CLIやホスト直接実行へ切り替えない。通常の`make verify`にはモデルAPIを使う試験を含めない。
