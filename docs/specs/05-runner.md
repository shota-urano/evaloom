---
status: confirmed
範囲: Local Runner の pull ループ、heartbeat、Execution の実行手順、ログ収集と機密情報除外、タイムアウト、キャンセル
---

# 05. Local Runner 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Trusted Runner（`evaloom-runner`）の動作全体。個々の工程の中身は 06〜11 に分ける

## 1. 目的

- Mac mini 上でハーネスを実行する Local Runner を、Control Plane から論理的に独立したコンポーネントとして作る（requirements 2.3, 8.4）。
- Runner は Control Plane へ自分から仕事を取りに行く（pull）。Control Plane から Runner へは接続しない（requirements 2.3）。
- Execution の工程（準備 → ハーネス実行 → 検証 → push・PR 作成）を決まった順序で進め、各工程の結果を Control Plane へ報告する。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | claim の応答（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.3.1）、heartbeat の応答（中止指示） |
| 出力 | Runner 用 API への報告（events / result / context）、worktree、ログ・成果物のファイル |

## 3. 処理詳細

### 3.1 起動

1. 設定ファイルから Runner ID、Control Plane の URL、Runner トークンの置き場所、Worktree Root、データディレクトリを読む。
2. 起動 ID（bootId）を生成する。
3. 最初の heartbeat を送る。Control Plane はこれを契機に、前回の起動で動いていた Execution を `interrupted` にする（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.6）。

Control Plane の URL を設定で持つため、Control Plane を別サーバーへ移しても Runner の設定変更だけで切り替えられる（requirements 2.3）。

### 3.2 pull ループ

```
loop:
  heartbeat（bootId、実行中の Execution ID 一覧）
    └ 応答に中止指示があれば該当 Execution を中止（3.6）
  空きがあれば claim
    └ 仕事があれば Execution を別の goroutine で開始（3.3）
  待機（間隔は実装時に確定）
```

- Runner 側でも同時に動かす Execution を2本までに制限する。同時実行制御の正本は Control Plane の claim（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.3）で、Runner 側の制限は二重の防御である。
- heartbeat と claim の間隔、heartbeat 途絶とみなす時間は実装時に確定する。

### 3.3 Execution の実行手順

各工程の開始・終了を `POST /api/executions/{id}/result` で報告し、Control Plane が状態を遷移させる。resume_point がある場合（Blocked からの続行）は、その工程から始める。

| # | 工程 | Execution 状態 | 詳細仕様 |
|---|---|---|---|
| 1 | Project のパスを再検証する | `preparing` | [`03-registry.md`](./03-registry.md) 3.2 |
| 2 | 前回の PR の状態を確認する（mode=`continue` で PR があるとき） | `preparing` | [`11-git-publisher.md`](./11-git-publisher.md) 3.4 |
| 3 | worktree を作成または再利用し、開始点 SHA を記録する | `preparing` | [`06-worktree.md`](./06-worktree.md) |
| 4 | 適用対象の `AGENTS.md` と設定を固定し、Control Plane に送る | `preparing` | [`07-context.md`](./07-context.md) |
| 5 | Control Plane から Execution Context を受け取る | `preparing` | [`07-context.md`](./07-context.md) |
| 6 | セットアップコマンドを Test Sandbox で実行する | `preparing` | [`06-worktree.md`](./06-worktree.md) 3.4 |
| 7 | 既存テストの基準値を取る（Q11 の採用案による） | `preparing` | [`10-verification-gates.md`](./10-verification-gates.md) 3.3 |
| 8 | Adapter でハーネスを Harness Sandbox 内で起動し、終了まで監視する | `running` | [`08-harness-adapter.md`](./08-harness-adapter.md), [`09-sandbox.md`](./09-sandbox.md) |
| 9 | HEAD の SHA を取得して記録する（harness_head_sha） | `verifying` | [`10-verification-gates.md`](./10-verification-gates.md) |
| 10 | Gate 1：変更差分の検査 | `verifying` | [`10-verification-gates.md`](./10-verification-gates.md) 3.2 |
| 11 | Gate 2：テストを Test Sandbox で1回実行し、既存テストを照合する | `verifying` | [`10-verification-gates.md`](./10-verification-gates.md) 3.3 |
| 12 | Gate 3：検証 SHA と HEAD、作業ツリーの確認 | `verifying` | [`10-verification-gates.md`](./10-verification-gates.md) 3.4 |
| 13 | Git Publisher で push し、PR を作成または更新する | `publishing` | [`11-git-publisher.md`](./11-git-publisher.md) |
| 14 | 使用量を記録し、完了を報告する | `succeeded` | 3.7 |

