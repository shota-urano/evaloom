---
status: confirmed
confirmed_rev: 8e04e29
written: 2026-11-10
範囲: 整合性アンカー。テーブル、状態の列挙値、段の名前、イベント種別、エラー分類、本人の判断待ちの原因。スペシャルハーネスとの入出力と Profile 定義のスキーマはハーネス契約を参照する
---

# 02. データモデル 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: SQLite のテーブル、状態の列挙値、段の名前、イベント種別、エラー分類、本人の判断待ちの種類と原因

この文書は整合性アンカーである。テーブル・列名・列挙値の名前はここだけで決め、ほかの詳細仕様はここに従う。スペシャルハーネスとの入出力（要求 JSON、結果 JSON、finding、Profile 定義）の形は、ハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）だけで決め、この文書は参照する。

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。いまの実装との食い違いは扱わない（本人決定 1）。型・インデックスの最終確定は実装時に行う（requirements 17章）。

## 1. 目的

- Control Plane、Runner、Adapter、管理画面が、同じ名前・同じ値を使えるようにする。
- Evaloom が仕事の種類に固有の情報を持たないようにする。開発の情報（worktree、テストコマンド、ブランチ、PR、commit SHA）は列として持たず、スペシャルハーネスが決める不透明な値として記録する（vision.md 4章）。
- 再起動の後に、未完了の仕事と本人の判断待ちを見つけられるよう、状態を保存する（requirements 8.3）。
- Execution ごとに、使ったスペシャルハーネスの版、渡した値、段ごとの結果を後から確かめられるようにする（requirements 7.1、8.3）。

## 2. 入出力

- 入力: 各詳細仕様の処理が書き込む行。Runner が報告する段の結果（契約 6.5）。
- 出力: API のレスポンス（[`01-architecture.md`](./01-architecture.md)）、管理画面の表示、Linear への書き戻し。

## 3. 処理詳細

### 3.1 共通の約束

| 項目 | 約束 |
|---|---|
| DB | SQLite、WAL モード（requirements 2.2） |
| 書き込み主体 | Control Plane だけ（[`01-architecture.md`](./01-architecture.md) 3.1） |
| 列名 | snake_case。API の JSON は camelCase に変換する |
| ID | 既定は ULID の文字列。例外は下表 |
| 時刻 | UTC の RFC 3339 文字列 |
| JSON 列 | TEXT に JSON を保存し、アプリ側で検証する |
| 不透明な値 | `params`、`content_id`、`outputs`、`target` など、スペシャルハーネスが決める値は、保存・表示・完全一致の比較・受け渡しだけに使い、Evaloom は意味を解釈しない（vision.md 4章） |
| マイグレーション | 段階的に追加する（requirements 17章） |

ID の例外は次のとおりとする。

| 対象 | 形式 | 理由 |
|---|---|---|
| projects.id | 本人が付ける slug（例 `customer-system`） | 管理画面と振り分けの表で読みやすくする |
| harnesses.id | 本人が付ける slug（例 `software-engineer`） | 同上。契約の要求 JSON の `profile.harnessId` に入る |
| tasks.id | `task-<連番>`（例 `task-001`） | 本人が読み上げやすい短い ID にする |
| routes.label | 振り分けのラベルの名前そのもの | ラベルは振り分けの表の中で一意（本人決定 2） |

### 3.2 登録の表

#### runners

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | Runner ID（例 `mac-mini`） |
| name | TEXT | 表示名 |
| token_hash | TEXT | Runner トークンのハッシュ。平文は保存しない |
| status | TEXT | `online` / `offline` |
| last_heartbeat_at | TEXT NULL | 最後の heartbeat |
| boot_id | TEXT NULL | 最後に受信した heartbeat の bootId |
| created_at | TEXT | |

作業領域を作るディレクトリと鍵の保管場所は、Runner の設定ファイルに持ち、この表には持たない（[`05-runner.md`](./05-runner.md)、[`01-architecture.md`](./01-architecture.md) 3.5）。

#### harnesses（スペシャルハーネス）

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | スペシャルハーネスの ID |
| name | TEXT | 表示名 |
| repo_url | TEXT | スペシャルハーネスのリポジトリの URL |
| repo_path | TEXT | Control Plane と Runner がタグと commit を読むローカルの複製のパス |
| default_profile_id | TEXT NULL FK→harness_profiles | スペシャルハーネスへ直接振り分けたタスクで使う版。NULL なら直接振り分けたタスクを実行しない（`config_missing_profile`） |
| approval_waivers | TEXT (JSON 配列) | スペシャルハーネスへ直接振り分けたタスクで、承認を省く操作の名前の配列。既定 `[]`（すべての承認を求める）。default_profile_id の Profile で `waivable: true` と宣言された操作だけを入れられる（契約 7.3、Q113）。Project 経由のタスクには使わない |
| created_at / updated_at | TEXT | |

専門分野、対応タスク、Adapter、対応モデル、利用可能ツール、Tool Permissions の列は持たない。どれも Profile 定義（契約 5章）が宣言する（Q118）。ミクロハーネスの一覧は、Evaloom のコードに登録した Adapter（`claude_code` / `codex`、[`08-harness-adapter.md`](./08-harness-adapter.md)）である。

#### harness_profiles（スペシャルハーネスの版）

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| harness_id | TEXT FK→harnesses | |
| version | TEXT | SemVer（正本）。`UNIQUE(harness_id, version)` |
| commit_sha | TEXT | タグ `profile/v<version>` を解決した commit（契約 4.2） |
| tag_name | TEXT | `profile/v<version>` |
| contract_version | TEXT | Profile 定義の `contractVersion` |
| definition | TEXT (JSON) | 登録時に読んだ Profile 定義（契約 5章）の全文を JSON にしたもの |
| definition_hash | TEXT | definition の SHA-256 |
| operation_status | TEXT | `provisional`（暫定運用）/ `official`（正式運用）。評価基盤ができるまで `provisional`（requirements 11.1） |
| registered_at | TEXT | |

- 一度登録した行は更新しない。同じ版を別の commit で登録しようとしたら拒否する（契約 4.2）。
- Model ID、ミクロハーネス、段の宣言、承認の宣言は definition の中にある。表示と検索のために列へ写すかは実装時に確定する。

#### projects（Project）

Project が持つのは、vision.md 4章の6項目だけとする（本人決定 6）。

