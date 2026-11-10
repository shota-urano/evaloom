---
status: confirmed
confirmed_rev: 0ad23d9
written: 2026-11-10
範囲: 手動タスク作成、タスクと Execution の状態遷移、claim と同時実行制御、段の報告、本人の判断の後の再開、中止、再起動時の回収、再実行
---

# 04. タスクライフサイクル 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Control Plane 側のタスク・Execution・振り分けの提案の管理

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。状態の値と遷移は [`02-data-model.md`](./02-data-model.md) 5.1・5.2・5.6、API は [`01-architecture.md`](./01-architecture.md)、要求 JSON と結果 JSON の形はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）に従う。

claim の応答と工程内報告にあった開発の項目（worktree、テストの基準値、PR の一覧、前回の PR 番号、push した SHA、`worktree.path_rejected`・`publish.remote_mismatch` の監査の付記）は、software-engineer へ移し、この文書からは消した（本人決定 1）。Evaloom は、それらを要求 JSON の材料（`params`、`previous.outputs`、`previous.checkedContentId`）としてだけ受け渡す。

## 1. 目的

- 本人が管理画面からタスクを作り、振り分け先（Project またはスペシャルハーネス）で実行できるようにする（requirements 8.1）。
- タスクと Execution の状態を永続化し、再起動の後も未完了の仕事と本人の判断待ちを見つけられるようにする（requirements 8.3、条件10）。
- 同時実行の上限（全体2本、振り分け先ごとに1本）を守る（requirements 8.7、条件13）。
- 本人の判断（承認、続行、質問への回答）の後に、同じ Execution を決まった段から再開できるようにする（本人決定 3、契約 8章・10.3）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | 本人用 API（タスクの作成・中止・再実行、判断待ちへの回答）、Runner 用 API（claim・heartbeat・result・route-proposals の result） |
| 出力 | tasks、executions、task_workspaces、stage_runs、check_results、owner_decisions、approvals、route_proposals、execution_events、audit_logs の更新、通知の要求（[`12-notification.md`](./12-notification.md)）、linear_outbox（[`16-linear-feedback.md`](./16-linear-feedback.md)） |

## 3. 処理詳細

### 3.1 手動タスク作成（`POST /api/tasks`）

入力は次のとおりとする。

| 項目 | 必須 | 検証 |
|---|---|---|
| `projectId` / `harnessId` | どちらか一方が必須 | 両方・どちらも無いと 400。振り分け先が登録済みでなければ 422 |
| `title` | 必須 | 空でない |
| `body` | 必須 | 空でない（Issue の本文に当たる） |
| `acceptanceCriteria` | 振り分け先の Profile が `acceptanceCriteria: required` なら必須 | `required` のとき、空白だけの文字列は 400（requirements 8.1、9.5）。`optional` のときは空文字列を許す |
| `Idempotency-Key` ヘッダ | 必須 | 管理画面が送信ごとに作る。同じキーの再送には最初に作ったタスクを返す |

振り分け先の Profile は、Project なら projects.profile_id、スペシャルハーネスなら harnesses.default_profile_id とする。スペシャルハーネスへ直接振り分けたのに default_profile_id が無ければ、タスクを作らず 422 にする。

処理は次のとおりとし、1つのトランザクションで行う。

1. tasks に行を作る（source=`manual`、振り分け先、collaboration_mode=`solo`、idempotency_key は `Idempotency-Key`）。
2. executions に seq=1、mode=`continue`、status=`pending` の行を作り、タスクを `queued` にする。
3. audit_logs（`task.create`）と execution_events（`status_changed`）に記録する。

手動のタスクは振り分けの提案（`routing`）を通らない。本人が振り分け先を選んで作るためである。受け入れ条件の無い入力は API で止める。保留（02 の 5.7）は Linear から来たタスクだけの扱いである（[`15-linear-intake.md`](./15-linear-intake.md)）。

### 3.2 タスク状態の遷移

状態の値は [`02-data-model.md`](./02-data-model.md) 5.1。タスクの状態は、振り分けの段階では tasks だけで決め、Execution ができた後は最新の Execution の状態から 02 の 5.2 の対応で決める。両方の更新は同じトランザクションで行う。

| 現在 | 契機 | 次 | 詳細 |
|---|---|---|---|
| `received` | 振り分け先が決まり、Execution を作った | `queued` | 3.1、15 |
| `received` | 振り分けのラベルが無い・知らない・2つ以上 | `routing` | 15、3.7 |
| `received` | 取り込みの保留（受け入れ条件が無い、既定の版が無い） | `blocked`（intake_block_cause、Execution なし） | 15、02 の 5.7 |
| `blocked`（取り込みの保留） | 保留の再判定で解除 | `queued` / `routing` | 15 の 3.6.3 |
| `routing` | 本人が振り分けを承認した、または本人が Linear で振り分けのラベルを1つ付けた | `queued` | 3.7、15 |
| `routing` | 本人が振り分けの提案を却下した | `cancelled` | 02 の Q117 |
| `queued` | Runner が claim | `running` | 3.3 |
| `running` | 段の報告による遷移 | 02 の 5.2 の対応のとおり（`waiting_approval` / `blocked` / `completed` / `failed` / `timed_out` / `cancelled`） | 3.4 |
| `waiting_approval` / `blocked` | 本人の判断の後の再開 | `queued`（同じ Execution が `pending`） | 3.6 |
| `waiting_approval` / `blocked` | 本人の判断で終える | `failed` / `cancelled` | 3.6 |
| `received` / `routing` / `queued` / `running` / `waiting_approval` / `blocked` | 本人が中止 | `cancelled`（`running` は Runner が止めた後。publish の結果がわからない判断待ちだけは `failed`） | 3.8 |
| `failed` / `timed_out` / `cancelled` / `completed` | 本人が再実行 | `queued`（新しい Execution） | 3.10 |

