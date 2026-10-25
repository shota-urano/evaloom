---
status: confirmed
範囲: 4階層 Context の Phase 1 での置き場所と固定方法、Context Builder、AGENTS.md とハーネス設定の固定
---

# 07. Context 管理 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Global / Harness / Project / Task Context の分離と、Execution ごとの固定、Execution Context の生成

## 1. 目的

- コンテキストを Global / Harness / Project / Task の4階層に分けて扱う（requirements 7.1、条件8）。
- Execution ごとに、使った Context の内容とハッシュを記録し、後から確認できるようにする（requirements 7.1, 8.3）。
- すべての Context を毎回渡さず、必要な情報だけを選んで渡す（requirements 7.2）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | Global Context のファイル、Harness Profile（定義と commit SHA）、worktree の開始点 commit に含まれる `AGENTS.md` 等、タスクの入力 |
| 出力 | context_revisions の行（[`02-data-model.md`](./02-data-model.md)）、Execution Context（HarnessRequest の `context`、[`02-data-model.md`](./02-data-model.md) 5.7） |

## 3. 処理詳細

### 3.1 4階層の Phase 1 での置き場所

| 階層 | Phase 1 の正本 | 固定の単位 | 状態 |
|---|---|---|---|
| Global | evaloom リポジトリの `context/` 配下のファイル（requirements 3.2 のディレクトリ構成） | ファイルごとの内容とハッシュ | 案（Q20） |
| Harness | ハーネスリポジトリ内のプロンプト・Skills・設定。Profile の commit SHA の時点のもの | Profile の commit SHA と、読み込んだファイルのハッシュ | 案（Q20） |
| Project | 各プロジェクトリポジトリの `AGENTS.md`（ルートとサブディレクトリ） | worktree の開始点 commit に含まれるもの | 確定（requirements 7.1） |
| Task | タスクの title / body / acceptanceCriteria | タスク作成時の値 | 確定（requirements 7.1） |

Evaloom 側で Project Context を管理する仕組みは後の Phase で追加する（requirements 7.1）。

### 3.2 Project Context の固定（requirements 7.1）

Runner が worktree 準備の後（[`05-runner.md`](./05-runner.md) 工程4）に行う。

1. 開始点 SHA（base_sha）の tree から、ファイル名が `AGENTS.md` のものをすべて列挙する。ルートだけでなくサブディレクトリも対象とする。
   ```
   git -C <worktree> ls-tree -r --name-only <base_sha>   # 名前が AGENTS.md のものを抽出
   git -C <worktree> show <base_sha>:<path>               # 内容を取得
   ```
2. 同じ方法で、ハーネス固有の指示ファイルと設定を列挙する（requirements 7.1「Claude Code 固有の指示ファイルやハーネスの設定についても、実行開始時の構成を同様に記録する」）。対象の候補は `CLAUDE.md`、`.claude/` 配下の設定・コマンド・エージェント・Skills、`.codex/` 配下、`.mcp.json`。一覧は実装時に確定する。
3. 各ファイルの相対パス、内容、SHA-256 を Control Plane へ送り、context_revisions（layer=`project`、source_commit_sha=base_sha）に保存する。
4. 作業ツリー上のファイルではなく、開始点 commit の内容を読む。実行途中に `AGENTS.md` が変更されても、その Execution で固定した指示としては採用しない。

ハーネスが実行中に `AGENTS.md` やハーネス固有の指示ファイルを変更した場合、Gate 1 の結果とともに管理画面と PR 本文で知らせる（[`10-verification-gates.md`](./10-verification-gates.md) 3.2 の `instruction_file_changed`）。Block にするかは requirements に記載が無く、Phase 1 では知らせるだけとする案である。

再実行（mode=`continue`）でも、固定の基準は worktree の開始点 commit（引き継いだ base_sha）とする（requirements 8.3「Project Context は worktree の開始点の commit に含まれるものを使う」）。

### 3.3 Harness Context の固定

1. Execution に固定した Profile の commit SHA で、ハーネスリポジトリからプロンプト・Skills・設定のファイルを読む（`git show <commit_sha>:<path>`）。どのファイルを読むかは Profile 定義（[`02-data-model.md`](./02-data-model.md) 4.3）で決める。
2. 読んだファイルのパスとハッシュを context_revisions（layer=`harness`）に保存する。
3. 読んだ内容は Harness Sandbox 内の専用設定環境に置く（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.4）。ハーネスリポジトリの作業ツリーは使わない。

### 3.4 Global Context の固定

1. Control Plane が evaloom リポジトリの `context/` 配下から、Execution に渡すファイルを選ぶ（3.6）。
2. 選んだファイルの内容とハッシュを context_revisions（layer=`global`）に保存する。

### 3.5 Task Context の固定

タスクの title / body / acceptanceCriteria を1件の context_revisions（layer=`task`、kind=`task_input`）として保存する。Issue 本文などの外部入力は信頼済みの命令として扱わない（requirements 15.1）。Execution Context の中では、Task Context をタスクの内容として区切って渡し、Global の安全・権限ルールと同じ扱いにしない。

### 3.6 Context Builder（requirements 7.2）