| 6項目 | 置き場所 |
|---|---|
| 名前 | projects.name |
| 振り分けのラベル | routes（label → project_id）。Project ごとに1つ |
| 頼むスペシャルハーネス | projects.harness_id と projects.profile_id（固定する版） |
| そのスペシャルハーネスに渡す値 | projects.params |
| 承認を省く設定 | projects.approval_waivers |
| 動かすマシン（Runner）の指定 | projects.runner_id |

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | Project ID |
| name | TEXT | 名前 |
| harness_id | TEXT FK→harnesses | 頼むスペシャルハーネス |
| profile_id | TEXT FK→harness_profiles | 固定する版。harness_id の版でなければならない。新しい版を登録しても自動では変わらない（requirements 4.3） |
| params | TEXT (JSON) | 渡す値。既定 `{}`。Evaloom は解釈しない。Profile が `params.schema` を持つなら形だけ確かめる（契約 5.2、Q115） |
| approval_waivers | TEXT (JSON 配列) | 承認を省く操作の名前の配列。既定 `[]`。profile_id の Profile で `waivable: true` と宣言された操作だけを入れられる（契約 7.3） |
| runner_id | TEXT FK→runners | 動かすマシン |
| created_at / updated_at | TEXT | |

- ローカルのパス、Git の URL、ベースブランチ、テストコマンド、setup、コピー許可リスト、Context の置き場所、許可操作、タイムアウトの列は持たない。必要ならスペシャルハーネスが params として受け取るか、対象から自分で読む（本人決定 1、6）。
- profile_id を別の版に変えるとき、新しい版で `waivable: true` でない操作が approval_waivers に残っていれば、その変更を 422 で拒否する。
- approval_waivers と profile_id の変更は audit_logs に残す。Runner とスペシャルハーネスには変更の経路を作らない（requirements 15.3）。
- harnesses.approval_waivers と default_profile_id にも同じ規則を当てる（Q113）。default_profile_id を別の版に変えるとき、新しい版で `waivable: true` でない操作が残っていれば 422 で拒否する。

#### routes（振り分けの表）

ラベルから振り分け先を引く表である（本人決定 2、vision.md 3章）。

| 列 | 型 | 説明 |
|---|---|---|
| label | TEXT PK | 振り分けのラベルの名前（例 `route:customer-system`）。完全一致で照合する |
| project_id | TEXT NULL FK→projects | 振り分け先が Project のとき |
| harness_id | TEXT NULL FK→harnesses | 振り分け先がスペシャルハーネスのとき |
| created_at / updated_at | TEXT | |

- project_id と harness_id は、ちょうど一方だけを入れる（CHECK 制約）。
- `UNIQUE(project_id)`。1つの Project に振り分けのラベルは1つだけ。harness_id にも `UNIQUE(harness_id)` を付け、スペシャルハーネスへ直接振り分けるラベルも1つだけとする。
- ラベルは全体設定 `linear.routingLabelPrefix` で始まらなければならない（3.8、Q120）。
- Linear Team の列は持たない。振り分けに Team を使わない（本人決定 2）。

### 3.3 タスクと実行の表

#### tasks

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | `task-<連番>` |
| source | TEXT | `manual` / `linear` |
| project_id | TEXT NULL FK→projects | 振り分け先が Project のとき |
| harness_id | TEXT NULL FK→harnesses | 振り分け先がスペシャルハーネスのとき |
| title | TEXT | 件名 |
| body | TEXT | 本文。外から来た文として扱う（requirements 15.1） |
| acceptance_criteria | TEXT | 受け入れ条件。振り分け先の Profile が `acceptanceCriteria: required` なら空を許さない（契約 5.2） |
| collaboration_mode | TEXT | `solo` だけ（requirements 8.2） |
| status | TEXT | 5.1 のタスク状態 |
| intake_block_cause | TEXT NULL | 5.7 の保留の理由。保留していなければ NULL |
| idempotency_key | TEXT UNIQUE | 二重作成の防止（手動は画面が採番、Linear は `linear:<Issue ID>:<attach_seq>`） |
| created_at / updated_at | TEXT | |

- project_id と harness_id は、多くても一方だけを入れる（CHECK 制約）。両方 NULL（振り分け先が未定）を許すのは、status が `received` / `routing` / `cancelled` のときだけとする（アプリ側で検証する）。
- 振り分け先は、タスクを `queued` にした時点で決まり、その後は変えない。スペシャルハーネスの版は claim の時点で executions に固定する。
- task_type と short_name の列は持たない（仕事の種類と、ブランチ名などの名前の付け方はスペシャルハーネスが決める）。

#### executions

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| task_id | TEXT FK→tasks | |
| seq | INTEGER | タスク内の通し番号。`UNIQUE(task_id, seq)` |
| mode | TEXT | `continue` / `fresh`（契約 10.3） |
| status | TEXT | 5.2 の Execution 状態 |
| resume_stage | TEXT NULL | 本人の判断の後に再開する段。`work`（質問への回答の後）/ `publish`（承認・続行の後）。最初から始めるときは NULL |
| runner_id | TEXT NULL FK→runners | claim した Runner |
| route_kind | TEXT NULL | `project` / `harness`。claim 時に固定 |
| project_id | TEXT NULL | claim 時に固定（route_kind=`project` のとき） |
| harness_id | TEXT NULL | claim 時に固定 |
| profile_id | TEXT NULL | claim 時に固定 |
| profile_version | TEXT NULL | claim 時に固定（表示用の重複保持） |
| profile_commit_sha | TEXT NULL | claim 時に固定 |
| contract_version | TEXT NULL | claim 時に固定 |
| params | TEXT (JSON) NULL | claim 時に固定した渡す値。直接振り分けでは `{}` |
| workspace_id | TEXT NULL FK→task_workspaces | 使った作業領域 |
| work_session_id | TEXT NULL | 作業の段でミクロハーネスが返したセッション識別子。質問の後の再開に使う（契約 8章） |
| content_id | TEXT NULL | 確かめる段が報告した中身の識別子（契約 6.5）。不透明な値 |
| outputs | TEXT (JSON 配列) NULL | 出す段が報告した外に出したものの参照（`kind`、`label`、`url`）。不透明な値 |
| error_class | TEXT NULL | 5.5 のエラー分類 |
| error_message | TEXT NULL | 機密情報を除いた説明 |
| cancel_requested_at | TEXT NULL | 中止の要求時刻 |
| claimed_at / started_at / finished_at | TEXT NULL | |
| last_heartbeat_at | TEXT NULL | Runner の最終報告 |
| created_at | TEXT | |

- worktree_id、branch_name、base_sha、harness_head_sha、verified_sha、pushed_sha、pr_number、pr_url の列は持たない。開発の値は content_id と outputs と成果物に入る。
- Evaloom は content_id を、承認の結び付け（3.4 approvals）と表示と次の Execution への受け渡し（契約 6.1 の `previous.checkedContentId`）にだけ使う。確かめた段と出す段の照合はしない（本人決定 5）。

#### task_workspaces（作業領域）