Execution 状態の遷移は 02 の 5.2 の表を正本とし、この文書は遷移の契機となる報告と判断を定める（3.4、3.6）。表に無い遷移を要求されたら 409 `conflict` を返す（例 `completed` のタスクの中止）。

### 3.3 claim と同時実行制御（`POST /api/runners/{id}/claim`）

Runner は空きがあるときに claim を呼ぶ。要求の本文には、Runner の作業領域のルートの絶対パス（`workspaceRoot`）を入れる（Q134）。Control Plane は1つの書き込みトランザクション（`BEGIN IMMEDIATE`）の中で次を行う。

1. 動いている仕事を数える。Execution の status が `claimed` / `preparing` / `working` / `checking` / `publishing` のものと、route_proposals の status が `running` のものの合計が2以上なら、仕事なし（`job: null`）を返す（02 の Q111）。
2. 振り分けの提案を先に見る（Q132）。route_proposals の status が `queued` で、全体設定 `routing.defaultRunnerId` が呼び出し元の Runner と同じものがあれば、作成順の最初の1件を `running` にし、runner_id を書いて返す。
3. 提案が無ければ、Execution の status が `pending` のものを作成順に見て、次をすべて満たす最初の1件を選ぶ。
   - 振り分け先の Runner が呼び出し元と同じ。振り分け先が Project なら projects.runner_id、スペシャルハーネスなら `routing.defaultRunnerId`（02 の Q113）
   - 同じ振り分け先（同じ Project、または同じスペシャルハーネスへの直接の振り分け）に、動いている Execution が無い。同じスペシャルハーネスを使う別々の Project は、別の振り分け先として数える
4. 選んだ Execution が最初から始めるもの（resume_stage が NULL）なら、版と値を固定する（requirements 8.3）。
   - route_kind、project_id、harness_id を書く。
   - Profile は Project なら projects.profile_id、スペシャルハーネスなら harnesses.default_profile_id とし、profile_id、profile_version、profile_commit_sha、contract_version を書く。
   - 渡す値は Project なら projects.params、スペシャルハーネスなら `{}` とし、params に書く。Profile が `params.schema` を持つなら、ここで形を確かめる（Q115）。
   - 作業領域を決める。mode=`continue` なら、同じタスクの最新の `active` / `retained` の task_workspaces の行を引き継ぐ。無ければ、または mode=`fresh` なら、新しい行を作る（path は `<workspaceRoot>/<task_workspaces.id>`、status=`active`）。workspace_id を書く。
5. 選んだ Execution が再開するもの（resume_stage がある）なら、版、渡す値、作業領域は固定済みの値を使い、書き換えない。task_workspaces の行が `cleaned` なら、Execution を `blocked`（cause=`workspace_missing`）にして、次の候補を見る。
6. status を `claimed`、runner_id と claimed_at を書き、タスクを `running` にする。
7. 3.3.1 の応答を返す。

4 で次のどれかに当たったら、その Execution を `failed`（`config_error`）にし、通知を要求して、次の候補を見る。

- スペシャルハーネスの default_profile_id が無い
- 渡す値が Profile のスキーマに合わない
- Profile が `acceptanceCriteria: required` で、タスクの受け入れ条件が空（Project の版がタスク作成の後に変わった場合）
- Profile の `timeoutSec` が、いまの全体設定 `limits.stageTimeoutMaxSec` を超える（扱いは Q135）

`waiting_approval` と `blocked` の Execution は同時実行の枠に数えない。段のプロセスは終わっているためである（02 の 5.2、契約 7.1）。作業領域は残す。

#### 3.3.1 claim の応答

応答は `job` の `kind` で分ける。

```json
{ "job": null }
```

`kind` が `execution` のときは、Runner が要求 JSON（契約 6.1）を組み立てる材料を返す。開発の項目（worktree、ブランチ、PR、テストコマンド）は無い。仕事の種類に固有の値は `params` と `previous.outputs` に入っている。

```json
{
  "job": {
    "kind": "execution",
    "execution": { "id": "01J...", "taskId": "task-001", "seq": 2, "mode": "continue", "resumeStage": null },
    "task": {
      "id": "task-001", "title": "...", "body": "...", "acceptanceCriteria": "...",
      "source": { "kind": "linear", "identifier": "ENG-123", "url": "https://linear.app/..." }
    },
    "route": { "kind": "project", "projectId": "customer-system", "projectName": "顧客システム" },
    "params": {},
    "profile": {
      "id": "01J...", "harnessId": "software-engineer", "version": "1.2.0", "commitSha": "<40桁>",
      "tagName": "profile/v1.2.0", "repoPath": "/Users/.../software-engineer", "definition": {}
    },
    "workspace": { "id": "01J...", "path": "/.../workspaces/01J...", "created": true },
    "limits": { "timeoutSec": { "prepare": 600, "work": 3600, "check": 900, "publish": 600 } },
    "previous": { "executionId": "01J...", "checkedContentId": "<識別子>", "outputs": [] },
    "resume": null
  }
}
```

