# 起動コマンドと工程表の接続

`Runner.ExecuteClaim`が仕様05の13工程を組み立てる。起動コマンドはclaimを
この処理へ渡す。認証設定が無い場合や、必要な内部境界を取得できない場合は
`failed/config_error`を報告する。未実装の境界を成功扱いにはしない。

Projectのパス検証、作業用クローン、worktreeの作成・コピー・再利用、
commitに固定したContext、setup、基準値、Adapter、Gate 2・3、Publisher、
ログと使用量には既存の部品を使う。各Executionは専用の工程ハンドラを持つ。
本番のSandboxはContainerであり、ホスト実行への代替経路は設けていない。

工程の期限は設定の`defaultTimeouts`から作り、Projectの同名の値を優先する。
Sandboxにも工程の残り時間を渡す。キャンセルの猶予とruntime制御コマンドの
上限は、仕様05の4.1で認められている実装定数として各5秒にする。
Gitの作者情報は`commitAuthor`からExecution専用のGit設定環境変数で渡す。
既存のHarness認証ロック、設定コピーの照合、認証の書き戻しはAdapterが行う。
ログの除外処理にはstdout・stderrをそれぞれ継続したストリームで渡す。

## 現在の本番接続の制約

仕様書・公開API・テーブル・列・設定キーは変更していない。
そのため、起動コマンドだけでは全工程を完了できない。
`PipelineDependencies`の次の境界は、仕様の確定と実装が必要になる。
起動コマンドは空の依存設定を渡すため、認証とパスの検証を通っても
工程3で停止する。再実行では工程2で停止する場合もある。

| 境界 | 不足している仕様・入力 |
| --- | --- |
| `Copies` | worktreeとコピー一覧の登録、`worktree_ready`を永続化するRunnerからの要求形式。既存の`WorktreeCopyRecorder`のHTTP実装が無い |
| `Audit`、Publisherの監査 | パス拒否とリモートSHA不一致をaudit_logsへ送る要求形式 |
| `Reuse` | 前回の既知SHA、保持状態、準備完了の取得と、復元開始・コピー再登録の送信形式 |
| `FreshNumber` | continueを含むExecutionのseqとは別の、タスク内のfresh通し番号の取得経路 |
| `PreviousPR` | タスクの過去のPRの確認経路。claimのpreviousは直前のExecutionだけで、freshではnullになる。open/freshのBlockに必要な前回HEADも不足する |
| `Policy` | CPU・メモリ、Sandbox内のシェルと環境変数、JUnit形式名の対応、モデルAPIとsetupの許可宛先、Harnessリポジトリのパス、Tool Permissionsの取得とCLI権限への変換規則 |
| `Publication` | pushを確認した直後、PR作成より前にpushed_shaを保存する要求形式。PR情報の保存形式も未実装。既存のresult APIはpublishing中のメタデータだけの保存に対応していない |
| 再開時の情報 | Blockから続行した場合のworktree準備・実際に使ったHarnessの版・本人が承認したBlockの内容の取得経路。現在の組み立てはresumeを安全側で停止する |
| 読み取り用認証 | 非公開Projectの検証・clone・fetchに使う認証の供給方法。Publisher用の書き込み権限を読み取り用認証の代わりにはしない |
| Harnessセッションの記録 | Adapterが発行したsession IDを既存のexecutions.harness_session_idへ送る要求形式 |

Harnessの版は通常実行のPR本文では、登録済みProfile定義の`harnessVersion`を使う。
実CLIの版を観測して一致を保証する取得経路は、上の表の未定義部分に含む。
不足した入力をProfileの他の値やExecutionのseqで推測して埋めない。

## 結合テストの範囲

`TestPipelineClaimThroughPushAndSucceeded`はpullループのheartbeat・claimから
工程実行、ローカルbareリポジトリへのpush、HTTPサーバーへのPR作成要求、
succeededと使用量の報告までを通す。Adapterには`adapter/dummy`を登録する。
テストだけのラッパーがworktreeへのcommitを作る。setup・テストの隔離境界は
テスト用Sandboxに置き換え、本番コードにはホスト実行経路を追加しない。
Gitの安全なラッパーがローカルファイルへの直接pushを拒否するため、
bareリポジトリへはループバックのGit daemon経由で接続する。

未定義のworktree・Publisher保存境界はテスト用の記録実装を渡す。
Control PlaneのHTTPはテストサーバーを使い、既存result形式と状態の順序を検査する。
このテストは、本番Control Planeへのworktree・PR保存や管理DBへの保存まで
実装できたことの証拠にはならない。使用量のDB保存は既存のusageテストの担当である。
外部GitHub、モデルAPI、実コンテナには依存しない。

setup失敗、Gate 2失敗、認証設定不足、未定義の境界、Projectの期限超過と
defaultTimeoutsの期限超過でも後続工程を止める。作成済みworktreeは保持する。
Runnerトークンと複数行の秘密鍵の内容がログ・eventsに残らないことも確認する。
起動コマンドのテストは、実際の`run`がclaimを工程表へ渡してconfig_errorを
報告することと、終了指示が重なっても確定済みのfailedを再報告しないことを確認する。

実コンテナと実CLIでの隔離・モデル実行の試験は、この結合テストでは実行しない。
