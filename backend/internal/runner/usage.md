# 使用量の記録

工程実行器は準備から工程14の完了までの時間と、工程8の時間を計測する。
ハーネス工程はAdapterから受け取った結果を`Execution.HarnessResult`に設定する。
実際の時計には`time.Now`の単調時計を使い、ミリ秒で渡す。

`NewExecutorWithControl`は工程14が成功した後、`succeeded`のresult報告に
使用量を必ず付ける。別のRecorderを注入する必要はない。
Runnerは既存の`POST /api/executions/{id}/result`でControl Planeに送り、
管理DBを直接開かない。

Control Planeは報告したRunnerとclaimしたRunnerの一致と状態遷移を検証し、
`store.RecordUsage`でExecution全体の時間を1行保存する。
トークン数はHarnessResultのnilと0を区別して保存する。
取れないトークン数と未指定の費用はSQL NULLにし、費用は算出しない。
使用量、状態、通知要求、イベント、監査記録は同じトランザクションで保存する。
保存が失敗した場合は全体をロールバックし、成功を応答しない。
同時送信や再送の扱いは既存resultと同じで、成功後の重複報告は409になる。

Runnerは通信失敗や5xxで同じ本文を再送する。
再送の間に工程を再実行せず、使用量も計測し直さない。
工程14より前の失敗、Blocked、タイムアウト、中止では使用量を送らない。
それらの状態報告とプロセス停止は既存の実装に従う。
受け入れテストはRunnerの工程実行器から認証付きHTTP、Control Planeのresult API、
実際のSQLiteへの保存までを通し、実行時間とトークン数列のSQL NULLを検証する。
テスト専用Recorderによる使用量の直接INSERTは行わない。

## 仕様にない要求本文と未定の保存規約

仕様01の5.2、仕様04の3.4、仕様05の3.3にはresultの本文の形が書かれていない。
既存のresult本文に、次の`usage`オブジェクトを追加した。

```json
{
  "fromStatus": "publishing",
  "status": "succeeded",
  "usage": {
    "wallTimeMs": 1300,
    "harnessWallTimeMs": 100,
    "harnessUsage": {
      "inputTokens": null,
      "outputTokens": null,
      "wallTimeMs": 71
    }
  }
}
```

- `usage.wallTimeMs`は今回のExecute全体の実測値で、`succeeded`では必須とする。
  省略やnullでは成功を保存せず、400を返す。0は計測値として受け付ける。
- `harnessWallTimeMs`はRunnerが計測したハーネス工程の時間、
  `harnessUsage`はAdapterから取得したusageである。未取得の場合は省略する。
- 時間とトークン数は非負の整数だけを受け付ける。
  `succeeded`以外の報告に`usage`があれば400を返す。
- 仕様05の3.7はExecution全体とハーネス工程の時間を要求するが、仕様02の
  `usage_records`には`wall_time_ms`しかなく、期間を区別する列や行の規約がない。
  Execution全体を既存の1列に保存し、ハーネス工程の時間とAdapter自身の時間は
  要求の入力境界まで保持する。それらを区別する保存規約は追加していない。
- Blockedから同じExecutionを再開した場合、以前の計測値を引き継ぐ経路もない。
  今回のExecuteの時間を保存し、スキップしたハーネス工程の時間とusageは省略する。
  再開前の計測値との統合方法は未定である。

エンドポイント、テーブル、列、確定済みの仕様書は変更していない。
Runnerコマンドでの各工程の組み立ては既存のとおり未実装である。
