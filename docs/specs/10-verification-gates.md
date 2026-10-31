---
status: confirmed
範囲: テスト実行と既存テストの照合（Gate 2）、検証 SHA と作業ツリーの確認（Gate 3）。Gate 1 は Q36 で廃止
---

# 10. 検証 Gate 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: ハーネスの commit を push してよいかを Runner が機械的に判定する検証（Gate 2・3、requirements 8.5）

## 1. 目的

- 登録された必須の検証がすべて成功した場合だけ、push と PR 作成へ進む（requirements 8.5）。
- 既存のテストが実際に実行されたことを確かめる。ハーネスがテストを削除・無効化して検証を通すことを防ぐ（requirements 8.5、条件18）。
- テストした commit と同じ commit だけを push する（条件15）。
- 判定はテスト結果と SHA の照合で機械的に行い、AI の意味判断だけに頼らない（requirements 8.5）。
- 既存テストの中身の書き換え、テストの実行設定・CI の定義の変更は Gate では判定せず、PR のレビューで本人が確認する（requirements 8.5、Q36）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | worktree、harness_head_sha、Project のテストコマンド・レポート形式、コピー許可リストで置いたファイルの一覧 |
| 出力 | gate_results、test_runs、execution_blocks（Block のとき）、verified_sha、`gate_result` / `blocked` イベント |

## 3. 処理詳細

### 3.1 全体の順序（requirements 8.5）

1. ハーネスがコードを変更し、commit する。
2. Runner がその commit の SHA を取得し、記録する（harness_head_sha）。
3. Gate 2：Runner が Test Sandbox で、harness_head_sha に対してテストを1回実行する。テストした SHA を verified_sha として記録する。
4. Gate 3：HEAD が verified_sha と一致すること、作業ツリーに未コミットの変更が無いことを確かめる。
5. Git Publisher が verified_sha と同じ commit を push し、PR を作成する（[`11-git-publisher.md`](./11-git-publisher.md)）。

どの Gate でも結果は gate_results に保存する。Block 原因を見つけたら Execution を `blocked` にし、execution_blocks に原因と HEAD SHA を残して通知する（[`12-notification.md`](./12-notification.md)）。自動では push しない。

### 3.2 Gate 1（廃止）

Gate 1（変更差分の検査）は Q36 で廃止した。差分のファイル名パターンで弱体化を検出する方式は、テストの道具・設定ファイル・期待値ファイルの種類だけ列挙が要り、開発に固有の判定を本体に置くことになるためである。番号は既存の記録と合わせるため変えず、Gate 1 は欠番とする。節番号も変えない。試作は `feat/pdy.1-gate1-diff` に残す（未マージ）。

### 3.3 Gate 2：テストを1回実行し、既存テストを照合する

1. Project Registry のテストコマンドを、Test Sandbox で harness_head_sha に対して1回実行する（requirements 8.5）。ハーネスはテストコマンドを変更できない。
2. テストコマンドが未設定なら、テストを実行せず `failed`（`config_error`）とする。push しない。
3. テストランナーのレポート（test_report_format / test_report_path、Q12）から、実行したテストの ID、実行数、skip 数を取り出し、test_runs（kind=`verify`）と artifacts（`test_ids`）に保存する。
4. 次のどれかに当たれば止める。

| 状況 | 結果 |
|---|---|
| 終了コードが0でない、または失敗したテストがある | `failed`（`test_failed`） |
| 既存テストの ID のうち、実行されていないものがある | `blocked`（`gate2_missing_tests`） |
| ID が取れない環境で、実行数が基準値より少ない、または skip 数が基準値より多い | `blocked`（`gate2_missing_tests`） |
| レポートが無い・解釈できず、実行数も取れない | `blocked`（`gate2_unverifiable`） |

- 新しいテストを追加すれば実行数は増えるため、件数の一致では判定しない（requirements 8.5）。
- ID が取れない環境では実行数と skip 数で確認するが、それだけでは既存テストが欠けていないことを完全には証明できない（requirements 8.5）。この場合は gate_results に「件数による確認」と記録し、管理画面に表示する。
- 既存テストが残っていても実行されたとは限らない（例: 新しく追加したテストの `describe.only` によって他のテストが実行されない）。そのため実行数の確認を必須とする（requirements 8.5）。

5. すべて通れば verified_sha = harness_head_sha を記録する。

#### 3.3.1 既存テストの基準値をどこから得るか（Q11）

照合には「作業開始時点の既存テストの ID・実行数・skip 数」が要る。requirements はその取り方を定めていない。

| 案 | 内容 | 長所 | 短所 |
|---|---|---|---|
| A. 開始点で1回実行する | worktree の setup 後、開始点 SHA に対してテストを Test Sandbox で実行し、ID・件数を基準値として保存する（test_runs kind=`baseline`） | どのテストランナーでも同じ方法で取れる。開始点で既に失敗するテストも分かる | テスト時間が倍になる |
| B. 一覧出力を使う | テストランナーの一覧出力（実行せずに ID を列挙する機能）を開始点で使う | 速い | テストランナーごとに対応が要る。一覧と実行の ID 形式が一致しない場合がある |

A を推奨する。requirements 8.5 が「作業開始時点の既存テストを別に固定して再実行する仕組みは、Phase 1では作らない」としているのは、Gate 2 の後に既存テストを別に再実行する仕組みのことで、基準値を取るための開始点での実行とは別と解釈した。この解釈は要確認とする。

基準値は worktree ごとに1回取り、mode=`continue` の再実行では引き継ぐ。

### 3.4 Gate 3：実行結果と commit の検証