| 項目 | 内容 |
|---|---|
| `execution`、`task`、`route`、`params` | 契約 6.1 の同じ名前の項目にそのまま入る値 |
| `profile` | 固定した版。`definition` は harness_profiles.definition。Runner は `repoPath` から `commitSha` の中身を読み出す（[`05-runner.md`](./05-runner.md) 3.4） |
| `workspace` | 使う作業領域。`created` は、この claim で新しく作った行なら `true` |
| `limits.timeoutSec` | 段ごとの上限時間。Profile の `timeoutSec` を使う（Q135） |
| `previous` | 同じタスクで seq が1つ前の Execution の executions.id、content_id、outputs。前が無ければ `null`。契約 6.1 の `previous` に入る |
| `resume` | resume_stage があるときだけ、下の形。無ければ `null` |

`resume` の形は次のとおりとする。

```json
{
  "stage": "publish",
  "checkedContentId": "<識別子>",
  "approvals": [ { "id": "01J...", "operation": "merge_pull_request", "contentId": "<識別子>", "status": "approved" } ],
  "answer": null,
  "workSessionId": null,
  "completedStages": ["prepare", "work", "check"]
}
```

| 項目 | 内容 |
|---|---|
| `stage` | 再開する段（`work` / `publish`） |
| `checkedContentId` | stage=`publish` のとき、この Execution の executions.content_id。契約 6.1 の `checkedContentId` に入る |
| `approvals` | stage=`publish` のとき、この Execution の approvals のうち `approved` のものと、省いた操作（`status: "waived"`、`id: null`）。契約 6.1 の `approvals` に入る |
| `answer` | stage=`work` のとき、本人の回答 `{ "decisionId", "text", "option" }`。契約 6.1 の `answer` に入る |
| `workSessionId` | stage=`work` のとき、executions.work_session_id。同じセッションの再開に使う（契約 8章、Q124） |
| `completedStages` | この Execution で成功した段。Runner は、その段の `out` を後の段の `inputs/<段>/` に置く（[`05-runner.md`](./05-runner.md) 3.3） |

`kind` が `route_proposal` のときは、提案に要るものを返す。中身の形は [`15-linear-intake.md`](./15-linear-intake.md) で定める。

```json
{
  "job": {
    "kind": "route_proposal",
    "proposal": {
      "id": "01J...", "taskId": "task-007", "trigger": "no_label",
      "task": { "title": "...", "body": "..." },
      "candidates": [ { "label": "route:customer-system", "kind": "project", "id": "customer-system", "description": "..." } ],
      "proposer": { "microHarness": "claude_code", "model": "<Model ID>" },
      "timeoutSec": 300
    }
  }
}
```

### 3.4 Runner からの報告

- `POST /api/executions/{id}/events`: `log` イベントを execution_events に足す（[`05-runner.md`](./05-runner.md) 3.8）。
- `POST /api/executions/{id}/artifacts`: 成果物を登録する（05 の 3.8）。
- `POST /api/executions/{id}/result`: 段の開始と終了を報告する（3.4.1）。Control Plane は 02 の 5.2 の表に従って遷移を確かめ、合わない報告は 409 で拒否する。
- 終端の状態（`succeeded` / `failed` / `interrupted` / `timed_out`）に入ったら、通知を要求する（[`12-notification.md`](./12-notification.md) 3.1。`cancelled` は通知しない）。`waiting_approval` と `blocked` は、Execution の状態ではなく owner_decisions の行を作ったときに、行ごとに判断待ちの通知を要求する（12 の 3.1・3.4、3.5）。Linear 起因のタスクには linear_outbox に書く（[`16-linear-feedback.md`](./16-linear-feedback.md)）。

#### 3.4.1 段の開始と終了（result の形）

result で送ってよいのは、段の開始と終了の2種類だけとする。段の途中で状態を変えずに記録だけを送る報告（旧来の worktree の作成・作り直し、push の直後、PR の作成の報告）は無い。段の中で何をしたかは、段の結果 JSON（契約 6.5）と成果物とログで伝わる。

**段の開始**

```json
{ "type": "stage_started", "stage": "check", "requestArtifactId": "01J..." }
```

- Control Plane は stage_runs に行を作る（status=`running`）。attempt は、同じ Execution・同じ段の既存の行の最大値 + 1 とし、応答で返す（`{ "attempt": 1 }`）。
- Execution の状態を段に合わせて変える（`claimed` / 前の段の状態 → `preparing` / `working` / `checking` / `publishing`）。始めてよい段は、02 の 5.2 の表で次に来る段だけとする。Profile に prepare の宣言が無いのに prepare を始める報告、resume_stage と違う段から始める報告は 409。
- `stage_started` イベントを記録する。
- `requestArtifactId` は、Runner が先に登録した要求 JSON の成果物（kind=`request_json`）。鍵の値は含まない。

**段の終了**

```json
{
  "type": "stage_finished",
  "stage": "check",
  "attempt": 1,
  "exitCode": 0,
  "runnerError": null,
  "result": { "contractVersion": "1", "stage": "check", "status": "succeeded", "contentId": "<識別子>" },
  "resultArtifactId": "01J...",
  "work": null,
  "usage": { "wallTimeMs": 41000, "inputTokens": null, "outputTokens": null, "costAmount": null }
}
```

