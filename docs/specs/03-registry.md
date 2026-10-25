---
status: confirmed
範囲: Project / Harness / Harness Profile の登録、パス検証、版の固定、プロジェクトへの割り当て
---

# 03. Registry 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Project Registry、Harness Registry、Harness Profile の版の登録と固定、Project と Harness Profile の紐付け

## 1. 目的

- 複数のローカル Git プロジェクトを、任意のディレクトリに置いたまま登録できるようにする（requirements 20章 条件1〜3）。
- 1つのハーネスを複数プロジェクトで共有し、プロジェクトごとに Profile の版を固定できるようにする（条件4, 5）。
- AI に実行先パスを推測させない。実行先は Registry に登録・検証済みのパスだけとする（requirements 3.3）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | 管理画面からの登録・更新（本人用 API）、ハーネスリポジトリの Git タグと Profile 定義ファイル |
| 出力 | projects / harnesses / harness_profiles / project_harnesses の行（[`02-data-model.md`](./02-data-model.md) 3.2）、audit_logs |

## 3. 処理詳細

### 3.1 Project の登録・更新

1. 本人が管理画面から Project ID、名称、Local Repository Path、Git Repository URL、Runner ID、Base Branch、テストコマンド、セットアップコマンド、コピー許可リスト等を入力する。
2. Control Plane が入力を検証する（3.2）。
3. 検証に通ったら projects に保存し、audit_logs に記録する。
4. 更新（PATCH）も同じ検証を通す。テストコマンド・許可リスト・権限設定の変更は、変更前後の値を audit_logs に残す。

### 3.2 パスとリポジトリの検証（requirements 3.3）

Registry へパスを登録する際に次を確かめる。1つでも失敗したら登録しない（Fail Closed）。

| 確認 | 方法（案） |
|---|---|
| 存在確認 | 絶対パスであること、ディレクトリが存在すること |
| 正規化 | シンボリックリンクを解決した実パスで保存する |
| アクセス権限 | Runner の実行ユーザーが読み取れること |
| Git リポジトリの妥当性 | `git -C <path> rev-parse --show-toplevel` が同じパスを返すこと |
| リモートの一致 | 登録する Git Repository URL が `origin`（またはリモート設定）と一致すること |
| Base Branch | リモートに Base Branch が存在すること |
| 重複 | 同じ実パスを別の Project として登録していないこと |
| worktree との衝突 | パスが Runner の Worktree Root 配下ではないこと |

確認の実行主体: パスは Runner のマシン上にあるため、Runner 側での確認が必要になる。Phase 1 は Control Plane と Runner が同じ Mac mini にあるため Control Plane が直接確認する案とし、Runner を分けるとき（Phase 6）に Runner 経由の確認へ移す。この判断は実装時に確定する。

確認は登録時だけでなく、Runner が worktree を作る直前にも行う（[`06-worktree.md`](./06-worktree.md)）。

### 3.3 コピー許可リスト

- worktree へコピーしてよいファイルを、リポジトリルートからの相対パスで列挙する（requirements 5.1）。
- `..` を含むパス、絶対パス、ワイルドカードは受け付けない（案）。
- 許可リストには秘密情報を含まないファイルだけを載せる（requirements 8.6）。Control Plane は登録時に、ファイル名が `.env`、`.env.*`、`*.pem`、`*.key`、`id_*` に一致するものに警告を出す（案）。拒否まではしない。値が必要ならテスト専用のダミー値を使う運用を画面に表示する。

### 3.4 Harness の登録・更新

1. 本人が Harness ID、名称、専門分野、Adapter（`codex` / `claude_code`）、ハーネスリポジトリのパスと URL、対応モデル、利用可能ツール、Tool Permissions を入力する。
2. リポジトリのパスは 3.2 と同じ検証を通す。
3. Adapter は登録済みの Adapter 一覧（[`08-harness-adapter.md`](./08-harness-adapter.md)）に存在する値だけを受け付ける。

