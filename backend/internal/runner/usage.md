# 使用量の記録

仕様05の工程14は`NewUsageStage`で作り、工程実行器の`StageComplete`に渡す。
ハーネス工程はAdapterから受け取った結果を`Execution.HarnessResult`に設定する。
工程実行器は準備から工程14に到達するまでの時間と工程8の時間を計測する。
実際の時計には`time.Now`の単調時計を使い、ミリ秒で渡す。
トークン数はHarnessResultのnilと0を区別してそのまま渡す。費用は算出しない。

`UsageRecorder`が記録の成功を返した後に、既存のresult報告で`succeeded`にする。
記録エラーでは成功を報告せず、工程を自動で再実行しない。
工程14より前の失敗、Blocked、タイムアウト、中止では工程14を呼ばない。
それらの状態報告とプロセス停止は既存の実装に従う。

Control Plane側には既存テーブルの列だけを使う`store.RecordUsage`を用意した。
`*sql.Tx`も受け付けるため、経路が確定した後にresult保存と同じトランザクションに含められる。
Runnerから管理DBを直接開く実装は追加していない。

仕様上、次の点が未定である。

- 仕様01の5.2と仕様04の3.4には、usageを送る要求項目やAPIが無い。
  `UsageRecorder`の本番用実装は追加していない。受け入れテストではテスト専用の
  Recorderで既存のSQLiteテーブルに保存し、result報告には既存のHTTP経路を使う。
- 仕様05の3.7はExecution全体とハーネス工程の時間を要求するが、仕様02の
  `usage_records`には`wall_time_ms`しかなく、期間を区別する列や行の規約が無い。
  両方の計測値とAdapterのusageはインターフェースまで保持し、保存規約を創作しない。
  テスト専用RecorderではExecution全体の時間を1行に保存する。
- Blockedから同じExecutionを再開した場合、以前の計測値を引き継ぐ経路も無い。
  この場合は今回のExecuteの時間を渡し、スキップしたハーネス工程の時間とusageはnilにする。
  本番用Recorderでは再開前の記録との統合方法も確定させる必要がある。

公開API、テーブル、列、確定済みの仕様書は変更していない。
Runnerコマンドでの各工程の組み立ても既存のとおり未実装である。
