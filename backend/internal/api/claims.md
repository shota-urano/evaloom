# Runnerのclaim API

正本は`docs/specs/04-task-lifecycle.md`の3.3・3.3.1・6節、
`02-data-model.md`の3.3・5.2節、`05-runner.md`。`POST /api/runners/{id}/claim`は既存の
`protectRunner`で保護する。処理は`store.Claim`にある。

worktree、ブランチ、PR、テストコマンドなど開発の項目は応答に含めない（04 3.3.1）。

仕様にない部分の実装判断は次のとおり。

- 要求本文は`{"workspaceRoot":"<絶対パス>"}`だけを受け付ける。無い・相対パス・`filepath.Clean`で
  変わるパスは400 `validation_failed`（`workspaceRoot/required`・`workspaceRoot/invalid_format`）。
  知らない項目は`body/invalid_json`。
- 仕事が無いときは200で`{"job":null}`を返す（以前の204はやめた）。
- 提案とExecutionの作成順は`created_at`昇順、同時刻は`id`昇順。
- 提案は、全体設定`routing.defaultRunnerId`が呼び出し元と同じときだけ渡す。`proposer`は
  全体設定`routing.proposer`（無ければ02 3.8の既定値）、`timeoutSec`はQ201の300秒。
- 振り分け先ごとの枠は、`tasks.project_id`（Project）か`tasks.harness_id`（直接の振り分け）で数える。
  Projectのタスクは`tasks.harness_id`を持たないため、同じスペシャルハーネスを使う別のProjectとは別に数える。
- resume_stageのあるExecutionは、このタスク（9lz.3）では渡さない（再開のclaimは9lz.4）。
- 3.3の「config_error」に当たった候補は、同じトランザクションで`pending`→`failed`にし、
  `error_class=config_error`、`error_message`、`status_changed`（`reason=config_error`）を書いて次の候補を見る。
  通知の要求は遷移関数が作る。仕事が無くてもこの更新はコミットする。
- params.schemaの検査（`registry.ValidateProfileParams`）は、claimのトランザクションの中でGitを動かす。
  時間の上限はregistry側の定数に従う。
- mode=`continue`で引き継ぐ作業領域の行は、`created_at`の新しい順で選ぶ。行の状態（`retained`）は書き換えない。
- claim成功時は`status_changed`イベント（`pending`→`claimed`、`reason=runner_claimed`）と、監査記録
  `runner:<id>/execution.claim/execution/<Execution ID>`（Task ID、振り分け先、Profile ID、commit SHA、作業領域のID）を残す。
- ストアの接続設定の`_txlock=immediate`により、上限確認から応答の材料の読み出しまでを
  1つの書き込みトランザクションで行う。失敗時は全体をロールバックする。