タスクごとに Runner が用意するディレクトリである（契約 0章、6.2）。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| task_id | TEXT FK→tasks | |
| runner_id | TEXT FK→runners | |
| path | TEXT | Runner の作業領域のルートの下の絶対パス |
| status | TEXT | `active` / `retained`（中断・失敗・判断待ちで保持中）/ `cleaned` |
| created_by_execution_id | TEXT FK→executions | |
| created_at / cleaned_at | TEXT | |

- mode=`continue` は同じタスクの最新の `active` / `retained` の行を引き継ぎ、mode=`fresh` は新しい行を作る。保持期間と削除は Q121。

#### stage_runs（段の実行）

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK→executions | |
| stage | TEXT | 4.1 の段の名前 |
| attempt | INTEGER | 同じ Execution・同じ段の通し番号（再開で増える）。`UNIQUE(execution_id, stage, attempt)` |
| status | TEXT | `running` / `succeeded` / `failed` / `blocked` / `needs_approval` / `unknown` / `question` / `cancelled` / `timed_out` |
| exit_code | INTEGER NULL | |
| error_class | TEXT NULL | 5.5 |
| request_artifact_id | TEXT NULL FK→artifacts | 渡した要求 JSON（鍵の値は含まない） |
| result_artifact_id | TEXT NULL FK→artifacts | 受け取った結果 JSON。作業の段では NULL |
| started_at / finished_at | TEXT | |

- status の `blocked` / `needs_approval` / `unknown` は契約 6.5 の結果の値、`question` は作業の段で `out/question.json` があったとき（契約 8章）に使う。

#### check_results（確かめた結果）

gate_results を置き換える。確かめる段の結果を構造化して残す。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK→executions | |
| stage_run_id | TEXT FK→stage_runs | |
| result | TEXT | `pass`（succeeded・needs_approval）/ `fail`（failed）/ `blocked`（blocked） |
| content_id | TEXT NULL | 確かめた中身の識別子 |
| summary | TEXT NULL | 結果 JSON の summary（機密情報除外済み） |
| findings | TEXT (JSON 配列) | 契約 14章の finding |
| created_at | TEXT | |

#### execution_events

| 列 | 型 | 説明 |
|---|---|---|
| id | INTEGER PK AUTOINCREMENT | SSE の event id に使う |
| execution_id | TEXT FK→executions | |
| type | TEXT | 5.3 のイベント種別 |
| payload | TEXT (JSON) | 機密情報を除いた内容 |
| created_at | TEXT | |

#### context_revisions

Execution ごとに固定した文脈を保存する（requirements 7.1、8.3）。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK→executions | |
| layer | TEXT | `global` / `harness` / `task` / `reported` |
| kind | TEXT | `global_policy` / `instructions` / `skill` / `settings` / `record` / `task_input` / `reported` |
| source_path | TEXT | 読み取り元の相対パス。`reported` では、スペシャルハーネスが報告した `source` と `path` を `<source>:<path>` の形でつないだ値（[`07-context.md`](./07-context.md) 3.4） |
| source_revision | TEXT | 読み取り元の版（Profile の commit SHA、記録の ID、報告された revision） |
| content_hash | TEXT | SHA-256 |
| content | TEXT NULL | 内容。`reported` では持たない（記録だけ） |
| created_at | TEXT | |

- layer=`project`（対象リポジトリの `AGENTS.md` の固定）は持たない。スペシャルハーネスが準備の段で固定して報告したもの（契約 6.5 の `reportedContext`）を layer=`reported` として記録だけ残す（vision-alignment.md 5章の推奨）。
- 記録の受け渡し（契約 9章）は layer=`harness`、kind=`record` で残す。

### 3.4 本人の判断の表

本人の判断（振り分けの承認、操作の承認、止まった仕事の続行、作業中の AI の質問への回答）は、すべて owner_decisions の行として管理画面に出す（本人決定 3）。種類ごとの詳しい値は、route_proposals と approvals に持つ。execution_blocks は owner_decisions に置き換える。

#### owner_decisions（本人の判断待ち）

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| kind | TEXT | 5.6 の判断の種類（`route` / `approval` / `blocked` / `question`） |
| task_id | TEXT FK→tasks | |
| execution_id | TEXT NULL FK→executions | kind=`route` では NULL |
| subject_id | TEXT NULL | kind=`route` は route_proposals.id、kind=`approval` は approvals.id。ほかは NULL |
| cause | TEXT NULL | kind=`blocked` のとき 5.6 の原因。ほかは NULL |
| basis | TEXT NULL | 本人が判断の対象として見た値。kind=`approval` と cause=`check_blocked` では content_id、kind=`route` では提案の ID。回答で同じ値を送らせる |
| detail | TEXT (JSON) | 画面に出す内容（質問の本文と選択肢、finding の要約、出す段の message など）。機密情報除外済み |
| status | TEXT | `open` / `answered` / `withdrawn` |
| withdrawn_reason | TEXT NULL | `cancelled`（タスクの中止、または同じ Execution の別の承認の却下で Execution が終わった、[`17-approval.md`](./17-approval.md) 3.6）/ `superseded`（新しい判断待ちに置き換え、Linear で本人がラベルを付けた）/ `expired` |
| answer | TEXT (JSON) NULL | 5.6 の回答の形 |
| answered_at | TEXT NULL | |
| created_at | TEXT | |

- 同じ task_id で status が `open` の行は1件までとする（部分一意インデックス）。ただし kind=`approval` は、同じ Execution で操作ごとに1件ずつ並んでよい（`UNIQUE(execution_id, subject_id) WHERE status='open'`）。
- 判断の受け付け、取り下げは audit_logs に残す（actor は回答が `owner`、取り下げが `system`）。
- 行を作ったら、Linear 起因のタスクには「管理画面で判断待ち」のコメントを linear_outbox に書く（kind=`decision_open`、3.7）。回答の後の Linear への書き戻し（振り分けのラベルの追加、状態の変更）も linear_outbox に書く。

#### route_proposals（振り分けの提案）

