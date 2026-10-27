# Runnerのclaim API

正本は`docs/specs/04-task-lifecycle.md`の3.3・6節、
`02-data-model.md`の3.2・5.2・5.3節、`03-registry.md`の3.6節、
`05-runner.md`の3.2節。`POST /api/runners/{id}/claim`は既存の
`protectRunner`で保護し、認証したRunnerのProjectだけを取得する。

仕様にない部分と応答案の実装判断は次のとおり。

- 仕事があれば200で3.3.1節の各項目を返し、仕事がなければ本文なしの204を返す。
  リクエスト本文は使用せず、取得対象はサーバー側で選ぶ。
- 作成順はExecutionの`created_at`昇順とし、同時刻は`id`昇順で決める。
- 初回と`fresh`では`previous`をnullにする。`continue`の2回目以降は
  直前のExecutionとそのworktreeから読み、未記録の各値はnullで返す。
- Projectは既存のRegistry表現で返す。Profile定義とSHAは登録済み行から読み、
  Gitタグを再解決しない。Contextの固定は既存の準備工程のAPIで行う。
- 作成後にDefault Roleの割り当てがなくなった場合も422
  `precondition_failed`、詳細は`projectId/default_harness_missing`とする。
  取得対象を別のExecutionに差し替えず、そのトランザクションを取り消す。
- claim成功時は`status_changed`イベントにExecutionの`pending`から`claimed`への
  遷移と`reason=runner_claimed`を記録する。監査記録は
  `runner:<id>/execution.claim/execution/<Execution ID>`とし、Task ID、
  Project ID、遷移前後、Profile ID、commit SHAを残す。claimでは通知しない。
- ストアの接続設定に`_txlock=immediate`を指定する。既存の単一接続による
  直列化を維持し、`BeginTx`時点で書き込みを予約する。設定は接続の交換後も
  有効で、claim以外の既存トランザクションにも適用する。
  上限確認、候補選択、既存の遷移関数、版の保存、イベント、監査記録の
  すべてを同じトランザクションで行う。失敗時は全体をロールバックする。