Tool Permissions は本人だけが管理画面から変更できる。Runner とハーネスには変更する手段を与えない（requirements 15.3）。

### 3.5 Harness Profile の版の登録（requirements 5.4）

1. 本人が Harness と版（SemVer）を指定して登録を要求する。
2. Control Plane（Phase 1 は同じマシン上で直接）がハーネスリポジトリで、版に対応するタグ（Q5 案では `profile/v<SemVer>`）を commit SHA に解決する。
3. その commit の Profile 定義ファイルを読み、[`02-data-model.md`](./02-data-model.md) 4.3 のスキーマで検証する。
4. 次のどれかに当たれば拒否する。
   - 版が SemVer でない
   - 定義ファイル内の `profileVersion` がタグの版と一致しない
   - `executionConfiguration.adapter` が Harness の Adapter と一致しない
   - 同じ (Harness, 版) がすでに別の commit SHA で登録されている（タグの付け替え禁止）
5. 定義全文・定義のハッシュ・commit SHA を harness_profiles に保存する。状態は `provisional`（暫定運用）とする。

同じ (Harness, 版) を同じ SHA で再登録した場合は、何もせず既存の行を返す。

登録済みのタグがのちに別の commit を指すようになったことを検出したら（再登録時、または Execution 時の確認で）、その版での実行を止め、`config_error` とする。Execution では harness_profiles に保存した commit SHA を使い、タグは読み直さない。

### 3.6 Project への割り当て（requirements 4.2, 4.3）

1. 本人が Project、役割名（例 `developer`）、Harness、Profile の版を指定する（`POST /api/projects/{id}/harnesses`）。
2. Profile の版が harness_profiles に登録済みであることを確かめる。
3. project_harnesses に (project_id, role) で保存する。すでにあれば版を差し替える。差し替え前後を audit_logs に残す。
4. Project の `default_role` が指す役割が project_harnesses に無いと、その Project のタスクは実行できない（[`04-task-lifecycle.md`](./04-task-lifecycle.md) でタスク作成時に検証する）。

- 新しい版を登録しても、既存の割り当ては自動では変わらない（requirements 3.4, 4.3）。
- 割り当ての変更は、実行中の Execution には影響しない。版は claim 時に Execution に固定する（requirements 8.3）。
- 1つの Harness を複数 Project に割り当てられる。Project ごとに違う版を固定できる（requirements 4.3 の表）。

### 3.7 Phase 1 の権限設定（Q25）

requirements 15.3 により、Phase 1 でも本人は管理画面から許可操作・承認ポリシー・Tool Permissions を変更できる。承認の仕組みは Phase 2 のため、Phase 1 での扱いを次のとおりとする（案）。

| 設定 | Phase 1 での効果 |
|---|---|
| 許可操作（projects.allowed_operations） | 保存と表示。requirements 15.3 の承認対象操作は、この設定にかかわらず Runner が実行しない（[`09-sandbox.md`](./09-sandbox.md)） |
| 承認ポリシー（projects.approval_policy） | 保存と表示のみ。Phase 2 で使う |
| Tool Permissions（harnesses.tool_permissions） | Adapter がハーネスの起動設定に反映する（[`08-harness-adapter.md`](./08-harness-adapter.md)） |

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| Profile の版 | SemVer が正本。commit SHA を記録 | requirements 5.4 |
| 公開済みタグの付け替え | 禁止 | requirements 5.4 |
| Phase 1 の Profile 状態 | 暫定運用（`provisional`） | requirements 11.1 |
| 新しい版の反映 | 既存の割り当てへ自動反映しない | requirements 3.4, 4.3, 4.4 |
| テストコマンドの正本 | Project Registry。ハーネスは変更できない | requirements 8.5 |
| worktree へのコピー | 許可リストのみ。`.env` 等を無条件にコピーしない | requirements 5.1 |
| 実行先パス | Registry に登録・検証済みのものだけ。AI に推測させない | requirements 3.3 |
| 配置場所 | 強制しない。既存プロジェクトの移動は不要 | requirements 3.2 |
| ハーネスのソースをプロジェクトにコピーしない | 参照関係は Registry で管理 | requirements 3.4 |