ラベルで振り分け先が決まらないタスク（5.6 の `route` の契機）について、AI が鍵の無い隔離の中で振り分け先を1つ提案した記録である（vision.md 3章）。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| task_id | TEXT FK→tasks | |
| trigger | TEXT | `no_label`（振り分けのラベルが無い）/ `unknown_label`（routes に無い）/ `multiple_labels`（2つ以上） |
| seen_labels | TEXT (JSON 配列) | 判定したときに Issue に付いていた振り分けのラベル |
| candidates | TEXT (JSON 配列) | 提案を作るときに AI に見せた振り分け先の一覧（routes の label、振り分け先の種類と ID、Profile の description） |
| status | TEXT | `queued`（Runner の取得待ち）/ `running` / `proposed` / `failed` / `decided` / `superseded` / `cancelled`（タスクの中止で止めた、Q133） |
| runner_id | TEXT NULL FK→runners | |
| proposed_label | TEXT NULL | AI が選んだラベル。candidates の中のものでなければ捨てて `failed` にする |
| reason | TEXT NULL | AI が書いた理由（2000 文字以内、機密情報除外済み） |
| error_class | TEXT NULL | `sandbox_error` / `timeout` / `contract_error`（提案が一覧に無い、形が合わない）/ `interrupted`（Runner の再起動で中断、Q133）。`routing.proposer` の model が null で提案を作らなかったときは NULL |
| decision_id | TEXT NULL FK→owner_decisions | |
| created_at / finished_at | TEXT | |

- 全体設定 `routing.proposer` の model が null のときは、行を作って Runner に渡さずに `failed`（error_class は NULL）にする（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.4）。
- 提案が `proposed` になっても `failed` になっても、owner_decisions（kind=`route`）を作る。`failed` のときは提案なしで、本人が一覧から選ぶ。
- 提案の途中でタスクを中止したら、`queued` の行はそのまま `cancelled` にする。`running` の行は、heartbeat の応答の `cancelRouteProposalIds`（[`01-architecture.md`](./01-architecture.md) 5.2）で Runner に止めさせ、`cancelled` にする（Q133）。
- 提案の途中で Runner が再起動したら、`running` の行を `failed`（`interrupted`）にする。タスクが `routing` のままなら、提案なしの owner_decisions（kind=`route`）を作る（Q133）。
- 承認されるまで、タスクは `routing` のままで実行しない。
- 提案を待つ間に本人が Linear で振り分けのラベルを1つだけ付けて決まったら、普通の振り分けとして扱い（本人決定 3）、提案を `superseded`、判断待ちを `withdrawn`（`superseded`）にする。

#### approvals（操作の承認）

前の版の承認の表を、スペシャルハーネスが宣言する操作に一般化したもの（契約 7章）。使い方は [`17-approval.md`](./17-approval.md) 3.3〜3.8 で定める。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| task_id | TEXT FK→tasks | |
| execution_id | TEXT FK→executions | |
| decision_id | TEXT FK→owner_decisions | |
| requested_stage | TEXT | 承認を求めた段（`check` / `publish`） |
| operation | TEXT | Profile の `approvals[].operation` |
| target | TEXT (JSON) | 結果 JSON の `target`（表示用、不透明） |
| summary | TEXT NULL | 結果 JSON の `summary` |
| content_id | TEXT | 承認を求めたときの中身の識別子 |
| params_hash | TEXT | operation・target・content_id を正規化した JSON の SHA-256 |
| status | TEXT | `pending` / `approved` / `rejected` / `consumed` / `invalidated` / `expired` |
| invalidated_reason | TEXT NULL | `content_changed` / `superseded` / `cancelled` |
| decided_at | TEXT NULL | |
| expires_at | TEXT NULL | 承認した時刻 + 有効時間（既定は 17 の Q55） |
| max_uses | INTEGER | 1 |
| use_count | INTEGER | 0 から始める |
| consumed_at | TEXT NULL | |
| created_at | TEXT | |

- 同じ execution_id と operation で、status が `pending` / `approved` の行は1件までとする。新しく求めたら前の行を `invalidated`（`superseded`）にする（契約 7.2）。
- 消費は、出す段の問い合わせ（契約 7.4）を受けた Runner が `POST /api/executions/{id}/authorize`（[`01-architecture.md`](./01-architecture.md) 5.2）を呼び、Control Plane が期待する現在値を条件にした1つの UPDATE で行う。content_id が違えば `invalidated`（`content_changed`）にする。
- 承認を省いた操作（projects.approval_waivers）は行を作らず、execution_events に `approval_waived` を残す。出す段の問い合わせの時点で振り分け先の approval_waivers（Project なら projects、直接振り分けたタスクなら harnesses）を読み直し、省く設定が外れていれば許可しない（[`17-approval.md`](./17-approval.md) Q61 と同じ考え方）。

### 3.5 記録の表

#### artifacts

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT NULL FK→executions | |
| route_proposal_id | TEXT NULL FK→route_proposals | 振り分けの提案のログ。execution_id とどちらか一方 |
| stage | TEXT | 4.1 の段の名前、または `runner` / `proposal` |
| kind | TEXT | `log` / `out_file`（段の `out` のファイル）/ `request_json` / `result_json` / `work_summary`（`out/summary.md`）/ `question`（`out/question.json`）/ `proposal_result`（振り分けの提案の AI が `out` に書いた結果のファイル、[`08-harness-adapter.md`](./08-harness-adapter.md) 3.10、[`15-linear-intake.md`](./15-linear-intake.md) 3.6.4） |
| name | TEXT | `out` からの相対パスなど、表示用の名前 |
| storage_path | TEXT | Evaloom のデータディレクトリ内のパス（Git には入れない） |
| sha256 | TEXT | |
| size_bytes | INTEGER | |
| redacted | INTEGER | 機密情報の除外処理を通したか（0 / 1） |
| created_at | TEXT | |

#### harness_records（スペシャルハーネスの記録）

契約 9章の記録である（[`19-routing-and-stages.md`](./19-routing-and-stages.md) 3.8 の案A を引き継ぐ）。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| harness_id | TEXT FK→harnesses | 記録はスペシャルハーネス単位で持ち、Profile の版をまたぐ |
| artifact_id | TEXT FK→artifacts | 記録のファイル（kind=`out_file`） |
| execution_id | TEXT FK→executions | |
| task_id | TEXT FK→tasks | |
| source_identifier | TEXT NULL | タスクの出所の識別子（例 `ENG-123`） |
| file_name | TEXT | 次の仕事の `records/` に置くときの名前 |
| created_at | TEXT | |
| deleted_at | TEXT NULL | 本人が消した時刻。消した記録は次の仕事に渡さない |

#### audit_logs

| 列 | 型 | 説明 |
|---|---|---|
| id | INTEGER PK AUTOINCREMENT | |
| actor | TEXT | `owner` / `runner:<id>` / `system` |
| action | TEXT | 例 `project.update`、`route.update`、`profile.register`、`task.cancel`、`decision.answer`、`approval.consume`、`settings.update`、`auth.reject` |
| target_type / target_id | TEXT | |
| detail | TEXT (JSON) | 変更前後の値など。秘密情報は含めない |
| created_at | TEXT | |

#### usage_records

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK→executions | |
| stage | TEXT | 4.1 の段の名前 |
| input_tokens / output_tokens | INTEGER NULL | わかる場合だけ |
| wall_time_ms | INTEGER | 実行時間 |
| cost_amount | REAL NULL | API 利用時だけ（requirements 12.5） |
| created_at | TEXT | |

