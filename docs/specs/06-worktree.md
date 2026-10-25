---
status: confirmed
範囲: worktree の作成・再利用、開始点 SHA の記録、ブランチ作成、コピー許可リスト、セットアップコマンド、保持とクリーンアップ
---

# 06. Worktree 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Runner が行う worktree の準備と後始末（requirements 8.4, 8.6, 15.4）

## 1. 目的

- 本人の通常のローカルリポジトリを直接変更せず、タスク専用の worktree で作業させる（requirements 15.4）。
- 開始点の commit SHA を記録し、Gate 1 の差分検査と Project Context の固定の基準にする（requirements 7.1, 8.5, 8.6）。
- `.env` などのファイルを無条件にコピーせず、許可リストに載ったものだけを置く（requirements 5.1, 8.6）。
- セットアップコマンドをホスト上で直接実行せず、Test Sandbox で実行する（requirements 8.6, 15.7）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | claim の応答（Project の設定、previous）、Runner の Worktree Root |
| 出力 | worktree ディレクトリ、worktrees の行、executions.base_sha / branch_name / worktree_id、`worktree_ready` イベント、setup の test_runs（kind=`setup`） |

## 3. 処理詳細

### 3.1 どの経路を通るか

| 条件 | 経路 |
|---|---|
| mode=`continue` かつ前回の worktree がある | 3.3 再利用 |
| mode=`continue` かつ前回の worktree が無い（初回） | 3.2 新規作成 |
| mode=`fresh` | 3.2 新規作成（ブランチ名は 3.2.1 の「やり直し」） |

### 3.2 新規作成

1. Runner が worktree の元になるリポジトリ（3.2.2）でリモートの Base Branch を取得する（`git fetch <remote> <baseBranch>`）。取得に使う認証情報は Trusted Runner の読み取り用で、worktree には置かない。
2. 取得した `<remote>/<baseBranch>` の commit SHA を開始点 SHA（base_sha）として記録する。
3. ブランチ名を決め（3.2.1）、worktree を作る。
   ```
   git -C <元リポジトリ> worktree add -b <branch> <worktreeRoot>/<task-id>[-r<n>] <base_sha>
   ```
4. worktree のパスが Worktree Root の配下にあることを、シンボリックリンクを解決した実パスで確かめる。
5. コピー許可リストのファイルを置く（3.5）。
6. worktrees に行を作り（status=`active`）、Execution に base_sha・branch_name・worktree_id を書く。gate1_base_sha も base_sha にする。
7. `worktree_ready` イベントを送る。
8. セットアップコマンドを実行する（3.4）。

本人の通常のローカルリポジトリの作業ツリー（本人のチェックアウト）には触れない。Git 操作はすべて [`09-sandbox.md`](./09-sandbox.md) 3.6 の安全なラッパーで行う。

#### 3.2.2 worktree の元になるリポジトリ（Q27）

| 案 | 内容 | 判断 |
|---|---|---|
| A. Evaloom の作業用クローン | Project ごとに Worktree Root 配下へ Git Repository URL からクローンを作り、そこから worktree を作る。本人のローカルリポジトリはコピー許可リストの読み取り元としてだけ使う | 推奨。ハーネスが書き込める Git 領域が本人のリポジトリと分かれる |
| B. 本人のローカルリポジトリ | Local Repository Path から直接 worktree を作る | worktree は `.git` を共有するため、ハーネスが本人のブランチの ref や設定を書き換えられる |

requirements 15.4 は「通常のローカルリポジトリを直接変更せず、専用 worktree を使用する」としている。B では worktree 経由で本人のリポジトリの Git 領域が変更されうるため、A を推奨する。採否は本人が決める（Q27）。

#### 3.2.1 ブランチ名

| 場合 | ブランチ名 | 状態 |
|---|---|---|
| 通常 | `evaloom/<task-id>-<shortName>` | 確定（requirements 8.5） |
| 最初からやり直す（mode=`fresh`） | `evaloom/<task-id>-<shortName>-r<n>`（n は fresh の通し番号） | 案（Q22、requirements 21章で詳細設計事項） |

同名のブランチがローカルかリモートにすでにある場合は作らずに `failed`（`config_error`）とする。既存ブランチを上書きしない。