- どの工程でも失敗したら、以降の工程を行わない。worktree は削除せず保持する（[`06-worktree.md`](./06-worktree.md) 3.6）。
- 工程 10〜12 で Block 原因を見つけたら `blocked` を報告して止まる。自動で push・PR 作成を行わない（requirements 8.5）。
- ハーネスが commit を1つも作らなかった場合の扱いは requirements に記載が無い。`failed`（`execution_failed`、理由「変更なし」）とする案で、実装時に確定する。

### 3.4 ログ収集

- ハーネス・setup・テストの標準出力と標準エラーを行単位で読み、機密情報を除外（3.5）してから次の2か所に出す。
  - `POST /api/executions/{id}/events` に `log` イベントとして送る（管理画面の SSE 表示用）
  - データディレクトリのログファイルに追記し、終了時に artifacts として登録する
- 送信が失敗してもログファイルへの書き込みは続ける。送れなかった分は後でまとめて送る（再送の方式は実装時に確定）。
- データディレクトリは Git 管理外に置く（requirements 3.1）。

### 3.5 ログの機密情報除外（requirements 15.2）

- Runner が保持している秘密の値（Runner トークン、GitHub の認証情報、モデル API の認証情報など）と完全一致する文字列を `[REDACTED]` に置き換える。
- 加えて、よく知られた形式（`Authorization:` ヘッダ、`-----BEGIN ... PRIVATE KEY-----` など）を置き換える。パターンの一覧は実装時に確定する（Q24）。
- 置き換えたログには artifacts.redacted=1 を付ける。置き換え前の内容は保存しない。

Sandbox には本物の秘密情報を渡さない（[`09-sandbox.md`](./09-sandbox.md)）ため、ログに秘密情報が現れる経路は Runner 自身の処理に限られる。除外処理はその前提が崩れたときの二重の防御である。

### 3.6 タイムアウトとキャンセル

**タイムアウト**:

- 工程ごとの上限時間（setup / ハーネス / テスト）は Project の timeouts、無ければ全体設定の既定値を使う。値は requirements に無く、実装時に確定する（Q15）。
- 上限を超えたら、Adapter のキャンセル（[`08-harness-adapter.md`](./08-harness-adapter.md)）を呼び、Sandbox ごとプロセスを止める。一定時間で止まらなければ強制終了する。
- `timed_out`（error_class=`timeout`）を報告する。

**キャンセル**:

- heartbeat の応答に中止指示があれば、実行中の工程を止める（止め方はタイムアウトと同じ）。
- `cancelled`（error_class=`cancelled`）を報告する。
- push の直前（[`11-git-publisher.md`](./11-git-publisher.md)）にも中止要求の有無を Control Plane に確かめる。

どちらの場合も worktree は削除せず保持する。

### 3.7 使用量の記録

HarnessResult の `usage`（取れる場合のトークン数）と、Execution 全体・ハーネス工程の実行時間を usage_records に記録する（requirements 12.5「費用はトークン数と実行時間で記録する」）。費用上限（requirements 15.2）による停止を Phase 1 で行うかは未定（Q16）。

### 3.8 Runner が行わない操作

requirements 15.3 の承認対象操作（本番デプロイ、PR マージ、外部メール送信、重要ファイル削除、認証・権限変更、共通契約・評価基準・中央セキュリティポリシーの変更）を行うコードを Runner に持たせない。Git Publisher が使う GitHub の認証情報にもマージの権限を与えない（[`11-git-publisher.md`](./11-git-publisher.md) 3.5）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 仕事の受け取り方 | Runner からの pull のみ | requirements 2.3, 8.4 |
| 同時実行（全体） | 2本（正本は Control Plane の claim） | requirements 8.7 |
| 同時実行（同一プロジェクト） | 1本（正本は Control Plane の claim） | requirements 8.7 |
| 自動リトライ | 0回 | requirements 8.3 |
| 中断・失敗時の worktree | 保持する | requirements 8.3 |
| push の条件 | 必須の検証がすべて成功したときだけ | requirements 8.5 |
| タイムアウト値 | 未定（実装時に確定） | Q15 |