### 3.6 本人のログインの表

[`18-remote-access.md`](./18-remote-access.md) 3.8 の owner_credentials と owner_sessions を使う。列の定義は 18 に置く。

### 3.7 Linear の表

[`15-linear-intake.md`](./15-linear-intake.md) と [`16-linear-feedback.md`](./16-linear-feedback.md) の表を、本人決定 2・3 に合わせてここで定める。

#### linear_poll_teams

照会先（全体設定 `linear.teamId`、本人決定 2）の照会の状況を持つ。照会先は1つだが、照会先を変えたときに最初の照会からやり直すため、Team ID ごとに行を持つ。

| 列 | 型 | 説明 |
|---|---|---|
| team_id | TEXT PK | Linear Team ID |
| baseline_done_at | TEXT NULL | 最初の照会を終えた時刻。NULL のうちは起動しない（15 の Q64） |
| last_success_at | TEXT NULL | |
| last_error | TEXT NULL | `auth` / `rate_limited` / `network` / `other` |
| consecutive_failures | INTEGER | |

#### linear_issue_states

[`15-linear-intake.md`](./15-linear-intake.md) 3.8 の列のまま使う（linear_issue_id、issue_identifier、team_id、ai_ready、attach_seq、issue_updated_at、last_decision、last_decision_at、first_seen_at、last_seen_at、removed_at、recheck_requested_at）。last_decision に `routing` を足す（振り分けの提案に回した）。

#### linear_issue_links

[`15-linear-intake.md`](./15-linear-intake.md) 3.8 の列のまま使う。`UNIQUE(task_id)`、`UNIQUE(linear_issue_id, attach_seq)`。

#### linear_outbox

[`16-linear-feedback.md`](./16-linear-feedback.md) 3.7 の列に、ラベルの追加を足す。

| 列 | 型 | 説明 |
|---|---|---|
| action | TEXT | `comment` / `set_state` / `add_label` |
| kind | TEXT | 5.8 の値 |
| label_name | TEXT NULL | action=`add_label` のとき、付けるラベル。振り分けのラベル（routes.label）だけを許す。`ai-ready`（`linear.triggerLabel`）は書けない（本人決定 3） |

ほかの列（id、task_id、linear_issue_id、state_name、dedupe_key、body、status、attempts、next_attempt_at、linear_comment_id、created_at、sent_at、execution_id、outcome_unknown、failure_reason、failed_at）は 16 の 3.7 のまま使う。action=`add_label` の dedupe_key は `<task_id>:label:<decision_id>` とし、outcome_unknown は常に 0 にする（同じラベルを2回付けても結果は変わらないため）。action=`set_state` の dedupe_key は `<task_id>:state_<kind>:<対象の ID>` とする。kind=`started` の対象の ID は、Execution の最初の開始では Execution の ID、本人の判断の後の再開では回答した判断待ちの ID とする（16 の 3.6.3、Q100）。同じ Execution が判断の後に何度再開しても、そのたびに In Progress へ戻せるようにするためである。

### 3.8 全体設定（settings）

| 列 | 型 | 説明 |
|---|---|---|
| key | TEXT PK | 設定のキー |
| value | TEXT (JSON) | 値 |
| updated_at | TEXT | |

書いてよいキーは次だけとする。行が無いキーは既定値を使う。未知のキーは API で 400 にする。値の変更は audit_logs（`settings.update`）に残し、settings と同じトランザクションで書く。秘密情報はこの表に入れない。

| キー | 既定値 | 内容 | 出典 |
|---|---|---|---|
| `linear.teamId` | なし（未設定なら照会しない） | Evaloom が照会する Linear Team。1つだけ | 本人決定 2 |
| `linear.triggerLabel` | `ai-ready` | 起動の合図のラベル | 15、Q48 |
| `linear.routingLabelPrefix` | `route:` | 振り分けのラベルの接頭辞。この接頭辞で始まるラベルだけを振り分けのラベルとして数える | Q120 |
| `routing.proposer` | `{ "microHarness": "claude_code", "model": null }` | 振り分けを提案する AI。model が null なら提案を作らず、本人が一覧から選ぶ | Q111 |
| `routing.defaultRunnerId` | なし | スペシャルハーネスへ直接振り分けたタスクと、振り分けの提案を動かす Runner | Q111、Q113 |
| `limits.stageTimeoutMaxSec` | `{ "prepare": 1800, "work": 14400, "check": 3600, "publish": 1800 }` | Profile の `timeoutSec` の上限（契約 10.2） | 案 |
| `approval.ttlSec` | 17 の Q55 の値 | 承認の有効時間 | 17 |

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| Project が持つもの | 名前、振り分けのラベル、頼むスペシャルハーネス、渡す値、承認を省く設定、Runner の指定の6つだけ | vision.md 4章、本人決定 6 |
| 振り分けのキー | ラベルだけ。Linear Team は使わない | 本人決定 2 |
| 振り分けのラベル | 振り分けの表の中で一意。振り分け先は Project かスペシャルハーネスの一方 | 本人決定 2 |
| 段 | `prepare`（任意）→ `work` → `check` → `publish` | 本人決定 7 |
| 本人の判断の場所 | 管理画面だけ（owner_decisions） | 本人決定 3 |
| 確かめたもの＝出すもの | スペシャルハーネスが見張る。Evaloom は照合しない | 本人決定 5 |
| 承認と中身の結び付け | Evaloom が approvals.content_id と出す直前の識別子を比べる | 本人決定 5 |
| Profile の版 | SemVer が正本、commit SHA を記録。公開済みタグの付け替え禁止 | requirements 5.4、契約 4.2 |
| 同時実行の上限 | 全体2本、振り分け先（Project またはスペシャルハーネス）ごとに1本 | requirements 8.7、vision-alignment.md 5章 |
| 自動リトライ | 0回 | requirements 8.3 |
| collaboration_mode | `solo` だけ | requirements 8.2 |
| `users` テーブル | 作らない | requirements 17章 |

### 4.1 段の名前

| 段 | Execution 状態 | 動かすもの | 鍵 | 外への接続 |
|---|---|---|---|---|
| `prepare` | `preparing` | スペシャルハーネスの固定版のプログラム（AI なし）。Profile に宣言が無ければ行わない | 読み取り用だけ | 宣言した宛先だけ |
| `work` | `working` | ミクロハーネス | 無し | モデルの API だけ |
| `check` | `checking` | スペシャルハーネスの固定版のプログラム（AI なし） | 無し | 無し |
| `publish` | `publishing` | スペシャルハーネスの固定版のプログラム（AI なし） | 宣言したもの | 宣言した宛先だけ |

