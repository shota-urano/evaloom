# タスク中止APIとheartbeat

正本は`docs/specs/04-task-lifecycle.md`の3.2・3.5・6節、
`02-data-model.md`のexecutions・audit_logs・5.2〜5.4節、
`05-runner.md`の3.6・5節、`12-notification.md`の3.1節。

`POST /api/tasks/{id}/cancel`は既存の本人用認証で保護する。
セッション・CSRF・Origin・Hostの検証を通った要求だけを処理する。
claim・resultと同じ`BEGIN IMMEDIATE`で最新Executionを読み、
状態と最新seqを条件にして更新する。
queued・blockedの即時中止には既存の遷移関数と結果メタデータ保存を使う。
終了時刻、通知対象外の処理も既存関数を共有する。

仕様に記載がない部分は、次のように判断した。

- 中止要求の入力項目はなく、成功は本文なしの204を返す。
  不正なTask IDは400、存在しないTaskは404、対象外の状態は409とする。
  Taskと最新Executionの状態が対応しない場合も409にし、書き込まない。
- 実行中の同じExecutionへの再送は204にし、最初の`cancel_requested_at`を保持する。
  監査記録は増やさない。即時中止後の再送は、仕様04の「上記以外」に従い409とする。
  即時中止では`cancel_requested_at`を設定せず、結果報告と同じ`error_class=cancelled`を保存する。
- 中止の監査記録は`owner/task.cancel/task/<Task ID>`とし、
  `executionId`、Taskの`from`・`to`、`cancelRequested`を保存する。
  即時中止は`status_changed`にExecutionの`from`・`to`と
  `reason=task_cancelled`を記録する。
  実行中の要求は状態を変えないため、監査記録だけを追加する。
  Runnerのcancelled報告時に既存のresult APIが状態変更イベントを追加する。
  仕様02にないイベント種別や、同じ状態への遷移イベントは追加しない。
- claimと中止は同じ書き込み予約で直列化する。中止が先ならpendingをcancelledにし、
  claimの候補から外す。claimが先なら、中止は読み取り時点のrunningに従って要求時刻を保存する。
  古い状態を前提とした更新は、既存の状態条件と最新seq条件で拒否する。

3.8の各経路は次のように実装した。

- queued（再開待ちを含む）とwaiting_approvalは、未使用（pending・approved）の承認を
  `invalidated`（`cancelled`）にし、開いている判断待ちを`withdrawn`（`cancelled`）にする。
- blocked（判断待ち）は判断待ちを`withdrawn`（`cancelled`）にする。開いている判断待ちが
  `blocked`/`publish_unknown`のときだけ、Executionとタスクを`failed`（`publish_unknown`）にする。
  監査記録とイベントの`to`は`failed`になる。
- routingは、route の判断待ちを`withdrawn`（`cancelled`）にし、`queued`の提案を`cancelled`にする。
  `running`の提案は枠を早く空けないよう`running`のまま残し、heartbeatの`cancelRouteProposalIds`で
  Runnerに止めさせる。止めた後の結果報告で提案を`cancelled`にし、結果は記録だけ残す。
- どの経路でも作業領域（task_workspaces）は変更しない。

heartbeatは既存のRunner認証と`store/runners.go`で処理する。

- 要求は仕様05の`{bootId, runningExecutionIds}`とする。
  bootIdは空白以外を含む文字列、runningExecutionIdsはULIDの配列を必須にする。
  空配列は受け付け、未知のフィールドは既存のJSON検証で拒否する。
  bootIdによる再起動回収は別タスクのため、今回の処理では保存・判定しない。
- 認証したRunnerとパスのIDは既存の認証処理で照合する。
  報告されたExecution IDはすべて、そのRunnerのclaimに属することを確認する。
  他のRunnerのID、未claim、存在しないIDは403とし、
  `auth.reject`に`execution_runner_mismatch`を記録する。
  Projectの現在のRunner割り当てを所有権の代用にはしない。
- 所有権の検証後、Runnerをonlineにし、Runnerと報告されたアクティブExecutionの
  `last_heartbeat_at`を同じUTC時刻で保存する。
  終端状態のIDは、resultとの競合を考慮して受け付けるが時刻を更新しない。
- 任意の`runningRouteProposalIds`（ULIDの配列）を受け付け、そのRunnerのclaimに属さない提案は403にする。
  応答の`cancelRouteProposalIds`には、そのRunnerの`running`の提案のうちタスクが`cancelled`のものを
  ID順で入れ、結果報告まで繰り返し返す。空の場合も配列にする。
- 応答は200で`{cancelExecutionIds: [...]}`を返す。空の場合も配列にする。
  対象は認証したRunnerが持つアクティブExecutionのうち、中止要求時刻があるものとする。
  claim直後に報告一覧へ反映されていない場合も渡せるよう、要求配列には限定しない。
  ID順で返し、要求時刻は消費・削除せず、Runnerの終端報告まで繰り返し返す。
  blocked・pending・終端状態への中止指示は返さない。
- 状態・要求時刻・イベント・監査ログ、heartbeat時刻は、それぞれの処理の
  同じトランザクションに含める。保存失敗時は全体をロールバックする。
