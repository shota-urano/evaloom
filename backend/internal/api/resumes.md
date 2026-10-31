# Blockedからの続行・中止API

正本は`docs/specs/04-task-lifecycle.md`の3.8・6節、
`10-verification-gates.md`の3.5節、`07-context.md`の3.7節、
`01-architecture.md`の5.1・5.3節、`02-data-model.md`の列定義。

`POST /api/tasks/{id}/resume`は本人用認証を通し、仕様の
`blockId`・`decision`・`headSha`だけを受け付ける。
既存のcancel・retryと同じく、成功時は本文なしの204を返す。
要求・応答本文への追加項目、テーブル・列の追加はない。

claim・cancel・retry・resultと同じ`BEGIN IMMEDIATE`で対象を読む。
Blockが指定したTaskに属すること、最新ExecutionとTaskがblockedであること、
Blockが未判断であること、確認SHAが記録済みSHAと一致することを確かめる。
continueの対象原因と再開工程は仕様04・10の表に従う。
Executionは同じ行のままpendingに戻すため、Profile・Context・worktreeの記録を保持する。
resume時点ではRunnerを起動せず、既存claimの同時実行制御で取得を待つ。
abortは既存cancelと同じ結果保存・遷移処理を使い、worktreeを保持する。
判断、状態遷移、イベント、監査ログの保存に失敗した場合は全体を取り消す。

仕様に具体的な定義がない箇所は、既存APIの慣例と共通エラー規約に合わせた。

- 成功時のHTTPステータスは仕様に明記がない。cancel・retryと同じ204とし、本文は返さない。
- 必須項目の欠け、形式不正、未知のフィールドは400。TaskまたはそのTaskに属するBlockが
  無い場合は404。判断済み、過去のExecution、blocked以外の状態は状態遷移規約に従い409。
  continueできない原因は前提条件の不成立として422。SHA不一致は仕様の422を使う。
- 監査ログの具体的なactionとdetail、イベントのreasonは未定義。
  既存の命名規則に合わせ、`owner/task.resume/task/<Task ID>`に
  `executionId`・`blockId`・`decision`・`headSha`・Taskの`from`・`to`を残す。
  `status_changed`イベントにExecutionの`from`・`to`と
  `reason=block_continue`または`block_abort`を残す。

continueで再開できる原因は`gate2_missing_tests`・`gate2_unverifiable`・
`gate3_mismatch`で、どれも`resume_point`を`gate3`にする（Gate 1は廃止済み）。
Gate 2で止まった場合の続行では、同じExecutionのverify測定からverified_shaを記録する。
測定が1件だけ存在し、そのSHAがharness_head_shaとBlockのhead_shaに一致し、
終了コードと失敗数が0、タイムアウトが無く、実行数が正であることを条件とする。
条件を満たさなくても続行判断は保存し、Gate 3の再確認で止める。
Gate 3からの続行ではverified_shaを作り直さない。
判断・SHA・状態・監査ログは同じトランザクションで保存する。
Runnerは再claimで取得した直近の判断済みBlockのhead_shaを再開時に照合し、
一致しなければgit_stateで再びBlockする。詳細は`../runner/gate3.md`を参照。