振り分けの提案（route_proposals）は段ではない。Execution を持たず、`proposal` の隔離（[`01-architecture.md`](./01-architecture.md) 3.1）で動く。

### 4.2 ミクロハーネスの列挙値

| 値 | 対象 |
|---|---|
| `claude_code` | Claude Code CLI |
| `codex` | Codex CLI |

### 4.3 契約の版

Evaloom が対応する契約の版は `"1"` だけとする（契約 3章）。

## 5. インターフェース（列挙値）

要求 JSON、結果 JSON、finding、Profile 定義のスキーマは契約（6.1、6.5、14章、5章）を参照する。この章は Evaloom の内部の列挙値を定める。

### 5.1 タスク状態（tasks.status）

| 値 | 表示名 | 意味 |
|---|---|---|
| `received` | Received | 作成直後。振り分け前 |
| `routing` | Routing | 振り分け先が決まらず、提案と本人の承認を待つ（route_proposals） |
| `queued` | Queued | Execution が claim を待つ |
| `running` | Running | Execution が段のどれかを動かしている（`claimed` / `preparing` / `working` / `checking` / `publishing`） |
| `waiting_approval` | Waiting Approval | 操作の承認を待つ |
| `blocked` | Blocked | 本人の判断（続行・回答）を待つ。または取り込みの保留（intake_block_cause、Execution なし） |
| `completed` | Completed | 出す段まで成功した |
| `failed` | Failed | 失敗 |
| `cancelled` | Cancelled | 中止 |
| `timed_out` | Timed Out | タイムアウト |

requirements 8.3 の `planning` と `verifying` は使わない。段の細かい状態は Execution 状態で表す（Q119）。

### 5.2 Execution 状態（executions.status）

| 値 | 意味 | タスク状態 | 同時実行の枠 |
|---|---|---|---|
| `pending` | claim 待ち | `queued` | 数えない |
| `claimed` | Runner が取得し、版を固定した | `running` | 数える |
| `preparing` | 準備の段 | `running` | 数える |
| `working` | 作業の段 | `running` | 数える |
| `checking` | 確かめる段 | `running` | 数える |
| `publishing` | 出す段 | `running` | 数える |
| `waiting_approval` | 承認待ち（段のプロセスは終了） | `waiting_approval` | 数えない |
| `blocked` | 本人の判断待ち（続行・回答） | `blocked` | 数えない |
| `succeeded` | 出す段まで成功 | `completed` | 終端 |
| `failed` | 失敗 | `failed` | 終端 |
| `interrupted` | 再起動で中断（失敗の一種） | `failed` | 終端 |
| `cancelled` | 中止 | `cancelled` | 終端 |
| `timed_out` | タイムアウト | `timed_out` | 終端 |

主な遷移は次のとおりとする（詳細は [`04-task-lifecycle.md`](./04-task-lifecycle.md)）。

| 現在 | 契機 | 次 |
|---|---|---|
| `pending` | Runner が claim | `claimed` |
| `claimed` | 段を始める | Profile に prepare があれば `preparing`、無ければ `working`。resume_stage があればその段 |
| `preparing` | prepare が `succeeded` | `working` |
| `working` | 作業が終わり `out/question.json` が無い | `checking` |
| `working` | `out/question.json` がある | `blocked`（cause=`question`） |
| `checking` | check が `succeeded` で承認の要る操作が無い、または `needs_approval` で承認の要る操作をすべて省いた | `publishing` |
| `checking` / `publishing` | `needs_approval`（省かない操作がある） | `waiting_approval` |
| `publishing` | `needs_approval` で、承認の要る操作をすべて省いた | `pending`（resume_stage=`publish`。publish の段を新しく始める、[`17-approval.md`](./17-approval.md) 3.3 の8） |
| `checking` | check が `blocked` | `blocked`（cause=`check_blocked`） |
| `publishing` | publish が `unknown` | `blocked`（cause=`publish_unknown`） |
| `publishing` | publish が `succeeded` | `succeeded` |
| `waiting_approval` | 本人がすべて承認 | `pending`（resume_stage=`publish`） |
| `waiting_approval` | 本人が1つでも却下 | `failed`（`approval_rejected`） |
| `blocked` | 本人が続行（`check_blocked`）し、check の結果に省かない承認要求がある | `waiting_approval`（[`17-approval.md`](./17-approval.md) 3.3） |
| `blocked` | 本人が続行（`check_blocked` で承認要求が無い、または `publish_unknown`）または回答（`question`） | `pending`（resume_stage は 5.6 の表） |
| `blocked` / `waiting_approval` | 本人が中止（abort） | `cancelled`。ただし cause=`publish_unknown` の abort は `failed`（`publish_unknown`） |
| 動いている状態 | 失敗・タイムアウト・中止・再起動 | `failed` / `timed_out` / `cancelled` / `interrupted` |

本人の判断の後に Execution を `pending` に戻したときは、同じトランザクションでタスクを `queued` に戻す（5.2 の表の「タスク状態」の列のとおり）。Runner は次の claim で resume_stage の段から再開する（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.6）。

### 5.3 実行イベント種別（execution_events.type）

| 値 | 発生元 | payload の主な項目 |
|---|---|---|
| `status_changed` | Control Plane | `from`, `to`, `reason` |
| `log` | Runner | `sourceId`（Execution 内で一意）, `stream`（`prepare` / `work` / `check` / `publish` / `runner`）, `text`（機密情報除外済み） |
| `stage_started` | Runner | `stage`, `attempt` |
| `stage_finished` | Runner | `stage`, `attempt`, `status`, `errorClass`, `exitCode` |
| `context_pinned` | Control Plane | `revisions`（layer, path, hash の配列） |
| `check_result` | Control Plane | `checkResultId`, `result`, `findingCount`, `contentId` |
| `approval_requested` | Control Plane | `approvalId`, `operation`, `contentId` |
| `approval_waived` | Control Plane | `operation`, `contentId` |
| `approval_consumed` | Control Plane | `approvalId`, `operation`, `allowed`, `reason` |
| `decision_opened` | Control Plane | `decisionId`, `kind`, `cause` |
| `decision_answered` | Control Plane | `decisionId`, `kind`, `action` |
| `published` | Control Plane | `outputs`（`kind`, `label`, `url` の配列） |
| `notification_sent` | Control Plane | `kind`（判断待ちの通知では `decision`、[`12-notification.md`](./12-notification.md) 3.4）, `channel` |
| `notification_failed` | Control Plane | `kind`, `channel`, `reason`（channel=`macos` では `permission_denied` / `launch_failed` / `timeout` / `other`。channel=`linear` では `auth` / `issue_removed` / `retries_exhausted` / `state_not_found` / `label_not_found`（付けるラベルが Linear の Team に無い、16 の Q164）/ `other`） |

