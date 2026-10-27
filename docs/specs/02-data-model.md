---
status: confirmed
範囲: 整合性アンカー。Phase 1 のテーブル、状態の列挙値、共通契約と Harness Profile のスキーマ
---

# 02. データモデル 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: SQLite のテーブル、状態の列挙値、共通契約（ハーネスとの入出力）、Harness Profile 定義

この文書は整合性アンカーである。テーブル・列挙値・JSON スキーマはここだけで定義し、他の詳細仕様はここを参照する。

requirements 17章は「詳細な型、外部キー、制約、インデックスは詳細設計時に確定する」としている。以下の列定義は、requirements の保持情報（5.1〜5.4、7.1、8.3〜8.6）から導いた Phase 1 案である。型・インデックスの最終確定は実装時に行う。

## 1. 目的

- 工程間（Control Plane / Runner / Adapter / 管理画面）で同じ名前・同じ値を使えるようにする。
- 再起動後に未完了の仕事を検出できるよう、状態を永続化する（requirements 8.3）。
- Execution ごとに、使った Profile と Context の版を後から確認できるようにする（requirements 7.1, 8.3）。

## 2. 入出力

- 入力: 各詳細仕様の処理が書き込む行。
- 出力: API のレスポンス（[`01-architecture.md`](./01-architecture.md)）、管理画面の表示。

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
| マイグレーション | Phase ごとに段階的に追加する（requirements 17章）。Phase 1 で作らないテーブルは 3.3 |

ID の例外（提案）:

| 対象 | 形式 | 理由 |
|---|---|---|
| projects.id | 本人が付ける slug（例 `project-a`） | requirements 4.2 の例と、Phase 2 の `repo:*` Label との対応 |
| harnesses.id | 本人が付ける slug（例 `claude-developer`） | requirements 4.2 の例 |
| tasks.id | `task-<連番>`（例 `task-001`） | ブランチ名 `evaloom/<task-id>-<短い名前>` に入るため短くする（requirements 3.2 の例） |

### 3.2 Phase 1 で作るテーブル

17章のテーブルのうち Phase 1 で使うものと、Phase 1 の機能に必要な追加案（「追加」と表記）。

#### runners

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | Runner ID（例 `mac-mini`） |
| name | TEXT | 表示名 |
| token_hash | TEXT | Runner トークンのハッシュ。平文は保存しない |
| worktree_root | TEXT | worktree を作るディレクトリ（requirements 3.3 Worktree Root） |
| status | TEXT | `online` / `offline` |
| last_heartbeat_at | TEXT | 最後の heartbeat |
| boot_id | TEXT NULL | 最後に受信した heartbeat の bootId。未受信・既存DBの移行直後は NULL |
| created_at | TEXT | |

#### projects

requirements 5.1 の保持情報に対応する。

| 列 | 型 | 説明 | 5.1 の項目 |
|---|---|---|---|
| id | TEXT PK | Project ID | Project ID |
| name | TEXT | 名称 | 名称 |
| local_repo_path | TEXT | 絶対パス。登録時に検証（[`03-registry.md`](./03-registry.md)） | Local Repository Path |
| git_remote_url | TEXT | | Git Repository URL |
| runner_id | TEXT FK→runners | | Runner ID |
| base_branch | TEXT | | Base Branch |
| default_role | TEXT | 既定で使う役割名（例 `developer`）。project_harnesses.role を指す | Default Harness |
| review_role | TEXT NULL | Phase 5 まで使わない | Review Harness |
| context_location | TEXT | Phase 1 は `AGENTS.md`（リポジトリ内） | Context Location |
| test_command | TEXT NULL | テストコマンド。未設定ならテストできないため Gate 2 で止まる | テストコマンド |
| test_report_format | TEXT NULL | `junit_xml` など（Q12、追加） | - |
| test_report_path | TEXT NULL | worktree 内のレポート出力パス（Q12、追加） | - |
| setup_command | TEXT NULL | worktree 作成後に Test Sandbox で実行 | セットアップコマンド |
| copy_allowlist | TEXT (JSON 配列) | worktree へコピーしてよいファイルの相対パス | コピー許可リスト |
| test_file_patterns | TEXT (JSON 配列) | Gate 1 で追加検出するパターン（Q10、追加） | - |
| allowed_operations | TEXT (JSON) | 許可操作 | 許可操作 |
| approval_policy | TEXT (JSON) | 承認ポリシー。Phase 1 は保存と表示だけ（Q25） | 承認ポリシー |
| timeouts | TEXT (JSON) | `{ "setupSec", "harnessSec", "testSec" }`（Q15、追加） | - |
| created_at / updated_at | TEXT | | |