## 5. インターフェース

- Runner 用 API: `POST /api/runners/{id}/claim`, `POST /api/runners/{id}/heartbeat`, `POST /api/executions/{id}/events`, `POST /api/executions/{id}/result`, `POST /api/executions/{id}/context`（[`01-architecture.md`](./01-architecture.md) 5.2）
- イベント種別: [`02-data-model.md`](./02-data-model.md) 5.3
- 本人用の参照: `GET /api/executions/{id}/events`（SSE）, `GET /api/executions/{id}/logs`, `GET /api/runners`

heartbeat の要求と応答（案）:

```json
// 要求
{ "bootId": "...", "runningExecutionIds": ["..."] }
// 応答
{ "cancelExecutionIds": ["..."] }
```

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Control Plane に接続できない | 実行中の Execution は続ける。報告は再送待ちに積む。push の直前の確認ができない場合は push しない（Fail Closed） |
| パス再検証の失敗 | `failed`（`config_error`） |
| Sandbox の作成失敗 | `failed`（`sandbox_error`） |
| ハーネスの異常終了 | `failed`（`execution_failed`）。HarnessResult の errorClass を優先する |
| テストコマンド未設定 | Gate 2 で `failed`（`config_error`）。push しない（requirements 8.5） |
| ログ送信の失敗 | ファイルへの書き込みは続け、後で再送する |
| Runner 自身の異常終了 | 再起動後の最初の heartbeat で Control Plane が `interrupted` にする |

## 7. スコープ外

- 複数 Runner、別マシンでの Runner（Phase 6。ただし pull 方式と設定による接続先切り替えは Phase 1 から満たす）
- 評価の実行（Phase 3）
- 費用上限による自動停止（Q16 で決まるまで）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) Runner 用 API と権限
- [`02-data-model.md`](./02-data-model.md) executions / execution_events / artifacts / usage_records
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) claim と状態遷移
- [`06-worktree.md`](./06-worktree.md) 工程3, 6
- [`07-context.md`](./07-context.md) 工程4, 5
- [`08-harness-adapter.md`](./08-harness-adapter.md) 工程8
- [`09-sandbox.md`](./09-sandbox.md) Sandbox
- [`10-verification-gates.md`](./10-verification-gates.md) 工程7, 9〜12
- [`11-git-publisher.md`](./11-git-publisher.md) 工程2, 13

## 9. 実装単位

- [ ] [Backend] Runner の設定読み込み、bootId 生成、heartbeat の送信と中止指示の受け取り
  - 受け入れ基準: 起動直後に bootId 付きの heartbeat が送られる; 応答の cancelExecutionIds に含まれる Execution の中止処理が呼ばれる; Control Plane の URL を設定で差し替えられる
- [ ] [Backend] pull ループと Runner 側の同時実行上限
  - 受け入れ基準: Runner 側で同時に動く Execution が2本を超えない; 仕事が無いときに claim を繰り返しても Execution が作られない
- [ ] [Backend] Execution の工程実行器（工程表、resume_point からの再開、失敗時の打ち切り）
  - 受け入れ基準: 各工程をスタブにしたテストで、工程が表の順に呼ばれる; 途中の工程が失敗すると後続が呼ばれない; resume_point=gate2 のとき工程11から始まる
- [ ] [Backend] ログ収集（行単位の読み取り、events 送信、ファイル保存、artifacts 登録、送信失敗時の再送）
  - 受け入れ基準: ハーネスの出力が SSE で管理画面に届く; Control Plane を止めている間もログファイルに書かれ、再開後に送られる
- [ ] [Backend] ログの機密情報除外
  - 受け入れ基準: Runner が保持する秘密の値をログに出すテストで、保存されたログと events に値が残らず [REDACTED] になる; artifacts.redacted が 1 になる
- [ ] [Backend] タイムアウトとキャンセル（Adapter のキャンセル、強制終了、状態報告）
  - 受け入れ基準: 上限時間を短くしたテストでハーネスが止まり timed_out が報告される; 中止指示で cancelled が報告される; どちらも worktree が残る
- [ ] [Backend] 使用量の記録
  - 受け入れ基準: 完了した Execution ごとに usage_records に実行時間が記録される; トークン数が取れない Adapter では NULL になる