worktree_ready、gate_result、pushed、pr_created、harness_started、harness_finished、blocked は使わない。

### 5.4 振り分けの提案の状態（route_proposals.status）

3.4 の表のとおり（`queued` / `running` / `proposed` / `failed` / `decided` / `superseded` / `cancelled`）。

### 5.5 エラー分類（executions.error_class）

契約 13章と同じ値を使う。

| 値 | 意味 |
|---|---|
| `config_error` | 設定の不備（Profile の読み込み失敗、渡す値が形に合わない、要る鍵が無い、タグの付け替えを検出） |
| `prepare_failed` | 準備の段の失敗 |
| `work_failed` | 作業の段の失敗 |
| `check_failed` | 確かめた結果が基準を満たさない |
| `publish_failed` | 出す段の失敗 |
| `publish_unknown` | 出す操作の結果がわからない（本人が中止を選んだとき、この値で終える） |
| `approval_rejected` | 本人が承認を却下した |
| `contract_error` | 結果 JSON が契約に合わない |
| `sandbox_error` | 隔離の作成・実行の失敗 |
| `timeout` | 段の上限時間を超えた |
| `cancelled` | 中止 |
| `interrupted` | 再起動による中断 |

`execution_failed`、`test_failed` は使わない（それぞれ `work_failed`、`check_failed` に置き換える）。

### 5.6 本人の判断の種類・原因・回答

| kind | 契機 | cause | 回答（answer） | 回答の後 |
|---|---|---|---|---|
| `route` | 振り分けのラベルが無い・知らない・2つ以上 | - | `{ "action": "approve", "label": "<routes.label>", "basis": "<提案の ID>" }` / `{ "action": "reject" }` | approve: タスクに振り分け先を書いて `queued`、Linear に振り分けのラベルを付ける（linear_outbox action=`add_label`）。reject: タスクを `cancelled`（Q117） |
| `approval` | 段の結果が `needs_approval` | - | `{ "action": "approve" \| "reject", "contentId": "<basis>", "ttlSec": <整数> }`。`ttlSec` は approve のときだけ書ける任意の項目で、その承認の有効時間（省略したら `approval.ttlSec`。Q200） | 3.4 approvals と 5.2 |
| `blocked` | check が `blocked` | `check_blocked` | `{ "action": "continue" \| "abort", "contentId": "<basis>" }` | continue: 結果に承認要求があれば `waiting_approval`、無ければ `pending`（resume_stage=`publish`） |
| `blocked` | publish が `unknown` | `publish_unknown` | `{ "action": "continue" \| "abort" }` | continue: `pending`（resume_stage=`publish`）。abort: `failed`（`publish_unknown`） |
| `blocked` | 固定した文脈に解決できない矛盾 | `context_conflict` | `{ "action": "abort" }` だけ | `cancelled`。本人は直してから再実行する |
| `blocked` | 再開のときに作業領域が無い（削除済み）、または再開に要る前の段の `out` が Runner に無い（Q136） | `workspace_missing` | `{ "action": "abort" }` だけ | `cancelled`。本人は fresh で再実行する |
| `question` | 作業の段の `out/question.json` | `question` | `{ "action": "answer", "text": "...", "option": "<選択肢>" \| null }` / `{ "action": "abort" }` | answer: `pending`（resume_stage=`work`、契約 8章） |

- `basis` のある判断では、回答に画面で見ていた値を送らせ、行の basis と違えば 422 にする（[`04-task-lifecycle.md`](./04-task-lifecycle.md) の Blocked からの続行と同じ考え方）。
- kind=`route` の label は、判断の時点の routes にあるラベルでなければならない。提案と別のラベルを選べるかは Q112。
- cause=`workspace_missing` は、Runner が段の終了の報告で返す `runnerError: "workspace_missing"`（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.4.1）から作る。`runnerError` の `workspace_missing` は判断待ちの原因であり、5.5 のエラー分類ではない。最初から始める Execution で作業領域が無いときは、判断待ちにせず `failed`（`config_error`）にする。

### 5.7 取り込みの保留の理由（tasks.intake_block_cause）

振り分けが決まらない場合は保留ではなく、振り分けの提案（`routing`）に回す。保留にするのは次の2つだけとする。

| 値 | 意味 | 解除 |
|---|---|---|
| `missing_acceptance_criteria` | 振り分け先の Profile が `required` なのに受け入れ条件が無い | Issue の更新で再判定（15 の 3.6.3） |
| `config_missing_profile` | スペシャルハーネスへ直接振り分けたが default_profile_id が無い | 本人が既定の版を設定したときに再判定 |

`routing_multiple_labels`、`routing_unknown_label`、`routing_team_mismatch`、`routing_no_label`、`config_missing_assignment` は使わない。

### 5.8 linear_outbox の kind

| action | kind | 契機 |
|---|---|---|
| `comment` | `missing_acceptance_criteria` | 5.7 の保留。`config_missing_profile` の保留ではコメントを出さない（Evaloom 側の設定の不備で、Linear で直せることが無いため。[`16-linear-feedback.md`](./16-linear-feedback.md) 3.2） |
| `comment` | `decision_open` | owner_decisions の行を作った（「管理画面で判断待ち」とリンク） |
| `comment` | `succeeded` / `failed` / `cancelled` | Execution の終了（結果の返却、requirements 6.4） |
| `set_state` | `started` / `waiting_decision` / `completed` / `cancelled` | 状態の変更（状態の名前は [`16-linear-feedback.md`](./16-linear-feedback.md) 3.6） |
| `add_label` | `route_approved` | 本人が振り分けを承認した |

## 6. エラー処理

- JSON 列はアプリ側で検証し、スキーマに合わない値を書き込まない。不透明な値（params、content_id、outputs、target）は、契約の形（型と長さ）だけを確かめる。
- harness_profiles の同じ (harness_id, version) への別 SHA の登録は 409 で拒否する。
- routes で project_id と harness_id の両方、または両方 NULL の行は書けない。同じラベル、同じ Project への2つ目のラベルは 409。
- 状態列の更新は、期待する現在値を条件にした UPDATE（`WHERE status = ?`）で行い、競合したら 409 とする。
- approvals の消費が0件の更新になったら、許可しない（Fail Closed）。

## 7. スコープ外