| 項目 | 内容 |
|---|---|
| `runnerError` | Runner が自分で決めた失敗。`config_error`（鍵が無い、Profile の中身を読めない、タグの付け替え、作業領域のパスがルートの外）/ `sandbox_error` / `timeout` / `cancelled` / `contract_error`（結果のファイルが無い・JSON として読めない）/ `work_failed`（ミクロハーネスの異常終了）/ `workspace_missing`（引き継ぐ作業領域、または再開に要る前の段の `out` が無い。エラー分類ではなく、下の表で扱いを決める）。無ければ `null` |
| `result` | prepare・check・publish の段の結果 JSON（契約 6.5）を、機密情報の除外を通したうえでそのまま入れる。`runnerError` があるときと work の段では `null` |
| `work` | work の段だけ。`{ "sessionId", "summaryArtifactId", "question" }`。`question` は `out/question.json` の中身（契約 8章）か `null` |
| `usage` | Runner が測った実行時間と、取れたときの使用量 |

Control Plane は1つのトランザクションで次を行う。

1. `result` を契約で確かめる。知らない項目、段で書けない `status`、スペシャルハーネスが書けない `errorClass`、宣言していない操作の `approvalRequests`、必須の `contentId` の欠け（契約 6.5）があれば `contract_error` として扱う。
2. stage_runs の行を終える（status、exit_code、error_class、result_artifact_id）。usage_records に行を書く。work の段では executions.work_session_id を書く。
3. 段と結果で次を決める（02 の 5.2）。

| 段 | 結果 | Control Plane が行うこと | 応答の `next` |
|---|---|---|---|
| どれでも | `runnerError` が `workspace_missing` | 再開の Execution なら `blocked`（cause=`workspace_missing`）、最初から始める Execution なら `failed`（`config_error`）にする | `stop` |
| どれでも | ほかの `runnerError` がある、または `status: failed` | Execution を `failed`（timeout は `timed_out`、cancelled は `cancelled`）にし、error_class を書く | `stop` |
| prepare | `succeeded` | `reportedContext` を context_revisions（layer=`reported`）に記録する（[`07-context.md`](./07-context.md)） | `work` |
| work | `question` がある | stage_runs を `question` にし、Execution を `blocked`（owner_decisions kind=`question`）にする | `stop` |
| work | 終了コードが成功で `question` が無い | `work_summary` の成果物を Execution の概要として表示に使う | `check` |
| check | `succeeded`（承認要求なし）、または `succeeded` / `needs_approval` で承認要求をすべて省いた | content_id を書き、check_results（`pass`）を作る。省いた操作には `approval_waived` を残す | `publish` |
| check / publish | `needs_approval`、または `succeeded` で省かない承認要求がある | content_id を書き（check のとき）、approvals と owner_decisions（kind=`approval`）を操作ごとに作り、Execution を `waiting_approval` にする（[`17-approval.md`](./17-approval.md)） | `stop` |
| check | `blocked` | content_id を書き、check_results（`blocked`）を作り、Execution を `blocked`（cause=`check_blocked`、basis=content_id）にする | `stop` |
| check | `failed` | check_results（`fail`）を作り、Execution を `failed`（`check_failed`）にする | `stop` |
| publish | `succeeded` | outputs を書き、`published` イベントを記録し、`records.keep` のファイルを harness_records に登録し（契約 9章）、Execution を `succeeded` にする | `stop` |
| publish | `unknown` | Execution を `blocked`（cause=`publish_unknown`）にする | `stop` |
| publish | `needs_approval` で承認要求をすべて省いた | 省いた操作に `approval_waived` を残し、Execution を `pending`（resume_stage=`publish`）、タスクを `queued` にする。publish の段を新しく始める（[`17-approval.md`](./17-approval.md) 3.3 の8） | `stop` |

4. 応答で `next` を返す。`next` が `publish` のときは、`resume` と同じ形の `checkedContentId` と `approvals`（省いた操作だけ）を添える。Runner は `stop` を受けたら、この Execution の段を終える。

同じ内容の報告の再送は、保存済みの結果と同じ応答を返し、行もイベントも増やさない。同じ (stage, attempt) に違う内容を送ったら 409 にする。

### 3.5 本人の判断待ち

段の結果で本人の判断が要るときは、owner_decisions に行を作る（02 の 3.4・5.6）。本人の判断の場所は管理画面だけである（本人決定 3）。行を作ったら、判断待ちの通知（[`12-notification.md`](./12-notification.md) 3.1）を要求し、Linear 起因のタスクには「管理画面で判断待ち」のコメントを linear_outbox に書く（kind=`decision_open`）。

| kind / cause | 作る契機 | 判断待ちの間の Execution |
|---|---|---|
| `approval` | check・publish の段の承認要求 | `waiting_approval` |
| `blocked` / `check_blocked` | check の段の `blocked` | `blocked` |
| `blocked` / `publish_unknown` | publish の段の `unknown` | `blocked` |
| `blocked` / `context_conflict` | 固定した文脈に解決できない矛盾（[`07-context.md`](./07-context.md)） | `blocked` |
| `blocked` / `workspace_missing` | 再開のときに作業領域が無い、または再開に要る前の段の `out` が Runner に無い（3.3 の5、3.4.1、[`05-runner.md`](./05-runner.md) 3.5、Q136） | `blocked` |
| `question` | 作業の段の `out/question.json` | `blocked` |
| `route` | 振り分けの提案（3.7） | Execution なし。タスクは `routing` |

