---
status: confirmed
範囲: 手動タスク作成、タスクと Execution の状態遷移、claim と同時実行制御、再起動時の回収、再実行、中止、Blocked からの続行
---

# 04. タスクライフサイクル 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Control Plane 側のタスク・Execution 管理

## 1. 目的

- 本人が管理画面から開発タスクを作り、プロジェクトの Default Harness で実行できるようにする（requirements 8.1, 8.2）。
- タスクと Execution の状態を永続化し、再起動後も未完了の仕事を検出できるようにする（requirements 8.3、条件10）。
- 同時実行の上限（全体2本・同一プロジェクト1本）を守る（requirements 8.7、条件13）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | 本人用 API（作成・中止・再実行・続行）、Runner 用 API（claim・heartbeat・result） |
| 出力 | tasks / executions / execution_events / execution_blocks / audit_logs の更新、通知の要求（[`12-notification.md`](./12-notification.md)） |

## 3. 処理詳細

### 3.1 手動タスク作成（`POST /api/tasks`）

入力:

| 項目 | 必須 | 検証 |
|---|---|---|
| projectId | 必須 | 登録済みであること |
| title | 必須 | 空でないこと |
| body | 必須 | 空でないこと（Issue 本文に相当） |
| acceptanceCriteria | 必須 | 空白だけの文字列は不可（requirements 8.1, 9.5） |
| shortName | 任意 | 英小文字・数字・ハイフン、1〜40文字（案）。省略時は `task` |
| `Idempotency-Key` ヘッダ | 必須（案） | 管理画面が送信ごとに生成する。同じキーの再送には最初に作ったタスクを返す |

処理:

1. tasks に `received` で行を作る。task_type は `development`、collaboration_mode は `solo` に固定する（requirements 8.1, 8.2）。
2. Project の `default_role` に対応する割り当て（project_harnesses）があることを確かめる。無ければタスクを作らず 422 を返す。
3. executions に seq=1、mode=`continue`、status=`pending` の行を作り、タスクを `queued` にする。
4. audit_logs と execution_events（`status_changed`）に記録する。

手順 1〜3 は1つのトランザクションで行う。

受け入れ条件の無い入力は API で拒否する。requirements 6.3 / 8.3 の「受け入れ条件のないタスクを Blocked にする」は Linear から来るタスク（Phase 2）の扱いであり、手動作成では入力の段階で止める。

タスク分類（requirements 8.2 工程6）は行わない。Harness の選択は Default Harness で固定する。

### 3.2 タスク状態の遷移

状態の値は [`02-data-model.md`](./02-data-model.md) 5.1。Phase 1 で起こる遷移は次のとおり。`planning` と `waiting_approval` は Phase 1 では使わない。

| 現在 | 契機 | 次 |
|---|---|---|
| `received` | 検証に通り Execution を作成 | `queued` |
| `queued` | Runner が claim | `running` |
| `queued` | 本人が中止 | `cancelled` |
| `running` | ハーネス終了、Gate 開始 | `verifying` |
| `running` / `verifying` | 失敗、再起動で中断 | `failed` |
| `running` / `verifying` | タイムアウト | `timed_out` |
| `running` / `verifying` | 本人の中止を Runner が処理 | `cancelled` |
| `running` / `verifying` | Block 原因を検出 | `blocked` |
| `verifying` | PR 作成まで完了 | `completed` |
| `blocked` | 本人が続行を選ぶ | `queued`（続きを Runner が claim する） |
| `blocked` / `failed` / `timed_out` / `cancelled` / `completed` | 本人が再実行 | `queued`（新しい Execution） |
| `blocked` | 本人が中止 | `cancelled` |

タスクの状態は、最新の Execution の状態から [`02-data-model.md`](./02-data-model.md) 5.2 の対応表で決める。両方の更新は同じトランザクションで行う。

上記以外の遷移を要求されたら 409 `conflict` を返す（例: `completed` のタスクの中止）。

requirements 21章で「タスク状態の遷移の全体」は詳細設計事項とされている。この表は Phase 1 の範囲での案である。

### 3.3 claim と同時実行制御（`POST /api/runners/{id}/claim`）

Runner は空きがあるときに claim を呼ぶ。Control Plane は1つの書き込みトランザクション（`BEGIN IMMEDIATE`）の中で次を行う。