## 5. インターフェース

[`01-architecture.md`](./01-architecture.md) 5.1 の次のエンドポイント。

- `GET /api/projects`, `POST /api/projects`, `GET /api/projects/{id}`, `PATCH /api/projects/{id}`
- `GET /api/harnesses`, `POST /api/harnesses`, `PATCH /api/harnesses/{id}`
- `GET /api/harness-profiles`, `POST /api/harness-profiles`
- `POST /api/projects/{id}/harnesses`

テーブルは [`02-data-model.md`](./02-data-model.md) 3.2 の projects / harnesses / harness_profiles / project_harnesses / runners。

## 6. エラー処理

| 状況 | 応答 |
|---|---|
| パスが存在しない、Git リポジトリでない、リモート不一致 | 422 `precondition_failed`。`details` にどの確認で失敗したかを入れる |
| 同じ実パスの重複登録 | 409 `conflict` |
| Profile 定義ファイルが無い、スキーマ不一致、版の不一致 | 422 `precondition_failed` |
| 同じ版を別 SHA で登録 | 409 `conflict`。audit_logs に記録する |
| 割り当てで未登録の版を指定 | 422 `precondition_failed` |

## 7. スコープ外

- Model Registry（requirements 5.3）。Phase 1 は Profile が Model ID を文字列で持つ（Q19）
- Linear Team ID / Routing Label の登録（Phase 2）
- Profile の改善候補管理、ロールバック API、影響プロジェクトの検出（Phase 4、requirements 4.4）
- Evaloom 側での Project Context の管理（requirements 7.1）
- Review Harness の利用（Phase 5）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API 一覧
- [`02-data-model.md`](./02-data-model.md) テーブル定義と Profile 定義ファイル
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) claim 時の版の固定
- [`06-worktree.md`](./06-worktree.md) コピー許可リストとセットアップコマンドの使い方
- [`08-harness-adapter.md`](./08-harness-adapter.md) Adapter の一覧と Tool Permissions の反映
- [`10-verification-gates.md`](./10-verification-gates.md) テストコマンドとレポート形式の使い方
- [`13-management-ui.md`](./13-management-ui.md) Projects / Harnesses 画面

## 9. 実装単位

- [ ] [Backend] パスとリポジトリの検証ロジック（3.2）
  - 受け入れ基準: 存在しないパス、Git リポジトリでないパス、リモート URL 不一致、Worktree Root 配下のパスがそれぞれ別の理由で拒否される; シンボリックリンクは実パスで保存される
- [ ] [Backend] Project の登録・取得・更新 API
  - 受け入れ基準: 異なるディレクトリに置いた2つのリポジトリを登録できる; 同じ実パスの二重登録が 409 になる; 更新で変更前後の値が audit_logs に残る
- [ ] [Backend] コピー許可リストの入力検証
  - 受け入れ基準: `..` を含むパスと絶対パスが拒否される; `.env` を含む許可リストに警告が返る
- [ ] [Backend] Harness の登録・更新 API
  - 受け入れ基準: 未登録の Adapter 名が拒否される; Tool Permissions の変更が audit_logs に残る
- [ ] [Backend] Harness Profile の版の登録（タグ解決、定義ファイルの読み込みと検証、SHA 記録）
  - 受け入れ基準: SemVer でない版、定義内の版の不一致、Adapter の不一致が拒否される; 同じ版を別 SHA で登録すると 409; 同じ SHA の再登録は既存行を返す; 登録された行の operation_status が provisional
- [ ] [Backend] Project への Profile 割り当て API
  - 受け入れ基準: 1つの Harness を2つの Project に別の版で割り当てられる; 新しい版を登録しても既存の割り当てが変わらない; 未登録の版の指定が 422 になる