### 3.6 本人の判断の後の再開（`POST /api/decisions/{id}/answer`）

止まった仕事の続行と、作業中の AI の質問への回答は、どちらもこのエンドポイントで受け付ける。旧来の `POST /api/tasks/{id}/resume` は使わない（[`01-architecture.md`](./01-architecture.md) 5.1）。回答の形は 02 の 5.6 を正本とする。

1. owner_decisions の行が `open` であることを確かめる。`answered` / `withdrawn` なら 409。
2. 回答の `action` が、その kind と cause で許されたものかを確かめる（02 の 5.6）。許されなければ 400。
3. basis のある判断（kind=`approval`、cause=`check_blocked`、kind=`route`）では、回答の `contentId`（route では `basis`）が行の basis と一致することを確かめる。一致しなければ 422。本人が画面で見ていた中身と、判断の対象が同じであることを保証するためである。
4. 行を `answered` にし、answer と answered_at を書き、audit_logs（`decision.answer`、actor=`owner`）と `decision_answered` イベントを残す。
5. 次の表のとおり処理する。どれも同じトランザクションで行う。

| kind / cause | action | 処理 |
|---|---|---|
| `approval` | `approve` | 承認を `approved` にする（17）。同じ Execution の承認待ちがすべて `approved` になったら、Execution を `pending`（resume_stage=`publish`）、タスクを `queued` にする |
| `approval` | `reject` | 承認を `rejected` にする。同じ Execution のほかの未使用の承認を `invalidated`（`cancelled`）にし、その判断待ちを `withdrawn`（`cancelled`）にする（[`17-approval.md`](./17-approval.md) 3.6）。Execution とタスクを `failed`（`approval_rejected`）にする |
| `blocked` / `check_blocked` | `continue` | 回答の `contentId` が行の basis（check が報告した content_id）と一致することを 3 で確かめた後、check の結果に省かない承認要求があれば、承認待ちを作って `waiting_approval` にする（[`17-approval.md`](./17-approval.md) 3.3）。無ければ `pending`（resume_stage=`publish`） |
| `blocked` / `publish_unknown` | `continue` | `pending`（resume_stage=`publish`）。出す段は始めに外の状態を確かめる（契約 7.5） |
| `blocked` / `publish_unknown` | `abort` | `failed`（`publish_unknown`） |
| `blocked` / `check_blocked`、`context_conflict`、`workspace_missing` | `abort` | `cancelled` |
| `question` | `answer` | `pending`（resume_stage=`work`）。回答は claim の `resume.answer` で渡す |
| `question` | `abort` | `cancelled` |
| `route` | `approve` / `reject` | 3.7 |

- Execution を `pending` にしたときは、同じトランザクションでタスクを `queued` にする（02 の 5.2）。Execution とタスクを終える（`failed` / `cancelled`）ときも、タスクを同じ値にする。
- 再開は再実行ではなく、同じ Execution の続きである（契約 10.3）。新しい Execution は作らない。版、渡す値、作業領域は claim のときに固定したものを使う。
- 再開した Execution は通常の claim と同じく同時実行の制御を受ける。
- 作業の段を再開するときに同じセッションを再開できない場合の扱いは、契約 8章の4（`stages.work.resume`、Q124）に従う。
- 判断が `cancelled`、`failed` で終わったときも、作業領域は削除しない。

### 3.7 振り分けの提案と判断

振り分けのラベルで決まらない Linear 起因のタスクは `routing` にし、route_proposals に `queued` の行を作る（作り方と提案の中身は [`15-linear-intake.md`](./15-linear-intake.md)）。この文書は状態と同時実行だけを定める。

- 提案は段ではなく、Execution を持たない（02 の 4.1）。Runner は claim で受け取り（3.3 の2）、結果を `POST /api/route-proposals/{id}/result` で返す。
- 提案は全体2本の枠に数え、振り分け先ごとの枠には数えない（02 の Q111）。
- 結果を受けたら、提案を `proposed` か `failed` にし、どちらでも owner_decisions（kind=`route`）を作る。
- 本人が `approve` したら、タスクに振り分け先を書き、seq=1・mode=`continue`・status=`pending` の Execution を作ってタスクを `queued` にする。提案を `decided` にし、linear_outbox に振り分けのラベルの追加（action=`add_label`、kind=`route_approved`）を書く。
- 本人が `reject` したら、提案を `decided` にし、タスクを `cancelled` にする（02 の Q117）。
- 判断待ちの間に本人が Linear で振り分けのラベルを1つだけ付けて振り分け先が決まったら、普通の振り分けとして扱う。提案を `superseded`、判断待ちを `withdrawn`（`superseded`）にする（本人決定 3）。
- 提案の途中でタスクを中止したら、`queued` の提案は `cancelled` にし、`running` の提案は heartbeat の応答の `cancelRouteProposalIds` で Runner に止めさせて `cancelled` にする。Runner の再起動で中断した提案は `failed`（`interrupted`）にし、タスクが `routing` のままなら提案なしの判断待ち（kind=`route`）を作る（02 の route_proposals、Q133）。

