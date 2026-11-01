# Gate 2

`Runner.NewGate2Executor`は既存の工程表へ基準値取得、ハーネス終了後のHEAD取得、
Gate 2を組み込む。残りの工程ハンドラとExecutionControlを呼び出し側から渡す。
単独で組み込む場合は`Runner.Gate2Handler`を`StageGate2`へ渡す。
基準値取得とGate 2には同じSandbox設定とJUnitIDResolverを使う。

Gate 2はRegistryのコマンドをTest Sandboxで1回実行する。
開始前にTrusted RunnerがHEADを照合し、前回のレポートを除去する。
ネットワークを禁止し、Git管理領域や認証情報をSandboxへ渡さない。
レポートの読み取りは基準値取得と同じパス検証と通常ファイル検証を使う。
コマンド未設定では実行せずfailed(config_error)を報告する。

`gate.Verify`は終了コードと失敗数を先に確認し、既存IDの包含、実行数の下限、
skip数の上限を確認する。実行数はtotalからskippedを引いた件数である。
IDの重複も保持する。件数の増加は拒否しない。
IDを取得できないレポートでは件数を使い、findings.messageに「件数による確認」を残す。
保存済みのID一覧を読み戻せない場合は、同じ件数でもgate2_unverifiableで止める。

取得したIDは既存のartifacts APIでJSON配列として保存する。
`test_runs(kind=verify)`、Gate結果、SHA、Blockは既存のresult APIで報告する。
成功したGate 2の結果は既存のgateResultsと同じく次の状態遷移でまとめて保存する。
Gate 3には`Execution.VerifyRun`と`VerifiedSHA`を渡す。
失敗とBlockではその場で状態遷移を報告して後続工程を止める。
タイムアウトとキャンセルではSandboxの終了を待った後、実行器が測定記録と状態を報告する。

resultの`testRuns`は既存test_runsの測定値を運ぶための本文項目である。
APIはExecution、Registryコマンド、harness_head_sha、件数、成果物の所有者を確認する。
測定記録、Gate結果、SHA、Block、イベント、状態遷移を同じトランザクションに含める。
新しいエンドポイント、テーブル、列は追加しない。
遷移報告の応答消失後の409という既存契約は変更していない。報告の再送中にテストは再実行しない。

continueでは元のworktreeの基準値を使う。cleanedからの復元は準備工程が担当し、
基準値は再取得しない。resume_pointがgate3またはpublishなら実行器がGate 2を呼ばない。

## 仕様の穴と接続範囲

- JUnitのID規則は先行タスクから未確定であり、引き続きJUnitIDResolverで注入する。
- Findingのrule一覧は仕様10に無い。`Gate2Rules`で明示的に注入し、既定値を追加しない。
- 保存済みtest_idsをRunnerが読む通信経路は仕様に無い。既存GETは本人用なので、
  Runnerトークンの権限を拡張しない。新規取得時は保存済みIDと同じ一覧をExecution内で保持し、
  continueでは`Gate2Config.LoadTestIDs`で元の成果物を読む境界を設ける。
  Reader未接続や読み取り失敗時はgate2_unverifiableとして自動公開を止める。
  結合テストではControl Plane側の成果物を読むテスト用Readerを注入する。
- 起動コマンド`cmd/evaloom-runner`は先行実装の時点で全工程の接続待ちである。
  本変更は製品の工程実行器を構築する`NewGate2Executor`までを接続する。
  常駐プロセス全体の起動設定と残りの工程実装は含めない。
- test_runsはタイムアウトを保存できるが、gate_results.resultはpass、blocked、failedだけである。
  timed_outとcancelledではGate結果を創作せず、測定記録とExecution状態を保存する。

`gate2_controlplane_test.go`は隔離ランタイムと未実装の周辺工程を置き換え、
製品のコマンド準備、レポート読み取り、工程接続、HTTP報告、SQLiteと成果物保存を確認する。
continueではRunnerを作り直し、実際のlinked worktreeを削除・復元しても元のIDで照合する。
APIの拒否と保存失敗時に、測定記録や状態遷移が部分的に残らないことも確認する。