1. アクティブな Execution（status が `claimed` / `preparing` / `running` / `verifying` / `publishing`）を数える。全体で2本以上なら「仕事なし」を返す。
2. status が `pending` の Execution を作成順に見て、次をすべて満たす最初の1件を選ぶ。
   - Project の runner_id が呼び出し元の Runner と一致する
   - 同じ Project にアクティブな Execution が無い
3. 選んだ Execution に、Project の `default_role` の割り当てから role / harness_id / profile_id / profile_version / profile_commit_sha を書き込み、status を `claimed`、claimed_at を記録する（版の固定、requirements 8.3）。
4. タスクを `running` にする。
5. Runner に実行に必要な情報を返す（3.3.1）。

`blocked` の Execution はアクティブに数えない（Q23 案）。Blocked の間はプロセスが動いていないためである。worktree は保持する。

#### 3.3.1 claim の応答（案）

```json
{
  "execution": { "id": "...", "taskId": "task-001", "seq": 2, "mode": "continue", "resumePoint": null },
  "task": { "title": "...", "body": "...", "acceptanceCriteria": "...", "shortName": "add-login" },
  "project": {
    "id": "project-a", "localRepoPath": "...", "gitRemoteUrl": "...", "baseBranch": "develop",
    "setupCommand": "...", "testCommand": "...", "testReportFormat": "junit_xml", "testReportPath": "...",
    "copyAllowlist": ["..."], "timeouts": { "setupSec": 0, "harnessSec": 0, "testSec": 0 }
  },
  "profile": { "harnessId": "...", "adapter": "claude_code", "version": "1.2.0", "commitSha": "...", "definition": {} },
  "previous": { "worktreePath": "...", "branchName": "...", "prNumber": 12 }
}
```

`previous` は同じタスクの直前の Execution の情報で、mode=`continue` のときに使う（[`06-worktree.md`](./06-worktree.md), [`11-git-publisher.md`](./11-git-publisher.md)）。

### 3.4 Runner からの報告

- `POST /api/executions/{id}/events`: ログ・進行イベントを execution_events に追加する。
- `POST /api/executions/{id}/result`: 工程の区切りごとに状態の遷移を要求する。Control Plane は 3.2 の表に従って遷移を検証し、合わない要求は 409 で拒否する。
- 終端状態（`succeeded` / `failed` / `cancelled` / `timed_out`）または `blocked` に入ったら、通知を要求する（[`12-notification.md`](./12-notification.md)）。

### 3.5 中止（`POST /api/tasks/{id}/cancel`）

| タスクの状態 | 処理 |
|---|---|
| `queued` | Execution を `cancelled` にし、タスクを `cancelled` にする |
| `running` / `verifying` | Execution の cancel_requested_at を記録する。Runner が次の heartbeat で受け取り、ハーネスと Sandbox を止めて `cancelled` を報告する（[`05-runner.md`](./05-runner.md)） |
| `blocked` | Execution を `cancelled` にし、タスクを `cancelled` にする。worktree は保持する |
| 上記以外 | 409 |

Runner は push の直前にも cancel_requested_at を確かめ、中止要求があれば push しない（[`11-git-publisher.md`](./11-git-publisher.md)）。

### 3.6 再起動時の回収（requirements 8.3、条件10）

Runner は起動のたびに新しい起動 ID（bootId）を作り、最初の heartbeat で送る。

1. Control Plane は、heartbeat の bootId が前回と違う Runner について、その Runner が持つアクティブな Execution をすべて `interrupted` にする。
2. Control Plane 自身の起動時には、Runner の heartbeat が途絶えたまま一定時間（値は実装時に確定）を過ぎた Runner のアクティブな Execution を `interrupted` にする。Runner が生きていて heartbeat を再開した場合は、その Execution を続ける。
3. `interrupted` にした Execution の error_class は `interrupted`、タスクは `failed` にする。
4. 作業中だった worktree は削除せず `retained` にする（[`06-worktree.md`](./06-worktree.md)）。
5. 失敗の通知を要求する。
6. 自動では再実行しない（自動リトライは0回）。

実装時の確定事項:

- heartbeat 途絶のしきい値と起動後の猶予は30秒。ローカルの再接続やスケジューリングの遅延を許容しつつ、未完了の Execution を長時間残さないためとする。Runner の送信間隔は別途確定する。
- Control Plane はHTTP受付を始めてから30秒待ち、起動後の heartbeat が無く、最終 heartbeat から30秒以上経過した Runner を1回回収する。最終 heartbeat が未記録の場合も猶予後に回収する。起動後に heartbeat を再開した Runner は実行を続ける。常時の途絶監視は行わない。
- boot_id が未記録の場合、最初の heartbeat は比較用の初期値として保存する。既知の値が変わった場合だけ bootId 変化として回収する。
- 回収では `status_changed` イベントと `system` の `execution.interrupt` 監査ログを記録する。理由は `runner_boot_changed` または `runner_heartbeat_lost`。同じ理由を error_message に保存する。途絶経路では Runner を `offline` にする。
- heartbeat の保存・回収・イベント・監査ログ・通知要求は同一トランザクションで処理する。DB エラーでは全更新を取り消す。起動時の回収に失敗した場合はログに残し、30秒後に回収を再試行する。Execution は再実行しない。

### 3.7 再実行（`POST /api/tasks/{id}/retry`）

入力: `mode`（`continue` / `fresh`）。`fresh` は本人が明示的に最初からやり直す場合だけに使う（requirements 8.5）。

1. タスクが `failed` / `timed_out` / `cancelled` / `completed` / `blocked` のときだけ受け付ける。
2. 同じタスクに seq+1 の Execution を `pending` で作る。Profile の版はまだ固定しない（claim 時に固定する）。
3. タスクを `queued` にする。

前回の PR がクローズ済み・マージ済みかどうかは GitHub を見なければ分からないため、Runner が準備の段階で確かめる（[`11-git-publisher.md`](./11-git-publisher.md) 3.4）。Control Plane は受け付けた時点では判定しない。

### 3.8 Blocked からの続行（`POST /api/tasks/{id}/resume`、追加案）

requirements 8.5「止まったタスクは、本人が変更内容を確認し、続行を選んだ場合だけ先へ進む」を実現する。

入力:

| 項目 | 内容 |
|---|---|
| blockId | 判断する execution_blocks の ID |
| decision | `continue` / `abort` |
| headSha | 本人が確認した時点の HEAD SHA（Q14 案） |

処理:

1. execution_blocks の行が未判断であること、`headSha` が行の head_sha と一致することを確かめる。一致しなければ 422。
2. `continue` を受け付けるのは、cause が `gate2_missing_tests` / `gate2_unverifiable` / `gate3_mismatch` のときだけ。`context_conflict` と `git_state` は続行できず、本人が原因を直して再実行する。
3. `continue` なら判断を記録し、Execution の status を `pending`、resume_point を次の工程（Gate 2・3 で止まったなら `gate3`。[`10-verification-gates.md`](./10-verification-gates.md) 3.5）にしてタスクを `queued` に戻す。Runner は claim 後に resume_point から続ける。同時実行制御は通常の claim と同じく適用される。
4. Runner は再開時に HEAD が head_sha と一致することを確かめる。一致しなければ `git_state` で再び Blocked にする。
5. `abort` なら Execution とタスクを `cancelled` にする。worktree は保持する。

この操作は承認の仕組み（Phase 2）ではない。本人の判断を、確認した SHA に結び付けて記録するだけである。

resume_point の列定義は [`02-data-model.md`](./02-data-model.md) の executions を参照する。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 同時実行の上限（全体） | 2本 | requirements 8.7 |
| 同時実行の上限（同一プロジェクト） | 1本 | requirements 8.7 |
| 上限超過時 | Queued で待つ | requirements 8.7 |
| 自動リトライ | 0回 | requirements 8.3 |
| 再起動で中断した Execution | 失敗（中断）。タスクは Failed。worktree は保持 | requirements 8.3 |
| Profile・Context の版の固定 | Runner が取得した時点で Execution に固定。実行途中で切り替えない | requirements 8.3 |
| タスク分類 | 行わない。Default Harness で実行 | requirements 8.2 |
| Collaboration Mode | Solo 固定 | requirements 8.2 |
| task_steps | 使わない | requirements 8.2 |
| 1タスクの Execution | 複数可。再実行は新しい Execution を追加 | requirements 8.3 |
| 手動作成の受け入れ条件 | 必須入力 | requirements 8.1 |

## 5. インターフェース