### 3.8 中止（`POST /api/tasks/{id}/cancel`）

| タスクの状態 | 処理 |
|---|---|
| `received` / `blocked`（取り込みの保留） | タスクを `cancelled` にする |
| `routing` | 判断待ちを `withdrawn`（`cancelled`）にし、タスクを `cancelled` にする。提案が `running` なら Runner に heartbeat で止めさせる（Q133） |
| `queued` | Execution を `cancelled` にし、タスクを `cancelled` にする。再開待ちの Execution で `approved` のまま使っていない承認は `invalidated`（`cancelled`）にする |
| `running` | Execution の cancel_requested_at を書く。Runner が次の heartbeat で受け取り、段を止めて `cancelled` を報告する（[`05-runner.md`](./05-runner.md) 3.10）。出す段の直前の問い合わせにも `cancelled` を返す（契約 7.4） |
| `waiting_approval` | 承認を `invalidated`（`cancelled`）、判断待ちを `withdrawn`（`cancelled`）にし、Execution とタスクを `cancelled` にする |
| `blocked`（判断待ち） | 判断待ちを `withdrawn`（`cancelled`）にし、Execution とタスクを `cancelled` にする。ただし cause=`publish_unknown` のときは `failed`（`publish_unknown`）にする（02 の 5.2） |
| 上記以外 | 409 |

中止しても作業領域は削除しない（契約 10.1）。中止は audit_logs（`task.cancel`）に残す。

Linear 起因の中止（`ai-ready` の取り外し、Issue のキャンセル・削除・移動、[`15-linear-intake.md`](./15-linear-intake.md) 3.7）も、`received` と `routing` を含めて上の表と同じ手順で行う。audit_logs の actor は `system`、理由は 15 の 3.4 の値とする。終端のタスクには何もせず、409 も返さない。

### 3.9 再起動時の回収（requirements 8.3、条件10）

Runner は起動のたびに新しい起動 ID（bootId）を作り、最初の heartbeat で送る。

1. Control Plane は、heartbeat の bootId が前回と違う Runner について、その Runner が持つ動いている Execution（`claimed` / `preparing` / `working` / `checking` / `publishing`）をすべて `interrupted` にする。動いている振り分けの提案（status=`running`）は `failed`（`interrupted`）にし、3.7 のとおり判断待ちを作る（Q133）。
2. Control Plane 自身の起動時には、Runner の heartbeat が途絶えたまま一定時間を過ぎた Runner の動いている Execution を `interrupted` にする。Runner が生きていて heartbeat を再開した場合は、その Execution を続ける。
3. `interrupted` にした Execution の error_class は `interrupted`、タスクは `failed` にする。動いていた stage_runs の行は `failed`（`interrupted`）にする。
4. 作業領域は削除せず `retained` にする。
5. 失敗の通知を要求する。
6. 自動では再実行も再開もしない（自動リトライは0回、契約 10.3）。publish の段で中断した場合も同じで、本人が再実行を選ぶ。

`waiting_approval` と `blocked` の Execution は回収の対象にしない。段のプロセスが動いていないためである。

実装時の確定事項（既存の決定を保つ）は次のとおりとする。

- heartbeat 途絶のしきい値と起動後の猶予は30秒。ローカルの再接続やスケジューリングの遅延を許容しつつ、未完了の Execution を長時間残さないためとする。Runner の送信間隔は別途確定する。
- Control Plane は HTTP の受付を始めてから30秒待ち、起動後の heartbeat が無く、最終 heartbeat から30秒以上たった Runner を1回回収する。最終 heartbeat が未記録の場合も猶予の後に回収する。起動後に heartbeat を再開した Runner は実行を続ける。常時の途絶監視は行わない。
- boot_id が未記録の場合、最初の heartbeat は比較用の初期値として保存する。既知の値が変わった場合だけ bootId の変化として回収する。
- 回収では `status_changed` イベントと `system` の `execution.interrupt` の監査ログを記録する。理由は `runner_boot_changed` または `runner_heartbeat_lost`。同じ理由を error_message に保存する。途絶の経路では Runner を `offline` にする。
- heartbeat の保存・回収・イベント・監査ログ・通知の要求は同じトランザクションで処理する。DB のエラーでは全部の更新を取り消す。起動時の回収に失敗した場合はログに残し、30秒後に回収を再試行する。Execution は再実行しない。

### 3.10 再実行（`POST /api/tasks/{id}/retry`）

入力は `mode`（`continue` / `fresh`）。`fresh` は本人が明示的に最初からやり直す場合だけに使う（requirements 8.5）。

1. タスクが `failed` / `timed_out` / `cancelled` / `completed` のときだけ受け付ける（Q131）。それ以外は 409。
2. 振り分け先が今も実行できることを確かめる。スペシャルハーネスへ直接振り分けたタスクで default_profile_id が無ければ 422。
3. 同じタスクに seq+1、指定した mode、status=`pending` の Execution を作る。版、渡す値、作業領域はまだ固定しない（claim のときに固定する）。
4. タスクを `queued` にし、audit_logs（`task.retry`）に残す。