Control Plane が提供する。Runner は Project Context を送った応答として Execution Context を受け取る（`POST /api/executions/{id}/context`）。

| 処理（requirements 7.2） | Phase 1 での内容 |
|---|---|
| Context 取得 | 3.2〜3.5 の4階層を集める |
| 関連情報の選択 | Global は開発タスク向けに指定したファイルだけ。Harness は Profile 定義が指定したものだけ。Project は固定した `AGENTS.md` 等。Task は当該タスクだけ |
| バージョン確認 | Profile の commit SHA と、Execution に固定した値が一致することを確かめる |
| 優先順位判定 | 3.7 |
| 権限確認 | Execution の Project・Harness に属さない Context を含めない |
| 情報量調整 | 上限を超える場合の扱いは実装時に確定 |
| 実行先向けの整形 | Adapter ごとの形式に合わせる（[`08-harness-adapter.md`](./08-harness-adapter.md)）。CLI が `AGENTS.md` 等を作業ディレクトリから自分で読む場合は、二重に渡さない |

Execution Context の中身（`instructions` と `revisionIds`）の形は [`02-data-model.md`](./02-data-model.md) 5.7 を参照する。

### 3.7 優先順位と矛盾（requirements 7.3）

- Global の安全・権限ルールは下位階層で上書きできない。Phase 1 では、安全・権限ルールは指示文ではなく Sandbox と Runner の権限で強制する（[`09-sandbox.md`](./09-sandbox.md)）。指示文の並びは Global → Harness → Project → Task とする（案）。
- Harness Context と Project Context の矛盾の自動検出は、Phase 1 では構造的な確認（必須ファイルの欠け、版の不一致）に限る案とする。意味の矛盾の検出方法は requirements に無い（21章「Context Builder の優先順位・競合解決」）。
- 解決できない重要な矛盾を検出したら実行せず、Execution を `blocked`（cause=`context_conflict`）にする。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| Project Context の正本 | 各プロジェクトリポジトリの `AGENTS.md` | requirements 7.1 |
| `AGENTS.md` の対象 | 開始点 commit に含まれる、ルートとサブディレクトリのすべて | requirements 7.1 |
| 記録する内容 | ファイルの内容とハッシュ | requirements 7.1 |
| 実行途中の変更 | その Execution の確定済みの指示として自動採用しない | requirements 7.1 |
| Context の版の固定時点 | Runner がタスクを取得した時点（Project Context は開始点 commit） | requirements 8.3 |
| 全 Context の毎回の受け渡し | 禁止 | requirements 7.2 |
| Global の安全・権限ルール | 下位階層で上書き不可 | requirements 7.3 |

## 5. インターフェース

- Runner 用 API `POST /api/executions/{id}/context`（追加案）
  - 要求: 固定した Project Context と Harness Context のファイル一覧（パス、内容、ハッシュ、source_commit_sha）
  - 応答: Execution Context（`instructions`、`revisionIds`）
- テーブル: context_revisions（[`02-data-model.md`](./02-data-model.md)）
- イベント: `context_pinned`（[`02-data-model.md`](./02-data-model.md) 5.3）

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| ハーネスリポジトリに Profile の commit SHA が無い | `failed`（`config_error`） |
| Profile 定義が指定したファイルが無い | `failed`（`config_error`） |
| 解決できない重要な矛盾 | `blocked`（`context_conflict`） |
| Global Context のファイルが読めない | `failed`（`config_error`） |

## 7. スコープ外

- Evaloom 側での Project Context の管理（後の Phase）
- Context 更新候補の提出と承認（requirements 7.4）
- 意味の矛盾の自動検出
- 管理画面での Global Context の編集（requirements 16.8 の Global Context は Phase 1 では表示だけとする案）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) context_revisions と HarnessRequest
- [`05-runner.md`](./05-runner.md) 工程4, 5
- [`06-worktree.md`](./06-worktree.md) 開始点 SHA
- [`08-harness-adapter.md`](./08-harness-adapter.md) 専用設定環境への配置
- [`10-verification-gates.md`](./10-verification-gates.md) 指示ファイルの変更の通知

## 9. 実装単位

- [ ] [Backend] 開始点 commit からの AGENTS.md とハーネス固有指示ファイルの列挙・取得
  - 受け入れ基準: ルートとサブディレクトリに AGENTS.md を置いたテスト用リポジトリで、すべてが列挙される; 作業ツリーで AGENTS.md を書き換えても、取得される内容は開始点 commit のものになる
- [ ] [Backend] Harness Context の commit SHA 固定読み込み
  - 受け入れ基準: ハーネスリポジトリの作業ツリーやタグを動かしても、Profile の commit SHA の内容が読まれる; 存在しない SHA で failed(config_error) になる
- [ ] [Backend] context_revisions への保存と context API（Runner 用）
  - 受け入れ基準: 1つの Execution について global / harness / project / task の4階層の行が保存される; 各行にハッシュと source_commit_sha が入る
- [ ] [Backend] Context Builder（選択、バージョン確認、整形、矛盾時の Blocked）
  - 受け入れ基準: 別 Project の Context が含まれない; 固定した版と違う Profile の Context を渡すとエラーになる; 構造的な矛盾のテストケースで blocked(context_conflict) になる