1. 現在の HEAD が verified_sha と一致することを確かめる。
2. 作業ツリーに未コミットの変更が無いことを確かめる（`git status --porcelain`）。コピー許可リストで置いたファイルのうち、内容が置いたときと同じものは除く（[`06-worktree.md`](./06-worktree.md) 3.5）。テストの出力先（test_report_path など）を除くかは実装時に確定する。
3. Gate 2 の test_runs が verified_sha に対するもので、実行数が0より大きいことを確かめる。
4. どれかに当たれば `blocked`（`gate3_mismatch`）にする。テストの後に commit が追加された場合や、作業ツリーに変更が残っている場合は push しない（requirements 8.5）。

GitHub の必須ステータスチェックでは `skipped` や `neutral` も条件を満たしたと扱われる場合があるため、必要なテストが実際に実行されたかは Evaloom 側で確かめる（requirements 8.5）。

SHA と作業ツリーの一致で確かめられるのはコードの一致までで、テスト環境や外部依存まで含めた完全な再現性は保証しない（requirements 8.5）。

### 3.5 Blocked からの続行

本人が変更内容を確認し、続行を選んだ場合だけ先へ進む（requirements 8.5）。続行の操作は [`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.8。再開する工程は次のとおり。

| Block した Gate | 再開する工程（resume_point） |
|---|---|
| Gate 2 | `gate3` |
| Gate 3 | `gate3`（もう一度確認する。HEAD が変わっていれば再び Blocked） |

Gate 3 は続行しても飛ばさない。push する commit がテストした commit と一致することは、本人の判断で置き換えられないためである（条件15）。Gate 3 の Block から抜けるには、本人が worktree を直して再実行する。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| push・PR 作成の条件 | 登録された必須の検証がすべて成功したときだけ | requirements 8.5 |
| テスト失敗・テストコマンド未設定・検証不能 | 理由と作業結果を保存して止める | requirements 8.5 |
| テストコマンドの正本 | Project Registry。ハーネスは変更できない | requirements 8.5 |
| 既存テストの中身・テストの実行設定・CI の定義の変更 | Gate では判定しない。PR のレビューで本人が確認する | requirements 8.5、Q36 |
| テストの実行回数 | Gate 2 で1回 | requirements 8.5 |
| 件数の一致での判定 | しない | requirements 8.5 |
| 実行数の確認 | 必須 | requirements 8.5 |
| push する commit | テストした commit と同じもの | requirements 8.5、条件15 |
| 検証できない場合 | Blocked。自動で push・PR 作成をしない | requirements 8.5 |

## 5. インターフェース

- テーブル: gate_results、test_runs、execution_blocks、executions（harness_head_sha / verified_sha）（[`02-data-model.md`](./02-data-model.md)）
- Block 原因の値: [`02-data-model.md`](./02-data-model.md) 5.6
- 報告: `POST /api/executions/{id}/result`、イベント `gate_result` / `blocked`
- Sandbox: [`09-sandbox.md`](./09-sandbox.md) 5章（kind=`test_run`）

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Test Sandbox を作れない | `failed`（`sandbox_error`） |
| テストのタイムアウト | `timed_out` |
| レポートのパスにファイルが無い | 実行数も取れなければ `blocked`（`gate2_unverifiable`） |

## 7. スコープ外

- AI が変更できない場所に置く非公開の受け入れテスト、評価 AI（Phase 3、requirements 11.6）
- 作業開始時点の既存テストを別に固定して再実行する仕組み（requirements 8.5）
- テストの変更を差分から判定する検査（旧 Gate 1、Q36 で廃止）。既存テストの書き換え、テストの実行設定・CI の定義の変更、テストのときだけ通る抜け道は PR レビューで本人が確認する
- GitHub 側の CI 結果の取り込み（Phase 1 では Evaloom 側の Gate を正とする）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) gate_results / test_runs / execution_blocks / Finding
- [`03-registry.md`](./03-registry.md) テストコマンド・レポート形式の登録
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) Blocked からの続行
- [`05-runner.md`](./05-runner.md) 工程7, 9〜11
- [`06-worktree.md`](./06-worktree.md) 開始点 SHA とコピーしたファイル
- [`09-sandbox.md`](./09-sandbox.md) Test Sandbox と安全な Git 操作
- [`11-git-publisher.md`](./11-git-publisher.md) 検証済み commit の push
- [`14-security-tests.md`](./14-security-tests.md) 条件15, 18 の受け入れテスト

## 9. 実装単位

- [ ] [Backend] テストレポートの解析（JUnit XML から ID・実行数・skip 数を取得）
  - 受け入れ基準: テスト用の JUnit XML から ID の一覧と件数が取れる; 壊れた XML で解析エラーが返る
- [ ] [Backend] 既存テストの基準値の取得（Q11 で採用した案）
  - 受け入れ基準: 開始点 SHA に対する基準値が test_runs（kind=baseline）に保存される; continue の再実行で再取得しない
- [ ] [Backend] Gate 2 のテスト実行と既存テストの照合
  - 受け入れ基準: テストコマンド未設定で failed(config_error); 既存テストの ID が1件欠けると blocked(gate2_missing_tests); skip 数が増えると blocked; 新しいテストを追加して実行数が増えても通る; レポートが無いと blocked(gate2_unverifiable)
- [ ] [Backend] Gate 3 の SHA と作業ツリーの確認
  - 受け入れ基準: テスト後に commit を追加すると blocked(gate3_mismatch); 未追跡ファイルを置くと blocked; コピー許可リストで置いた未変更のファイルだけなら通る
- [ ] [Backend] Blocked からの再開（resume_point）
  - 受け入れ基準: Gate 2 で Blocked → 続行で Gate 3 から再開する; Gate 3 の Block は続行しても Gate 3 をもう一度通す