再実行の Execution は、要求 JSON の `previous` に直前の Execution の content_id と outputs を受け取る（3.3.1）。前回に外へ出したものが今どうなっているか（開発なら PR が閉じられたか）を確かめるのは、スペシャルハーネスの prepare か publish の段である。Control Plane は受け付けた時点では判定しない。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 同時実行の上限（全体） | 2本。振り分けの提案を含む | requirements 8.7、02 の Q111 |
| 同時実行の上限（振り分け先ごと） | 1本。振り分け先は Project、またはスペシャルハーネスへの直接の振り分け | requirements 8.7、vision-alignment.md 5章 |
| 枠に数えない状態 | `pending`、`waiting_approval`、`blocked`、終端 | 02 の 5.2 |
| 上限を超えたとき | `queued` で待つ | requirements 8.7 |
| 自動リトライ | 0回 | requirements 8.3 |
| 再起動で中断した Execution | `interrupted`。タスクは `failed`。作業領域は保持 | requirements 8.3、契約 10.3 |
| Profile・渡す値・作業領域の固定 | claim の時点で Execution に固定。再開でも変えない | requirements 8.3、契約 10.3 |
| 本人の判断の場所 | 管理画面だけ（owner_decisions） | 本人決定 3 |
| Collaboration Mode | `solo` だけ | requirements 8.2 |
| 1タスクの Execution | 複数可。再実行は新しい Execution を足す。本人の判断の後の再開は同じ Execution | requirements 8.3、契約 10.3 |

## 5. インターフェース

- 本人用: `GET/POST /api/tasks`、`GET /api/tasks/{id}`、`POST /api/tasks/{id}/cancel`、`POST /api/tasks/{id}/retry`、`GET /api/decisions`、`GET /api/decisions/{id}`、`POST /api/decisions/{id}/answer`、`GET /api/executions/{id}`
- Runner 用: `POST /api/runners/{id}/claim`、`POST /api/runners/{id}/heartbeat`、`POST /api/executions/{id}/events`、`POST /api/executions/{id}/artifacts`、`POST /api/executions/{id}/result`、`POST /api/route-proposals/{id}/result`

定義は [`01-architecture.md`](./01-architecture.md) 5.1・5.2。状態の値は [`02-data-model.md`](./02-data-model.md) 5.1・5.2・5.6。

## 6. エラー処理

| 状況 | 応答・処理 |
|---|---|
| 振り分け先が無い・両方ある、受け入れ条件が必須なのに空 | 400 `validation_failed` |
| 振り分け先が未登録、直接振り分けで既定の版が無い | 422 `precondition_failed` |
| 許されない状態遷移、判断済みの判断待ちへの回答、同じ段の報告の内容違い | 409 `conflict` |
| 回答の basis の不一致 | 422 `precondition_failed` |
| claim と中止が同時に起きた | どちらも `WHERE status = ?` 付きの更新で行い、後から来た方を 409 にする。中止が負けた場合は cancel_requested_at の経路で止める |
| claim で版・渡す値・設定の不備を見つけた | その Execution を `failed`（`config_error`）にし、次の候補を見る |
| 段の結果 JSON が契約に合わない | Execution を `failed`（`contract_error`） |
| Runner がほかの Runner の Execution・提案を報告 | 403、audit_logs に記録 |

## 7. スコープ外

- Linear からのタスクの取り込み、振り分けのラベルの判定、取り込みの保留と再判定、振り分けの提案の中身（[`15-linear-intake.md`](./15-linear-intake.md)）
- 承認の有効期限、照合と消費の詳細（[`17-approval.md`](./17-approval.md)）
- タスクの分類と Planning（後の Phase）
- Collaboration Mode の判定（後の Phase）
- 自動リトライ、自動の再開
- 前回に外へ出したもの（開発なら PR）の状態の確認。スペシャルハーネスが行う

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約（要求 JSON、結果 JSON、質問と再開、中止と再実行）
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API 一覧
- [`02-data-model.md`](./02-data-model.md) 状態の値と遷移、本人の判断の種類
- [`03-registry.md`](./03-registry.md) 振り分け先と版
- [`05-runner.md`](./05-runner.md) claim の後の段の実行と heartbeat
- [`07-context.md`](./07-context.md) 固定した文脈と矛盾
- [`12-notification.md`](./12-notification.md) 通知
- [`15-linear-intake.md`](./15-linear-intake.md) 取り込みと振り分けの提案
- [`16-linear-feedback.md`](./16-linear-feedback.md) Linear への書き戻し
- [`17-approval.md`](./17-approval.md) 承認

## 9. 実装単位

- [ ] [Backend] 状態遷移の定義（タスクと Execution の遷移表と対応表）と遷移関数
  - 受け入れ基準: 3.2 の表と 02 の 5.2 の表にある遷移だけが成功し、それ以外が 409 相当のエラーになるテーブル駆動テストがある; タスクと Execution の状態が同じトランザクションで更新される
- [ ] [Backend] 手動タスク作成 API（Idempotency-Key 対応）
  - 受け入れ基準: 振り分け先が無い・両方あると 400; `required` の Profile で受け入れ条件が空だと 400、`optional` なら作れる; 既定の版の無いスペシャルハーネスへの直接の振り分けが 422; 同じ Idempotency-Key の再送で同じタスクが返り行が増えない; 作成後のタスクが queued、Execution が seq=1 の pending
