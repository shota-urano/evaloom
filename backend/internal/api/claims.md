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
- 3.3.2の追加項目はclaimのトランザクションで読む。`freshNumber`は同じタスクの
  seq以下のfresh件数、`pullRequests`は現在より前のExecutionを番号ごとにまとめ、
  最新のseq・ブランチを新しい順に返す。ProfileにはHarness行のrepo_pathと
  tool_permissionsを載せる。
- 再開点がある場合だけ、最新のcontinue判断を`resumeBlock`、同じExecutionの
  Gate結果を作成順に`resumeGateResults`として返す。再開点がなければ、
  再開用SHA・Block・検証実行はnull、Gate結果は空配列にする。
- `worktree`のstatusと直前のExecutionの既知SHAを返す。準備完了は、最後の
  復元開始より後の一致する`worktree_ready`と、setup成功または同じ準備での
  setup不要記録を照合する。関連付け前の復元完了はresultの監査記録でも照合する。
- Q69・Q73・Q74は00-overview.mdのQ表で2026-11-07に採用済み。
  Q73の列・登録APIは03-registry.mdの別実装単位であり、現在のDBには列がない。
  この実装ではclaim専用のProject表現で`setupAllowedHosts: []`を返す。
- Q74の成果物IDはトランザクション内で読み、確定後に成果物ストアから
  `summary`を読む。無い・読めない・形式が違う場合はnullでclaimを返す。
  同じExecutionに複数の`harness_output`がある場合の選択規則は仕様にないため、
  任意の1件を選ばず概要を取得できない場合としてnullにする。
- 同じExecutionをBlockedから続行する場合は、保存済みのrole・Harness・Profileを使う。
  Profile定義は保存済みのProfile IDから読み、版とSHAはExecutionの列値を返す。
  再claimではRunnerとclaim時刻だけを更新する。初回かどうかはProfile IDが未保存かで判定し、
  新しいExecutionの再実行では現在の割り当てから版を固定する。
- 再開点がある場合は、同じExecutionの`harnessHeadSha`、`verifiedSha`、
  `verifyRun`を返す。新しいcontinue Executionには前回の検証を引き継がない。
  `worktree`には元のbaselineに加え、保存済みの`copiedFiles`と`branchName`を返す。
  Gate 3はこのスナップショットを使い、再開時も実際のGit状態を確認する。
- Q40に従い、claimのトランザクションを確定した後にControl Planeの成果物ストアを使い、
  基準値を記録したExecutionの`test_ids`を読む。`worktree.baseline.testIds`には
  JSONの文字列配列を順序・重複を保持して返す。成果物が無い・読めない・形式が違う・
  件数が基準値と合わない場合と、基準値が件数だけの場合はnullにする。
  成果物の取得に失敗してもclaimは返す。Runner用APIとトークンの権限は変えない。
- 作成後にDefault Roleの割り当てがなくなった場合も422
  `precondition_failed`、詳細は`projectId/default_harness_missing`とする。
  取得対象を別のExecutionに差し替えず、そのトランザクションを取り消す。
  この条件は初回claimに適用し、固定済みのExecutionの続行は現在の割り当てに依存しない。
- claim成功時は`status_changed`イベントにExecutionの`pending`から`claimed`への
  遷移と`reason=runner_claimed`を記録する。監査記録は
  `runner:<id>/execution.claim/execution/<Execution ID>`とし、Task ID、
  Project ID、遷移前後、Profile ID、commit SHAを残す。claimでは通知しない。
- ストアの接続設定に`_txlock=immediate`を指定する。既存の単一接続による
  直列化を維持し、`BeginTx`時点で書き込みを予約する。設定は接続の交換後も
  有効で、claim以外の既存トランザクションにも適用する。
  上限確認、候補選択、既存の遷移関数、版の保存、イベント、監査記録の
  すべてを同じトランザクションで行う。失敗時は全体をロールバックする。