### 3.3 再利用（mode=`continue`）

requirements 8.5「原則として、前回のブランチ・worktree・PR を引き継ぐ」「再実行の前に、Git の状態と未コミットの変更を確認する」に対応する。

1. worktrees の行と実ディレクトリがあること、チェックアウト中のブランチが branch_name と一致することを確かめる。
2. `git status --porcelain` で未コミットの変更を確かめる。コピー許可リストで置いたファイル（内容が置いたときと同じもの）は除いて判定する。
3. HEAD が前回の Execution の最後の既知の SHA（pushed_sha、無ければ harness_head_sha、無ければ base_sha）と一致することを確かめる。
4. 2 か 3 に当たれば、Execution を `blocked`（cause=`git_state`）にして止める。本人が状態を確認して再実行するか中止する。
5. 問題が無ければ、base_sha と gate1_base_sha は前回の値を引き継ぎ（gate1_base_sha の扱いは [`10-verification-gates.md`](./10-verification-gates.md) Q13）、worktrees.status を `active` に戻す。
6. worktree が削除済み（`cleaned`）でブランチが残っている場合は、そのブランチから worktree を作り直す。開始点 SHA は前回の値を使う。

再利用時はベースブランチの最新を取り込まない（rebase / merge をしない）。取り込みが必要かどうかの判断は requirements に無いため、本人が新しいタスクか `fresh` で対応する。

### 3.4 セットアップコマンド（requirements 8.6）

- worktree を新規作成したとき、または前回のセットアップが成功していないときに、Project Registry のセットアップコマンドを Test Sandbox で実行する（[`09-sandbox.md`](./09-sandbox.md)）。ホスト上で直接実行しない。
- セットアップコマンドが未設定なら何もしない。
- 結果を test_runs（kind=`setup`、target_sha=base_sha）に記録し、ログを artifacts（`setup_log`）に保存する。
- 終了コードが0でなければ `failed`（`config_error`）とする。タイムアウトなら `timed_out`。
- セットアップの成否を worktrees に記録する（`setup_status` 列、[`02-data-model.md`](./02-data-model.md)）。

Test Sandbox には本物の秘密情報を渡さない。依存関係の取得にネットワークが必要な場合のポリシーは [`09-sandbox.md`](./09-sandbox.md) 3.3 に従う。

### 3.5 コピー許可リスト（requirements 5.1, 8.6, 15.6）

1. Project の copy_allowlist の各パスについて、本人のローカルリポジトリから worktree の同じ相対パスへコピーする。
2. コピーするのは通常ファイルだけ。シンボリックリンク、ディレクトリ、許可リスト外のファイルはコピーしない。
3. コピー元が無いパスは飛ばし、`log` イベントで警告を出す。
4. コピーしたファイルの相対パスと SHA-256 を worktrees.copied_files に記録する。Gate 3 の作業ツリー確認と、3.3 の未コミット確認で除外するためである。

許可リストに載せてよいのは秘密情報を含まないファイルだけである。テストに値が必要なら、テスト専用のダミーの値を使う（requirements 8.6, 15.6）。

### 3.6 保持とクリーンアップ

| 状況 | worktree の扱い | 出典 |
|---|---|---|
| 実行中 | `active` | - |
| 失敗・中断・タイムアウト・中止・Blocked | 削除せず `retained` | requirements 8.3 |
| PR 作成まで完了 | 削除しない（同じブランチへの追加 commit の再実行に使う） | requirements 8.5 |
| PR がマージ済みまたはクローズ済みで、タスクが終端状態 | クリーンアップしてよい | 案（Q26） |