- [ ] [Backend] claim API と同時実行制御（Execution と振り分けの提案）
  - 受け入れ基準: pending を5件（同じ Project に2件、同じスペシャルハーネスへの直接の振り分けに2件を含む）と queued の提案1件を用意して並行に claim したとき、動いているものが全体2本・振り分け先ごと1本を超えない; 提案が Execution より先に渡る; 同じスペシャルハーネスを使う別々の Project の Execution は同時に動ける; claim で profile_id・profile_commit_sha・params・workspace_id が書き込まれ、その後に Project を変えても Execution の値が変わらない; 応答に worktree・ブランチ・PR の項目が無い; waiting_approval と blocked の Execution が枠に数えられない
- [ ] [Backend] 再開の claim（resume_stage）
  - 受け入れ基準: resume_stage=`publish` の claim で版と作業領域が書き換わらず、`resume.approvals` に承認済みと省いた操作が入る; resume_stage=`work` の claim で `resume.answer` と `resume.workSessionId` が入る; 作業領域が cleaned のとき Execution が blocked（workspace_missing）になる
- [ ] [Backend] 段の開始と終了の報告（3.4.1）
  - 受け入れ基準: 3.4.1 の表の各行の結果を送ると、表のとおりの状態・行・イベントと `next` になる; prepare の宣言の無い Profile で prepare を始める報告が 409; 宣言していない操作の承認要求、書けない errorClass、check の succeeded で contentId の無い結果が contract_error になる; 同じ報告の再送で行もイベントも増えない; 内容の違う再送が 409; 開始・終了のほかの type が 400
- [ ] [Backend] 判断待ちへの回答（3.6）
  - 受け入れ基準: 02 の 5.6 の各行の回答で 3.6 の表のとおりに遷移する; basis の不一致が 422; 回答済みの判断への回答が 409; 承認が2件ある Execution で1件だけ approve しても pending にならず、2件目の approve で pending（resume_stage=publish）になる; 1件の reject で Execution が failed（approval_rejected）になり、もう1件の判断待ちが withdrawn になる; 判断が audit_logs に残る
- [ ] [Backend] 振り分けの提案の結果と判断（3.7）
  - 受け入れ基準: 提案の結果が proposed でも failed でも route の判断待ちができる; approve でタスクが queued になり、seq=1 の Execution と add_label の linear_outbox の行ができる; reject でタスクが cancelled になる
- [ ] [Backend] 中止 API（3.8 の各経路）
  - 受け入れ基準: queued は即 cancelled; running は cancel_requested_at が記録され heartbeat の応答に中止指示が載る; waiting_approval の中止で承認が invalidated になる; publish_unknown の判断待ちの中止で failed（publish_unknown）になる; completed の中止は 409; どの経路でも作業領域が残る
- [ ] [Backend] 再起動時の回収（bootId と heartbeat 途絶）
  - 受け入れ基準: Runner が新しい bootId で heartbeat すると、その Runner の動いている Execution が interrupted、タスクが failed になる; waiting_approval と blocked の Execution は変わらない; 作業領域が retained になる; 自動で新しい Execution が作られない
- [ ] [Backend] 再実行 API（continue / fresh）
  - 受け入れ基準: failed のタスクに retry すると seq+1 の pending Execution ができる; running・blocked・waiting_approval のタスクへの retry は 409; mode が Execution に記録される; fresh の claim で新しい作業領域の行ができ、continue の claim で前の作業領域を引き継ぐ

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q131 | 再実行を受け付けるタスクの状態（旧仕様は `blocked` からの再実行も受け付けていた） | 決定（2026-11-10 本人、案を採用）: 終端（`failed` / `timed_out` / `cancelled` / `completed`）だけにする。判断待ちの `blocked` と `waiting_approval` は、判断待ちと承認が残ったままになるのを避けるため、先に中止か abort で終えてから再実行する |
| Q132 | claim で、振り分けの提案と Execution のどちらを先に渡すか | 決定（2026-11-10 本人、案を採用）: 提案を先に渡す。提案は短く、本人の判断待ちを早く出せるためである。提案が続いても Execution が待ち続けないよう、提案は全体の枠の1本までに絞る案も実装時に確かめる |
| Q133 | 振り分けの提案の途中で、タスクの中止や Runner の再起動が起きたときの値 | 決定（2026-11-10 本人、案を採用）: route_proposals.status に `cancelled` を、error_class に `interrupted` を足す（02 に反映済み）。中止は heartbeat の応答で Runner に止めさせて `cancelled`、再起動は `failed`（`interrupted`）にし、タスクが `routing` のままなら提案なしの route の判断待ちを作る |
| Q134 | 作業領域のパス（task_workspaces.path）を誰が決めるか（Control Plane は Runner のルートを知らない） | 決定（2026-11-10 本人、案を採用）: claim の要求に Runner が `workspaceRoot`（絶対パス）を入れ、Control Plane が `<workspaceRoot>/<task_workspaces.id>` を書く。Runner はディレクトリを作るときに、実パスがルートの下にあることを確かめる |
| Q135 | Profile を登録した後に全体設定 `limits.stageTimeoutMaxSec` を下げ、Profile の `timeoutSec` が上限を超えたとき | 決定（2026-11-10 本人、案を採用）: claim で `failed`（`config_error`）にする。上限を黙って縮めて Profile の宣言と違う時間で止めるより、本人に設定か版を見直してもらう |