Linear Team ID と Linear Routing Label の列は Phase 2 のマイグレーションで追加する。

#### harnesses

requirements 5.2 の保持情報に対応する。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | Harness ID |
| name | TEXT | 名称 |
| specialty | TEXT | 専門分野（Phase 1 は `developer`） |
| supported_tasks | TEXT (JSON 配列) | 対応タスク（Phase 1 は `["development"]`） |
| adapter | TEXT | `codex` / `claude_code`（4.4） |
| repo_path | TEXT | ハーネスリポジトリのローカルパス |
| repo_url | TEXT | ハーネスリポジトリの URL |
| supported_models | TEXT (JSON 配列) | 対応モデルの Model ID |
| available_tools | TEXT (JSON 配列) | 利用可能ツール |
| tool_permissions | TEXT (JSON) | Tool Permissions。本人だけが変更できる |
| contract_version | TEXT | 対応する共通契約の版（5.1） |
| created_at / updated_at | TEXT | |

Harness Version / Prompt Version / Skill Version は harness_profiles 側に持つ（requirements 5.4 で Profile が一体として定義するため）。Evaluation Policy と Improvement History は Phase 3 / 4 で追加する。

#### harness_profiles

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| harness_id | TEXT FK→harnesses | |
| version | TEXT | SemVer（正本）。`UNIQUE(harness_id, version)` |
| commit_sha | TEXT | 実体の Git commit SHA（40桁） |
| tag_name | TEXT | 対応する Git タグ名（Q5） |
| definition | TEXT (JSON) | 登録時に読み込んだ Profile 定義（4.3）の全文 |
| definition_hash | TEXT | definition の SHA-256 |
| model_id | TEXT | 正式な Model ID（表示名ではない、requirements 5.3） |
| operation_status | TEXT | `provisional`（暫定運用）/ `official`（正式運用）。Phase 1 は常に `provisional` |
| registered_at | TEXT | |

一度登録した行は更新しない。同じ版を別の SHA で登録しようとしたら拒否する（タグの付け替え禁止、requirements 5.4）。

#### project_harnesses

| 列 | 型 | 説明 |
|---|---|---|
| project_id | TEXT FK→projects | |
| role | TEXT | 役割名（例 `developer`） |
| harness_id | TEXT FK→harnesses | |
| profile_id | TEXT FK→harness_profiles | 固定する版 |
| assigned_at | TEXT | |
| PK | (project_id, role) | |

割り当ての変更履歴は audit_logs に残す。どの Execution がどの版を使ったかは executions に残す（requirements 12.6）。

#### tasks

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | `task-<連番>` |
| project_id | TEXT FK→projects | |
| title | TEXT | 件名 |
| body | TEXT | 本文（Issue 本文に相当） |
| acceptance_criteria | TEXT | 受け入れ条件。空文字は不可（requirements 8.1） |
| short_name | TEXT | ブランチ名用の短い名前（英小文字・数字・ハイフン） |
| task_type | TEXT | Phase 1 は `development` のみ |
| collaboration_mode | TEXT | Phase 1 は `solo` のみ |
| status | TEXT | 5.1 のタスク状態 |
| idempotency_key | TEXT UNIQUE | 二重送信防止（[`04-task-lifecycle.md`](./04-task-lifecycle.md)） |
| created_at / updated_at | TEXT | |