- PostgreSQL への移行（requirements 2.2 の将来候補）
- バックアップ・障害復旧、ログ保存期間（requirements 21章、Q24）
- 評価・改善・協議の表（users、task_steps、evaluation_rubrics、evaluation_runs、evaluation_scores、contracts、dependency_edges、human_feedback、improvement_proposals、experiments、experiment_assignments、collaboration_sessions、collaboration_messages）。requirements 10〜13 章の将来の Phase で作る
- 開発の表（worktrees、test_runs）と開発の列。software-engineer へ移した（本人決定 1）
- project_harnesses（役割ごとの割り当て）、project_integrations、event_inbox、execution_blocks、gate_results。それぞれ projects.harness_id と profile_id、settings、linear_issue_states、owner_decisions、check_results に置き換えた

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約（要求 JSON、結果 JSON、Profile 定義、finding）
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API と認証（もう一つのアンカー）
- [`03-registry.md`](./03-registry.md) projects / harnesses / harness_profiles / routes の登録
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 状態遷移、claim、本人の判断の後の再開
- [`07-context.md`](./07-context.md) context_revisions
- [`15-linear-intake.md`](./15-linear-intake.md) 振り分けと振り分けの提案
- [`16-linear-feedback.md`](./16-linear-feedback.md) linear_outbox
- [`17-approval.md`](./17-approval.md) approvals
- [`18-remote-access.md`](./18-remote-access.md) owner_credentials / owner_sessions
- [`19-routing-and-stages.md`](./19-routing-and-stages.md) 振り分け先・段・安全な場所

## 9. 実装単位

- [ ] [Backend] SQLite 接続（WAL モード）とマイグレーション基盤
  - 受け入れ基準: 起動時に未適用のマイグレーションが順に適用される; `PRAGMA journal_mode` が `wal` を返す
- [ ] [Backend] 登録の表（runners, harnesses, harness_profiles, projects, routes）
  - 受け入れ基準: harness_profiles に UNIQUE(harness_id, version) があり重複登録が失敗する; routes に project_id と harness_id の両方、または両方 NULL の行を書けない; 同じ Project に2つ目のラベルを書けない; projects に6項目以外の列が無い
- [ ] [Backend] タスクと実行の表（tasks, executions, task_workspaces, stage_runs, check_results, execution_events, context_revisions）
  - 受け入れ基準: tasks に project_id と harness_id の両方が入った行を書けない; UNIQUE(task_id, seq) と tasks.idempotency_key の一意制約がある; 5.1 と 5.2 以外の状態値を書き込めない
- [ ] [Backend] 本人の判断の表（owner_decisions, route_proposals, approvals）
  - 受け入れ基準: 同じタスクで open の判断待ち（approval 以外）を2つ作れない; 同じ Execution・同じ operation で pending / approved の承認を2つ作れない; 5.6 以外の kind・cause を書き込めない
- [ ] [Backend] 記録の表（artifacts, harness_records, audit_logs, usage_records）
  - 受け入れ基準: artifacts の実ファイルが Git 管理外のデータディレクトリに置かれる; harness_records の deleted_at が入った行が次の仕事への受け渡しの対象にならない
- [ ] [Backend] Linear の表と settings（linear_poll_teams, linear_issue_states, linear_issue_links, linear_outbox, settings）
  - 受け入れ基準: linear_outbox の action=add_label に routes に無いラベルと `linear.triggerLabel` のラベルを書けない; settings に 3.8 の表に無いキーを書けない
- [ ] [Backend] 列挙値と、契約の要求 JSON・結果 JSON・finding・Profile 定義の Go 型と JSON 検証
  - 受け入れ基準: 契約 5.1・6.1・6.5・14章の例が検証を通る; 未知の項目、未知の errorClass、スペシャルハーネスが書けない errorClass が拒否される

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q111 | 振り分けを提案する AI をどこで、どのミクロハーネスで動かし、同時実行にどう数えるか | 決定（2026-11-10 本人、案を採用）: 全体設定 `routing.defaultRunnerId` の Runner が、鍵なし・モデルの API だけの `proposal` の隔離で動かす。使うミクロハーネスとモデルは全体設定 `routing.proposer`。全体2本の枠に数え、振り分け先ごとの枠には数えない |
| Q112 | 本人が、AI の提案と別の振り分け先を選べるか | 決定（2026-11-10 本人、案を採用）: 選べる。管理画面で、判断の時点の振り分けの表から1つ選ぶ。表に無い振り分け先は選べない（先に振り分けの表へ足す） |
| Q113 | スペシャルハーネスへ直接振り分けたタスクの承認を省く設定と Runner | 決定（2026-11-10 本人、案を変更）: 直接振り分けた仕事の承認を省く設定は、スペシャルハーネスの登録（harnesses.approval_waivers）に持つ。既定は `[]` で、宣言したすべての操作で止まる。本人が、もう止めなくてよい操作を後から足していく。足せるのは既定の版で `waivable: true` と宣言された操作だけ。Runner は全体設定 `routing.defaultRunnerId` |
| Q115 | 渡す値の検証 | 決定（2026-11-10 本人、案を採用）: Profile の `params.schema` で、Project の登録・更新と claim のときに形だけ確かめる |
| Q117 | 本人が振り分けの提案を却下したときのタスク | 決定（2026-11-10 本人、案を採用）: `cancelled` にする。もう一度実行したいときは、Linear で振り分けのラベルを付けるか、`ai-ready` を外して付け直す |
| Q118 | Tool Permissions の置き場所（いまは harnesses.tool_permissions で本人だけが変える） | 決定（2026-11-10 本人、案を採用）: Profile 定義の `stages.work.toolPermissions` に移す。本人は、Project に固定する版を選ぶことで許可を選ぶ（requirements 15.3 の「本人だけが変更できる」を、版の選択で満たす） |
| Q119 | タスク状態から `planning` と `verifying` を外し、`routing` を足すか（requirements 8.3 は書き換え済み） | 決定（2026-11-10 本人、案を採用）: 外して足す。段の状態は Execution 状態で見せる |
| Q120 | 振り分けのラベルの接頭辞の既定 | 決定（2026-11-10 本人）: `route:`。Project にもスペシャルハーネスにも使えるため、`repo:` から変える |
| Q121 | 作業領域の保持期間と削除 | 決定（2026-11-10 本人、案を採用）: 終端になってから 14 日保持し、Runner が削除して cleaned にする。判断待ちと承認待ちの作業領域は削除しない。本人は管理画面から先に消せる |
| Q200 | 承認の有効時間を、本人が承認するときに変えられるか（17 の Q55 は「既定 24 時間、承認時に本人が変えられる」と決めたが、回答の形に入れる項目が無かった） | 決定（2026-11-10 本人）: 変えられるようにする。5.6 の kind=`approval` の回答に任意の `ttlSec`（60 秒以上。上限は設けない）を足す。省略したら `approval.ttlSec` を使う。expires_at は承認した時刻 + `ttlSec` |
