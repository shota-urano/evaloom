---
status: confirmed
範囲: 検証済み commit の push と PR 作成・更新、push 直前の確認、再実行時の PR の扱い、GitHub 側の権限設定
---

# 11. Git Publisher 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Gate 1〜3 を通った commit を push し、PR を作る構成要素（requirements 8.5, 15.7）

## 1. 目的

- ハーネスの commit から、Runner が PR を作成できるようにする（条件12）。
- テストした commit と同じ commit だけを push する（条件15）。
- push と PR 作成に使う GitHub の認証情報を、ハーネスから切り離して Git Publisher の中だけで使う（requirements 15.6, 15.7）。
- Evaloom は PR をマージしない（requirements 8.5, 15.3）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | verified_sha、branch_name、worktree（作業用クローン）、Project の Git Repository URL と Base Branch、前回の PR 番号、タスクと Gate の結果 |
| 出力 | リモートのブランチ、PR、executions.pushed_sha / pr_number / pr_url、`pushed` / `pr_created` イベント |

## 3. 処理詳細

### 3.1 push 直前の確認

Gate 3 を通った後、push の直前に次を確かめる（requirements 15.3「副作用のある操作を実際に行う直前にも確認する」）。

1. Control Plane に中止要求の有無を問い合わせる。中止要求があれば push しない。問い合わせできなければ push しない（Fail Closed）。
2. HEAD が verified_sha と一致し、作業ツリーに未コミットの変更が無いことをもう一度確かめる（requirements 8.5 の順序 4）。
3. push 先のブランチ名が `evaloom/` で始まることを確かめる。それ以外のブランチには push しない。

### 3.2 push

1. verified_sha を明示して push する。HEAD やブランチ名の解決結果には頼らない。
   ```
   git push <remote> <verified_sha>:refs/heads/<branch_name>
   ```
2. 強制 push はしない。リモートのブランチが想定と違う位置にあって fast-forward できなければ `failed`（`publish_failed`）とする。
3. push 後にリモートのブランチの SHA を取得し、verified_sha と一致することを確かめる。
4. pushed_sha を記録する。pushed_sha と verified_sha が一致しない状態で PR 作成へ進まない。

Git の実行は [`09-sandbox.md`](./09-sandbox.md) 3.6 の安全なラッパーを使う（フックと実行系の設定を無効にする）。

### 3.3 PR の作成・更新

| 状況 | 処理 |
|---|---|
| このブランチに開いている PR が無い | PR を作成する（head=branch_name、base=Base Branch） |
| このブランチに開いている PR がある | 新しい PR は作らない。同じブランチへの追加 commit として push し、PR にこの Execution の結果をコメントで追加する（案） |

1タスクにつきアクティブな PR は最大1本とする（requirements 8.5）。

PR の本文（案）:

- タスク ID と件名、受け入れ条件
- 作業概要（HarnessResult の summary）
- 実行したハーネスと Profile の版、commit SHA
- Gate の結果（Gate 2 の実行数・skip 数、ID による照合か件数による確認か）
- 本人が続行を選んで通した Block があれば、その内容
- `instruction_file_changed` があれば、変更された指示ファイル
- 「ハーネスが書いたテストの成功だけを要件充足の証明としない」旨の注記（requirements 8.5）

PR の作成後に pr_number と pr_url を記録し、`pr_created` イベントを送る。

### 3.4 再実行時の PR の扱い（requirements 8.5）

Runner は準備の段階（[`05-runner.md`](./05-runner.md) 工程2）で、前回の PR の状態を GitHub から取得して次のとおり扱う。

| 前回の PR | mode=`continue` | mode=`fresh` |
|---|---|---|
| まだ無い | 同じブランチで続け、作業完了後に PR を作る | 新しいブランチで実行する |
| 開いている | 同じブランチに追加 commit する | `blocked`（`git_state`）。本人が前の PR をクローズしてから再実行する（案、3.4.1） |
| クローズ済み（未マージ） | 受け付けない。`failed`（`config_error`、理由 `pr_closed`） | 新しいブランチで実行する |
| マージ済み | 受け付けない。`failed`（`config_error`、理由 `pr_merged`）。新しいタスクを作るよう案内する | 同左。評価結果や実装履歴が前のタスクと混ざらないよう、新しいタスクを作る |

#### 3.4.1 最初からやり直す場合

- 本人が明示的に `fresh` を選んだ場合だけ、新しいブランチで実行する（requirements 8.5）。ブランチ名は [`06-worktree.md`](./06-worktree.md) 3.2.1（Q22 案）。
- 過去の作業履歴は保持する。前のブランチは消さない。
- requirements 8.5 は「既存の PR があれば、クローズするなどして整理する」としている。Phase 1 では Evaloom が PR を自動でクローズせず、開いている PR がある間は `fresh` を止めて本人に整理を求める案とする。アクティブな PR を最大1本に保つためである。自動でクローズするかは要確認とする。

### 3.5 GitHub の権限（requirements 15.3, 15.7）