#### executions

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | ULID |
| task_id | TEXT FK→tasks | |
| seq | INTEGER | タスク内の通し番号（1, 2, ...）。`UNIQUE(task_id, seq)` |
| mode | TEXT | `continue`（前回のブランチ・worktree・PR を引き継ぐ）/ `fresh`（最初から） |
| status | TEXT | 5.2 の Execution 状態 |
| runner_id | TEXT NULL | claim した Runner |
| role | TEXT NULL | 使った役割（claim 時に固定） |
| harness_id | TEXT NULL | claim 時に固定 |
| profile_id | TEXT NULL | claim 時に固定 |
| profile_version | TEXT NULL | claim 時に固定（表示用の重複保持） |
| profile_commit_sha | TEXT NULL | claim 時に固定 |
| worktree_id | TEXT NULL FK→worktrees | |
| branch_name | TEXT NULL | |
| base_sha | TEXT NULL | 開始点 SHA（requirements 8.6） |
| gate1_base_sha | TEXT NULL | Gate 1 の差分検査の基準（[`10-verification-gates.md`](./10-verification-gates.md) Q13） |
| resume_point | TEXT NULL | Blocked から続行したときに再開する工程（`gate2` / `gate3` / `publish`）。[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.8 |
| harness_head_sha | TEXT NULL | ハーネス終了後に Runner が記録した HEAD |
| verified_sha | TEXT NULL | Gate 2 でテストした commit |
| pushed_sha | TEXT NULL | push した commit。verified_sha と一致しなければならない |
| pr_number | INTEGER NULL | |
| pr_url | TEXT NULL | |
| harness_session_id | TEXT NULL | Adapter が返す実行セッション識別子 |
| error_class | TEXT NULL | 5.4 のエラー分類 |
| error_message | TEXT NULL | 機密情報を除いた説明 |
| cancel_requested_at | TEXT NULL | 中止の要求時刻 |
| claimed_at / started_at / finished_at | TEXT NULL | |
| last_heartbeat_at | TEXT NULL | Runner の最終報告 |
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

Execution ごとに固定した Context を保存する（requirements 7.1, 8.3）。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK→executions | |
| layer | TEXT | `global` / `harness` / `project` / `task` |
| kind | TEXT | `agents_md` / `claude_md` / `harness_config` / `harness_prompt` / `global_policy` / `task_input` など |
| source_path | TEXT | リポジトリ内の相対パス（サブディレクトリの `AGENTS.md` を含む） |
| source_commit_sha | TEXT | 読み取り元の commit |
| content_hash | TEXT | SHA-256 |
| content | TEXT | 内容 |
| created_at | TEXT | |

#### worktrees

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| project_id | TEXT FK | |
| task_id | TEXT FK | |
| runner_id | TEXT FK | |
| path | TEXT | 絶対パス（Worktree Root 配下） |
| branch_name | TEXT | |
| base_sha | TEXT | 開始点 SHA |
| status | TEXT | `active` / `retained`（中断・失敗・Blocked で保持中）/ `cleaned` |
| setup_status | TEXT | `pending` / `succeeded` / `failed`（[`06-worktree.md`](./06-worktree.md) 3.4） |
| copied_files | TEXT (JSON 配列) | コピー許可リストで置いたファイルの相対パスと SHA-256（[`06-worktree.md`](./06-worktree.md) 3.5） |
| created_at / cleaned_at | TEXT | |

#### test_runs

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK | |
| kind | TEXT | `setup` / `baseline` / `verify` |
| target_sha | TEXT | 実行対象の commit |
| command | TEXT | 実行したコマンド（Project Registry から読んだもの） |
| exit_code | INTEGER NULL | |
| total / passed / failed / skipped | INTEGER NULL | レポートから得た件数 |
| test_ids_artifact_id | TEXT NULL FK→artifacts | 実行したテスト ID の一覧 |
| timed_out | INTEGER | 0 / 1 |
| started_at / finished_at | TEXT | |

#### gate_results（追加）

17章に無い。Gate 1〜3 の判定を構造化して残すために追加する。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK | |
| gate | INTEGER | 1 / 2 / 3 |
| result | TEXT | `pass` / `blocked` / `failed` |
| base_sha / target_sha | TEXT | 比較した commit |
| findings | TEXT (JSON 配列) | 5.5 の Finding |
| created_at | TEXT | |

#### execution_blocks（追加）

17章に無い。Blocked の原因と、本人の続行・中止の判断を残すために追加する。

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK | |
| cause | TEXT | 5.6 の Block 原因 |
| gate_result_id | TEXT NULL FK | Gate による Block のとき |
| head_sha | TEXT | Block した時点の HEAD |
| detail | TEXT (JSON) | |
| decision | TEXT NULL | `continue` / `abort`。未判断は NULL |
| decided_at | TEXT NULL | |
| created_at | TEXT | |

#### artifacts

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK | |
| kind | TEXT | `harness_log` / `test_log` / `setup_log` / `diff` / `test_report` / `test_ids` / `harness_output` |
| storage_path | TEXT | Evaloom のデータディレクトリ内のパス（Git には入れない、requirements 3.1） |
| sha256 | TEXT | |
| size_bytes | INTEGER | |
| redacted | INTEGER | 機密情報の除外処理を通したか（0 / 1） |
| created_at | TEXT | |

#### audit_logs

| 列 | 型 | 説明 |
|---|---|---|
| id | INTEGER PK AUTOINCREMENT | |
| actor | TEXT | `owner` / `runner:<id>` / `system` |
| action | TEXT | 例 `project.update`、`profile.register`、`task.cancel`、`auth.reject` |
| target_type / target_id | TEXT | |
| detail | TEXT (JSON) | 変更前後の値など。秘密情報は含めない |
| created_at | TEXT | |

#### usage_records

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | |
| execution_id | TEXT FK | |
| input_tokens / output_tokens | INTEGER NULL | ハーネスから取れる場合だけ |
| wall_time_ms | INTEGER | 実行時間 |
| cost_amount | REAL NULL | API 利用時だけ（requirements 12.5） |
| created_at | TEXT | |

### 3.3 Phase 1 で作らないテーブル

| テーブル | 作る Phase | 理由 |
|---|---|---|
| users | 複数ユーザー対応時 | requirements 17章 |
| task_steps | 未定 | Phase 1 では使わない（requirements 8.2） |
| project_integrations, linear_issue_links, event_inbox | 2 | Linear 連携 |
| approvals | 2 | 承認 |
| evaluation_rubrics, evaluation_runs, evaluation_scores, contracts, dependency_edges, human_feedback | 3〜4 | 評価・中央テスト |
| improvement_proposals, experiments, experiment_assignments | 4 | 自己改善 |
| collaboration_sessions, collaboration_messages | 5 | AI 協議 |
| models | 未定（Q19） | Phase 1 ロードマップに Model Registry が無い。Profile が Model ID を文字列で持つ |

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| Profile の版 | SemVer が正本、commit SHA を記録 | requirements 5.4 |
| 公開済みタグの付け替え | 禁止（同じ版の再登録を拒否） | requirements 5.4 |
| Phase 1 の Profile 状態 | すべて `provisional` | requirements 11.1 |
| `users` テーブル | 作らない | requirements 17章 |
| `task_steps` | 使わない | requirements 8.2 |
| task_type | `development` のみ | requirements 8.1 |
| collaboration_mode | `solo` のみ | requirements 8.2 |

### 4.1 Adapter の列挙値

| 値 | 対象 |
|---|---|
| `codex` | Codex CLI |
| `claude_code` | Claude Code CLI |

### 4.2 共通契約の版

Phase 1 の共通契約は `contractVersion: "1"` とする。厳密な定義は 5.7 / 5.8 を Phase 1 案とし、確定は実装時に行う（Q6）。

### 4.3 Harness Profile 定義ファイル（Q5、提案）

requirements 5.4 の構成を定義ファイルにしたもの。置き場所と読み込み方式は 21章で詳細設計事項とされているため、次は提案である。

- 置き場所: ハーネスリポジトリの `evaloom/profile.yaml`
- 版: ハーネスリポジトリの Git タグ `profile/v<SemVer>`（例 `profile/v1.2.0`）
- 読み込み: 登録時にタグを commit SHA に解決し、その commit の `evaloom/profile.yaml` を読んで harness_profiles.definition に保存する。実行時はこの保存内容と commit SHA を使い、タグを読み直さない。

```yaml
profileVersion: "1.2.0"          # タグと一致しなければ登録を拒否
harnessVersion: "1.2.0"
modelId: "<正式な Model ID>"
promptVersion: "2025-xx"         # ハーネスリポジトリ内のプロンプトの版
skillVersions:
  - { name: "<skill>", version: "<版>" }
toolConfiguration:
  allowedTools: ["..."]
contextPolicyVersion: "1"
executionConfiguration:
  adapter: "claude_code"         # harnesses.adapter と一致しなければ登録を拒否
  args: []                       # Adapter に渡す追加引数（Adapter が許可した項目だけ）
collaborationPolicy: "solo"
evaluationPolicyVersion: null    # Phase 3 まで null（暫定運用）
```

## 5. インターフェース（列挙値・スキーマ）

### 5.1 タスク状態（tasks.status）

requirements 8.3 の11状態。値は snake_case にする。

| 値 | 表示名 | Phase 1 で使うか |
|---|---|---|
| `received` | Received | 使う（作成直後、検証前） |
| `planning` | Planning | 使わない（タスク分類を行わないため） |
| `queued` | Queued | 使う |
| `running` | Running | 使う |
| `verifying` | Verifying | 使う |
| `waiting_approval` | Waiting Approval | 使わない（承認は Phase 2） |
| `completed` | Completed | 使う |
| `failed` | Failed | 使う |
| `cancelled` | Cancelled | 使う |
| `timed_out` | Timed Out | 使う |
| `blocked` | Blocked | 使う |

遷移は [`04-task-lifecycle.md`](./04-task-lifecycle.md) で定める。

### 5.2 Execution 状態（executions.status）

| 値 | 意味 | 対応するタスク状態 |
|---|---|---|
| `pending` | claim 待ち | `queued` |
| `claimed` | Runner が取得し、版を固定した | `running` |
| `preparing` | worktree 準備・setup 中 | `running` |
| `running` | ハーネス実行中 | `running` |
| `verifying` | Gate 1〜3 | `verifying` |
| `publishing` | push・PR 作成中 | `verifying` |
| `succeeded` | PR 作成まで完了 | `completed` |
| `blocked` | 本人の確認待ち | `blocked` |
| `failed` | 失敗 | `failed` |
| `interrupted` | 再起動で中断（失敗の一種） | `failed` |
| `cancelled` | 中止 | `cancelled` |
| `timed_out` | タイムアウト | `timed_out` |

終端状態: `succeeded` / `failed` / `interrupted` / `cancelled` / `timed_out`。`blocked` は本人の判断で `pending`（resume_point 付きで再 claim を待つ）に戻るか `cancelled` になる（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.8）。

### 5.3 実行イベント種別（execution_events.type）

| 値 | 発生元 | payload の主な項目 |
|---|---|---|
| `status_changed` | Control Plane | `from`, `to`, `reason` |
| `log` | Runner | `stream`（`harness` / `setup` / `test` / `runner`）, `text`（機密情報除外済み） |
| `worktree_ready` | Runner | `path`, `branch`, `baseSha` |
| `context_pinned` | Runner | `revisions`（layer, path, hash の配列） |
| `harness_started` / `harness_finished` | Runner | `sessionId`, `headSha`, `errorClass` |
| `gate_result` | Runner | `gate`, `result`, `findingCount` |
| `blocked` | Control Plane | `cause`, `blockId` |
| `pushed` | Runner | `sha`, `branch` |
| `pr_created` | Runner | `prNumber`, `prUrl` |
| `notification_sent` | Control Plane | `kind`, `channel` |
| `notification_failed` | Control Plane | `kind`, `channel`, `reason`（`permission_denied` / `launch_failed` / `timeout` / `other`。[`12-notification.md`](./12-notification.md) 3.4） |

### 5.4 エラー分類（executions.error_class）

requirements 9.5 の5分類に、Phase 1 の Runner 側で必要な分類を足す。

| 値 | 意味 | 出典 |
|---|---|---|
| `config_error` | 設定不備（テストコマンド未設定、パス不正、Profile 読み込み失敗など） | 9.5 |
| `execution_failed` | ハーネスの実行失敗 | 9.5 |
| `test_failed` | テスト失敗 | 9.5 |
| `timeout` | タイムアウト | 9.5 |
| `cancelled` | 中止 | 9.5 |
| `interrupted` | 再起動による中断 | 8.3（追加） |
| `publish_failed` | push・PR 作成の失敗 | 追加 |
| `sandbox_error` | Sandbox の作成・実行の失敗 | 追加 |

### 5.5 Finding（gate_results.findings の要素）

```json
{
  "rule": "existing_test_deleted",
  "path": "src/foo.test.ts",
  "line": 42,
  "message": "既存のテストファイルが削除された"
}
```

`rule` の値は [`10-verification-gates.md`](./10-verification-gates.md) で定める。

### 5.6 Block 原因（execution_blocks.cause）

| 値 | 意味 | 出典 |
|---|---|---|
| `gate1_test_change` | Gate 1 でテスト・テスト設定・CI の変更を検出 | 8.5 |
| `gate1_unverifiable` | Gate 1 の差分を取得・解析できない | 8.5 |
| `gate2_missing_tests` | Gate 2 で既存テストの欠けを検出 | 8.5 |
| `gate2_unverifiable` | テストの件数・ID が取れず検証できない | 8.5 |
| `gate3_mismatch` | 検証 SHA と HEAD の不一致、未コミットの変更 | 8.5 |
| `context_conflict` | 解決できない Context の矛盾 | 7.3 |
| `git_state` | 再実行前の Git 状態が想定と違う、PR がクローズ済み等 | 8.5 |

### 5.7 共通契約：HarnessRequest（Adapter への入力、Q6 案）

requirements 9.5 の入力「Issue 本文、受け入れ条件、対象プロジェクト、使用する Harness Profile」に対応する。

```json
{
  "contractVersion": "1",
  "executionId": "01J...",
  "task": {
    "id": "task-001",
    "title": "...",
    "body": "...",
    "acceptanceCriteria": "..."
  },
  "project": {
    "id": "project-a",
    "workdir": "<Sandbox 内から見た worktree のパス>",
    "branch": "evaloom/task-001-add-login",
    "baseSha": "<40桁>"
  },
  "profile": {
    "harnessId": "claude-developer",
    "version": "1.2.0",
    "commitSha": "<40桁>",
    "modelId": "<正式な Model ID>",
    "definition": { "...": "4.3 の内容" }
  },
  "context": {
    "instructions": "<Context Builder が整形した指示文>",
    "revisionIds": ["..."]
  },
  "limits": { "timeoutSec": 0 }
}
```

### 5.8 共通契約：HarnessResult（Adapter からの出力、Q6 案）

requirements 9.5 の出力「ステータス、変更差分、PR URL、ログ、成果物」に対応する。PR URL は Phase 1 ではハーネスではなく Git Publisher が作るため、Runner が後から埋める。

```json
{
  "contractVersion": "1",
  "status": "succeeded",
  "errorClass": null,
  "sessionId": "...",
  "headSha": "<40桁>",
  "commits": ["<40桁>"],
  "diffArtifactId": "...",
  "summary": "作業概要",
  "logArtifactIds": ["..."],
  "artifactIds": ["..."],
  "usage": { "inputTokens": null, "outputTokens": null, "wallTimeMs": 0 },
  "prUrl": null
}
```

`status` は `succeeded` / `failed`。`errorClass` は 5.4 の値。

## 6. エラー処理

- JSON 列はアプリ側で検証し、スキーマに合わない値を書き込まない。
- harness_profiles の同じ (harness_id, version) への別 SHA の登録は 409 で拒否する。
- 状態列の更新は、期待する現在値を条件にした UPDATE（`WHERE status = ?`）で行い、競合したら 409 とする。

## 7. スコープ外

- PostgreSQL への移行（requirements 2.2 の将来候補）
- バックアップ・障害復旧、ログ保存期間（21章、Q24）
- 3.3 のテーブル

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API と認証（もう一つのアンカー）
- [`03-registry.md`](./03-registry.md) projects / harnesses / harness_profiles / project_harnesses の登録
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 状態遷移
- [`07-context.md`](./07-context.md) context_revisions
- [`08-harness-adapter.md`](./08-harness-adapter.md) 共通契約
- [`10-verification-gates.md`](./10-verification-gates.md) gate_results / execution_blocks

## 9. 実装単位

- [ ] [Backend] SQLite 接続（WAL モード）とマイグレーション基盤
  - 受け入れ基準: 起動時に未適用のマイグレーションが順に適用される; `PRAGMA journal_mode` が `wal` を返す
- [ ] [Backend] Registry 系テーブル（runners, projects, harnesses, harness_profiles, project_harnesses）
  - 受け入れ基準: harness_profiles に UNIQUE(harness_id, version) があり重複登録が失敗する; 外部キー制約が有効
- [ ] [Backend] Task 系テーブル（tasks, executions, execution_events, worktrees, context_revisions）
  - 受け入れ基準: UNIQUE(task_id, seq) と tasks.idempotency_key の一意制約がある; 5.1 と 5.2 以外の状態値を書き込めない
- [ ] [Backend] 検証・記録系テーブル（test_runs, gate_results, execution_blocks, artifacts, audit_logs, usage_records）
  - 受け入れ基準: 各テーブルにテスト用の行を書き込み・読み出しできる; artifacts の実ファイルが Git 管理外のデータディレクトリに置かれる
- [ ] [Backend] 列挙値と共通契約（HarnessRequest / HarnessResult）の Go 型と JSON 検証
  - 受け入れ基準: 5.7 と 5.8 の例が検証を通る; 未知の errorClass や必須項目の欠けた JSON が検証で拒否される