- 本人用: `GET/POST /api/tasks`, `GET /api/tasks/{id}`, `POST /api/tasks/{id}/cancel`, `POST /api/tasks/{id}/retry`, `POST /api/tasks/{id}/resume`, `GET /api/executions/{id}`
- Runner 用: `POST /api/runners/{id}/claim`, `POST /api/runners/{id}/heartbeat`, `POST /api/executions/{id}/events`, `POST /api/executions/{id}/result`

定義は [`01-architecture.md`](./01-architecture.md) 5.1 / 5.2。状態の値は [`02-data-model.md`](./02-data-model.md) 5.1 / 5.2 / 5.6。

## 6. エラー処理

| 状況 | 応答・処理 |
|---|---|
| 受け入れ条件が空 | 400 `validation_failed` |
| Default Harness の割り当てが無い | 422 `precondition_failed` |
| 許されない状態遷移 | 409 `conflict` |
| claim と中止が同時に起きた | どちらも `WHERE status = ?` 付きの更新で行い、後から来た方を 409 にする。中止が負けた場合は cancel_requested_at の経路で止める |
| Runner が他の Runner の Execution を報告 | 403、audit_logs に記録 |
| 続行時の SHA 不一致 | 422 `precondition_failed` |

## 7. スコープ外

- Linear からのタスク生成、Team + Label ルーティング、受け入れ条件の無い Issue の Blocked 化とコメント返却（Phase 2）
- 承認待ち（`waiting_approval`）（Phase 2）
- タスク分類・Planning（後の Phase）
- Collaboration Mode の判定（Phase 5）
- 自動リトライ

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API 一覧
- [`02-data-model.md`](./02-data-model.md) tasks / executions / execution_blocks と状態の値
- [`03-registry.md`](./03-registry.md) Default Harness の割り当て
- [`05-runner.md`](./05-runner.md) claim 後の処理と heartbeat
- [`10-verification-gates.md`](./10-verification-gates.md) Block の原因
- [`11-git-publisher.md`](./11-git-publisher.md) 再実行時の PR の扱い
- [`12-notification.md`](./12-notification.md) 通知

## 9. 実装単位

- [ ] [Backend] 状態遷移の定義（タスク・Execution の遷移表と対応表）と遷移関数
  - 受け入れ基準: 3.2 の表にある遷移だけが成功し、それ以外が 409 相当のエラーになるテーブル駆動テストがある; タスクと Execution の状態が同一トランザクションで更新される
- [ ] [Backend] 手動タスク作成 API（Idempotency-Key 対応）
  - 受け入れ基準: 受け入れ条件が空だと 400; Default Harness の割り当てが無いと 422; 同じ Idempotency-Key の再送で同じタスクが返り行が増えない; 作成後のタスクが queued、Execution が seq=1 の pending
- [ ] [Backend] claim API と同時実行制御
  - 受け入れ基準: pending を5件（同一プロジェクト2件を含む）用意して並行に claim したとき、アクティブが全体2本・同一プロジェクト1本を超えない; claim 時に profile_id と profile_commit_sha が Execution に書き込まれる; claim 後に割り当てを変えても Execution の版が変わらない
- [ ] [Backend] Runner の結果報告 API と状態遷移の検証
  - 受け入れ基準: 遷移表に無い報告が 409; 終端状態と blocked で通知の要求が1回だけ発生する
- [ ] [Backend] 中止 API（queued / running / blocked の各経路）
  - 受け入れ基準: queued のタスクは即 cancelled; running のタスクは cancel_requested_at が記録され heartbeat の応答に中止指示が載る; completed のタスクの中止は 409
- [ ] [Backend] 再起動時の回収（bootId と heartbeat 途絶）
  - 受け入れ基準: Runner が新しい bootId で heartbeat すると、その Runner のアクティブな Execution が interrupted、タスクが failed になる; worktree が retained になる; 自動で新しい Execution が作られない
- [ ] [Backend] 再実行 API（continue / fresh）
  - 受け入れ基準: failed のタスクに retry すると seq+1 の pending Execution ができる; running のタスクへの retry は 409; mode が Execution に記録される
- [ ] [Backend] Blocked からの続行 API
  - 受け入れ基準: headSha が一致しないと 422; context_conflict の Block に continue すると 422; continue で Execution が pending に戻り resume_point が記録される; abort でタスクが cancelled になる; 判断が audit_logs に残る