| 対策 | 内容 | 担当 |
|---|---|---|
| 認証情報の範囲 | Git Publisher の認証情報は対象リポジトリに限る。方式（fine-grained token / GitHub App）は実装時に確定（Q29） | 本人 + Backend |
| 認証情報の置き場所 | Git Publisher の中だけで使う。worktree、Sandbox、ログに出さない | Backend |
| マージをしない | Git Publisher にマージの処理を持たせない | Backend |
| ベースブランチを更新させない | GitHub のブランチ保護またはルールセットで、ベースブランチの更新とマージを本人だけに許す。push できる認証情報は PR のマージにも使えるため、コードの作りだけに頼らず GitHub 側の権限で拒否する | 本人 |
| PR の CI | PR の最新 commit に対して CI を実行し、必須ステータスチェックで最新 commit の成功を求める。この CI には秘密情報・書き込み権限・デプロイ権限を渡さない（requirements 8.5, 15.7） | 本人 |

PR のマージは本人が GitHub 上で行う（requirements 8.5）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| push・PR 作成の実行者 | Runner の Git Publisher。ハーネスは commit まで | requirements 8.5 |
| push する commit | 検証した commit と同じ SHA | requirements 8.5、条件15 |
| ブランチ名 | `evaloom/<task-id>-<短い名前>` | requirements 8.5 |
| アクティブな PR | 1タスクにつき最大1本 | requirements 8.5 |
| PR のマージ | 承認対象。Phase 2 までは本人が GitHub 上で行う。Evaloom はマージしない | requirements 8.5, 15.3 |
| 既存 PR が開いている再実行 | 同じブランチに追加 commit | requirements 8.5 |
| 既存 PR がクローズ済み | 通常の再実行を受け付けない。本人が明示的に最初からやり直す場合だけ新しいブランチ | requirements 8.5 |
| 既存 PR がマージ済み | 原則として新しいタスクを作る | requirements 8.5 |
| 本物の秘密情報を使う場所 | Trusted Runner と Git Publisher の中だけ | requirements 15.6 |

## 5. インターフェース

- 報告: `POST /api/executions/{id}/result`（pushed_sha、pr_number、pr_url）、イベント `pushed` / `pr_created`（[`02-data-model.md`](./02-data-model.md) 5.3）
- push 直前の中止確認: `POST /api/runners/{id}/heartbeat` の応答を使う（[`05-runner.md`](./05-runner.md) 5章）
- GitHub: REST API（PR の取得・作成・コメント）。認証方式は Q29

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| push 直前の確認で中止要求あり | push せず `cancelled` |
| Control Plane に問い合わせできない | push せず `failed`（`publish_failed`）。worktree を保持し、本人が再実行する |
| fast-forward できない | `failed`（`publish_failed`） |
| push 後のリモート SHA が verified_sha と違う | `failed`（`publish_failed`）。PR を作らない。audit_logs に記録 |
| push は成功し PR 作成に失敗 | `failed`（`publish_failed`）。pushed_sha は記録する。再実行（continue）で PR が無い状態として PR を作る |
| GitHub の認証エラー | `failed`（`publish_failed`）。認証情報の値はログに出さない |

## 7. スコープ外

- PR のマージ、承認と操作内容の結び付け（Phase 2、requirements 15.8）
- Linear Issue への PR URL の返却（Phase 2）
- PR の自動クローズ（要確認）
- GitHub の CI 結果の取り込み

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) executions の pushed_sha / pr_number / pr_url
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 再実行と中止
- [`05-runner.md`](./05-runner.md) 工程2, 13
- [`06-worktree.md`](./06-worktree.md) ブランチ名
- [`09-sandbox.md`](./09-sandbox.md) 安全な Git 操作、承認対象操作の拒否
- [`10-verification-gates.md`](./10-verification-gates.md) verified_sha

## 9. 実装単位

- [ ] （人間） Git Publisher の GitHub 認証情報を用意し（Q29）、対象リポジトリでベースブランチの保護（更新・マージを本人だけに許す）と PR の必須ステータスチェックを設定する
- [ ] [Backend] push 直前の確認（中止要求、HEAD と verified_sha、作業ツリー、ブランチ名の接頭辞）
  - 受け入れ基準: 中止要求があると push しない; Control Plane に届かないと push しない; `evaloom/` で始まらないブランチ名では push しない
- [ ] [Backend] verified_sha を明示した push とリモート SHA の確認
  - 受け入れ基準: テスト用のリモートで、push 後のリモートブランチの SHA が verified_sha と一致する; 強制 push をしない; fast-forward できない場合に publish_failed になる
- [ ] [Backend] PR の作成と、開いている PR への結果コメント
  - 受け入れ基準: PR が無いとき1本作られ pr_url が記録される; 開いている PR があるとき新しい PR が作られずコメントが付く; PR 本文に Gate 2 の件数と確認方法が入る
- [ ] [Backend] 再実行時の前回 PR の状態確認（3.4 の表）
  - 受け入れ基準: 表の各セルに対応するテストで、期待どおりに続行・failed・blocked になる
- [ ] [Backend] Git Publisher の認証情報の分離
  - 受け入れ基準: GitHub の認証情報が worktree、Sandbox の環境変数、ログ、DB に現れないことをテストで確認できる