クリーンアップは `git worktree remove` で worktree ディレクトリだけを消し、ブランチは残す（requirements 8.5「過去の作業履歴は保持する」）。クリーンアップの契機（自動か本人の操作か）は requirements に無いため、実装時に確定する（Q26）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 作業場所 | 専用 worktree。通常のローカルリポジトリを直接変更しない | requirements 15.4 |
| 開始点 | リモートの Base Branch を取得し、その commit SHA を記録 | requirements 8.6 |
| ブランチ名 | `evaloom/<task-id>-<短い名前>` | requirements 8.5 |
| セットアップコマンドの実行場所 | Test Sandbox。ホストで直接実行しない | requirements 8.6, 15.7 |
| ファイルコピー | 許可リストのみ。`.env` 等を無条件にコピーしない | requirements 5.1, 8.6 |
| 中断・失敗時 | worktree を保持 | requirements 8.3 |
| 再実行 | 前回のブランチ・worktree・PR を引き継ぐ | requirements 8.5 |
| AI に実行先パスを推測させない | worktree のパスは Runner が決めて Adapter に渡す | requirements 3.3 |

## 5. インターフェース

- 報告: `POST /api/executions/{id}/result`（base_sha、branch_name、worktree の情報）、`POST /api/executions/{id}/events`（`worktree_ready`、`log`）
- テーブル: worktrees、executions、test_runs（[`02-data-model.md`](./02-data-model.md)）

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| fetch の失敗 | `failed`（`config_error`）。認証情報の値はログに出さない |
| 同名ブランチがすでにある | `failed`（`config_error`） |
| worktree のパスが Worktree Root の外に解決された | `failed`（`config_error`）。audit_logs に記録 |
| 再利用時に未コミットの変更がある、HEAD が想定と違う | `blocked`（`git_state`） |
| セットアップの失敗 | `failed`（`config_error`） |
| セットアップのタイムアウト | `timed_out` |

## 7. スコープ外

- worktree より強い隔離（別ユーザー・コンテナ）そのもの。これは [`09-sandbox.md`](./09-sandbox.md) で扱う
- ベースブランチの最新の取り込み（rebase / merge）
- 作業開始時点の既存テストを別に固定する仕組み（requirements 8.5）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) worktrees / executions / test_runs
- [`03-registry.md`](./03-registry.md) コピー許可リストとセットアップコマンドの登録
- [`05-runner.md`](./05-runner.md) 工程3, 6
- [`07-context.md`](./07-context.md) 開始点 SHA での AGENTS.md の固定
- [`09-sandbox.md`](./09-sandbox.md) Test Sandbox
- [`10-verification-gates.md`](./10-verification-gates.md) 開始点 SHA を基準にした差分検査
- [`11-git-publisher.md`](./11-git-publisher.md) 同じブランチへの push

## 9. 実装単位

- [ ] [Backend] worktree の元になるリポジトリの準備（Q27 で採用した案。A なら作業用クローンの作成と更新）
  - 受け入れ基準: 作業用クローンが Worktree Root 配下に Project ごとに1つ作られる; 2回目以降は fetch だけで再利用される
- [ ] [Backend] リモート Base Branch の取得と開始点 SHA の記録
  - 受け入れ基準: テスト用のリモートを使い、記録された base_sha がリモートの Base Branch の SHA と一致する; 本人のローカルリポジトリの作業ツリーとチェックアウト中のブランチが変わらない
- [ ] [Backend] worktree の新規作成とブランチ命名（通常・やり直し）
  - 受け入れ基準: ブランチ名が `evaloom/<task-id>-<shortName>` になる; 同名ブランチがあると failed になり既存ブランチが変わらない; worktree が Worktree Root の配下に作られる
- [ ] [Backend] コピー許可リストの適用とハッシュ記録
  - 受け入れ基準: 許可リストに無い `.env` がコピーされない; シンボリックリンクがコピーされない; コピーしたファイルのパスとハッシュが記録される
- [ ] [Backend] セットアップコマンドの Test Sandbox 実行と記録
  - 受け入れ基準: setup が Test Sandbox 内で実行される（ホストのプロセスとして実行されない）ことをテストで確認できる; 失敗時に failed、test_runs に kind=setup の行が残る
- [ ] [Backend] worktree の再利用（Git 状態の確認と Blocked）
  - 受け入れ基準: 未コミットの変更がある worktree で continue すると blocked（git_state）になる; コピーしたファイルだけが未追跡のときは blocked にならない; 削除済み worktree はブランチから作り直される
- [ ] [Backend] worktree の保持状態の管理とクリーンアップ（ブランチは残す）
  - 受け入れ基準: 失敗・中断後に worktrees.status が retained; クリーンアップ後もブランチが残る
