---
status: draft
written: 2026-11-10
範囲: Trusted Runner の pull ループ、heartbeat、段の枠（claim → prepare → work → check → 承認 → publish）の実行、Profile の中身の読み出し、作業領域のルート、鍵の受け渡し、通話口の中継、ログ収集と機密情報の除外、タイムアウトと中止、振り分けの提案の実行
---

# 05. Local Runner 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Trusted Runner（`evaloom-runner`）の動作全体。段ごとの隔離の作り方は [`09-sandbox.md`](./09-sandbox.md)、ミクロハーネスの起動は [`08-harness-adapter.md`](./08-harness-adapter.md) に分ける

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。段の名前と段ごとに見える範囲はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）6.2 の表、API は [`01-architecture.md`](./01-architecture.md)、テーブルと列挙値は [`02-data-model.md`](./02-data-model.md) に従う。

旧 3.3 の工程表のうち開発の中身（Project のパスの再検証、前回の PR の確認、worktree の作成、setup、テストの基準値、Gate 2・3、push と PR の作成）と、旧 3.9 の開発の接続の表、旧 4.1 の `worktreeRoot`・`githubAppPrivateKeyFile`・`commitAuthor`・`defaultTimeouts` は、software-engineer へ移し、この文書からは消した（本人決定 1）。Runner は段の枠だけを持ち、段の中身はスペシャルハーネスの固定版のプログラムとミクロハーネスが行う。

## 1. 目的

- Mac mini 上で段を動かす Local Runner を、Control Plane から独立した構成要素として作る（requirements 2.3、8.4）。
- Runner は Control Plane へ自分から仕事を取りに行く（pull）。Control Plane から Runner へは接続しない（requirements 2.3）。
- どの種類の仕事でも同じ段の枠（claim → prepare → work → check → 承認 → publish）で動かし、段ごとに契約 6.2 の表のとおりの隔離・鍵・接続を用意する（vision.md 3章）。
- 鍵を持つ段（prepare と publish）では AI を動かさず、作業の段には鍵と外への接続を渡さない、という境界を Runner の動作として守る（本人決定 4・7）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | claim の応答（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.3.1）、段の終了の報告への応答（04 の 3.4.1 の `next`）、heartbeat の応答（中止の指示、作業領域の削除の指示）、Runner の設定ファイル（4.1）、鍵の保管（[`01-architecture.md`](./01-architecture.md) 3.5）、スペシャルハーネスのリポジトリのローカルの複製 |
| 出力 | Runner 用 API への報告（result、events、artifacts、context、authorize、route-proposals の result）、作業領域、段ごとの `out` とログのファイル |

## 3. 処理詳細

### 3.1 起動

1. 設定ファイル（4.1）を読み、検査する。
2. 起動 ID（bootId）を作る。
3. 最初の heartbeat を送る。Control Plane はこれを契機に、前回の起動で動いていた Execution を `interrupted` にする（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.9）。

Control Plane の URL を設定で持つため、Control Plane を別のマシンへ移しても Runner の設定の変更だけで切り替えられる（requirements 2.3）。

### 3.2 pull ループ

```
loop:
  heartbeat（bootId、動いている Execution と振り分けの提案の ID、削除した作業領域の ID）
    ├ 応答に中止の指示があれば、該当の Execution・提案を止める（3.10）
    └ 応答に作業領域の削除の指示があれば、削除する（3.5）
  空きがあれば claim（作業領域のルートを添える）
    ├ job.kind = execution      → 段の枠を別の goroutine で始める（3.3）
    └ job.kind = route_proposal → 振り分けの提案を別の goroutine で始める（3.13）
  待機（間隔は実装の定数）
```

- Runner 側でも、同時に動かす Execution と提案を合わせて2本までに制限する。同時実行の制御の正本は Control Plane の claim（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.3）で、Runner 側の制限は二重の防御である。
- heartbeat と claim の間隔、強制終了までの猶予は実装の定数とする。heartbeat 途絶とみなす時間は 04 の 3.9 の30秒。

### 3.3 段の枠

1件の Execution を次の枠で進める。段の中身は Runner が決めない。Runner が行うのは、段ごとの準備、起動、見張り、結果の回収と報告である。

| # | 段・処理 | Execution 状態 | 動かすもの | Runner が行うこと | 詳細 |
|---|---|---|---|---|---|
| 1 | claim | `claimed` | なし | 応答から版・渡す値・作業領域・再開の情報を受け取る | 04 の 3.3 |
| 2 | 段の前の用意 | `claimed` | なし | Profile の中身の読み出し（3.4）、作業領域の用意（3.5）、鍵の有無の確認（3.6） | 3.4〜3.6 |
| 3 | prepare（任意） | `preparing` | Profile の固定版のプログラム | Prepare Sandbox を作って `command` を動かす。読み取り用の鍵と宣言した宛先だけを渡す | 契約 6.4、09 |
| 4 | work | `working` | ミクロハーネス | 固定した文脈を送って Execution Context を受け取り（[`07-context.md`](./07-context.md)）、Work Sandbox で Adapter を起動する。鍵は渡さない。外への接続はモデルの API だけ | 契約 6.3、08、09 |
| 5 | check | `checking` | Profile の固定版のプログラム | Check Sandbox を作って `command` を動かす。鍵も外への接続も渡さない | 契約 6.4、09 |
| 6 | 承認 | `waiting_approval` | なし | 何もしない。段のプロセスは終わっている。本人の承認の後、Control Plane が Execution を `pending` に戻し、Runner は claim で publish から再開する | 04 の 3.6、17 |
| 7 | publish | `publishing` | Profile の固定版のプログラム | Publish Sandbox を作って `command` を動かす。宣言した鍵と宛先を渡し、通話口で出す操作の許可を中継する（3.7） | 契約 6.4・7.4、09 |

各段は次の順で動かす。

1. `stage_started` を報告し、attempt を受け取る（04 の 3.4.1）。
2. 段の作業ディレクトリ（3.5）に要求 JSON（契約 6.1）を書き、成果物（kind=`request_json`）として登録する。要求 JSON の値は claim の応答と、前の段の終了の報告への応答から組み立てる。鍵の値は入れない。
3. 段の Sandbox を作る（[`09-sandbox.md`](./09-sandbox.md)）。見える範囲は契約 6.2 の表のとおりとし、`inputs/<段>/` には、同じ Execution で成功した前の段の `out` を読み取り専用で置く。
4. 段を動かし、上限時間と中止を見張る（3.10）。
5. 段の結果を回収する。プログラムの段は `EVALOOM_RESULT` のファイルを読む。作業の段は終了コード、セッション識別子、`out/summary.md`、`out/question.json` を読む（契約 6.3）。`out` の各ファイルは機密情報の除外（3.9）を通して成果物（kind=`out_file`、`work_summary`、`question`、`result_json`）として登録する。
6. `stage_finished` を報告する（04 の 3.4.1）。Runner は結果の合否を自分で判断しない。結果 JSON をそのまま送り、Control Plane が契約で確かめて遷移を決める。Runner が自分で決めるのは `runnerError`（04 の 3.4.1 の表）だけである。
7. 応答の `next` が段の名前なら次の段へ進み、`stop` なら、この Execution の段を終える。

- 最初の段は、Profile に `stages.prepare` があれば prepare、無ければ work とする。claim の `resume` があれば `resume.stage` から始める。`resume.completedStages` の段の `out` は、Runner のデータディレクトリに残したもの（3.5）を `inputs/<段>/` に置く。残っていないときの扱いは Q136。
- どの段でも失敗したら、以降の段を行わない。作業領域は削除せずに残す（契約 10.1）。
- Runner は段を自動でやり直さない（自動リトライは0回）。

### 3.4 Profile の中身の読み出し

Runner は、claim の `profile.commitSha` の中身だけを使う。スペシャルハーネスのリポジトリの作業ツリーとブランチは使わない（契約 4.2）。

1. `profile.repoPath` で、`profile.tagName` がいまも `profile.commitSha` を指していることを確かめる。違えば段を始めずに `config_error` にする（タグの付け替えの検出、契約 4.2）。タグが消えていても commit があれば続ける。
2. その commit を `git archive` でデータディレクトリの `profiles/<commitSha>/` に展開する。展開済みならそれを使う。展開したファイルは読み取り専用にし、Runner の実行中に書き換えない。
3. 展開した中身に、`definition` が参照するパス（契約 4.1）が実在し、シンボリックリンクを含まないことを確かめる。失敗したら `config_error`。
4. prepare・check・publish の段には展開した全体を `profile` として読み取り専用で見せる。work の段には `stages.work` で宣言したファイルだけを専用設定環境に置き、全体は見せない（契約 6.2・6.3）。

Runner はスペシャルハーネスのリポジトリに書き込まない。`git fetch` もしない（[`03-registry.md`](./03-registry.md) Q126）。

### 3.5 作業領域のルート

- Runner の設定 `workspaceRoot`（4.1）を、すべての作業領域のルートとする。claim の要求にこの値を入れ、Control Plane が `<workspaceRoot>/<作業領域の ID>` を作業領域のパスとして返す（04 の Q134）。
- Runner は作業領域を作る前と使う前に、パスのシンボリックリンクを解決した実パスが `workspaceRoot` の下にあることを確かめる。外に解決されたら段を始めずに `config_error` にする。
- 新しい作業領域（claim の `workspace.created` が `true`）は、権限 0700 の空のディレクトリとして作る。
- 引き継ぐ作業領域（`created` が `false`）のディレクトリが無ければ、最初の段の中身を動かさず、その段の終了を `runnerError: "workspace_missing"` で報告する。Control Plane は、再開の Execution なら `blocked`（cause=`workspace_missing`）にし、最初から始める Execution なら `failed`（`config_error`）にする（04 の 3.4.1）。
- 作業領域のパスはタスクの文（title、body など）や `params` から作らない。AI に実行先のパスを推測させない（requirements 3.3）。
- `workspaceRoot` は `dataDir` と重ねない。スペシャルハーネスのリポジトリの中にも置かない。
- 作業領域の削除は、Control Plane が heartbeat の応答で指示したものだけを行い、削除した ID を次の heartbeat で返す（02 の Q121、Q137）。判断待ちと承認待ちの作業領域は Control Plane が指示しない。

段ごとの作業ディレクトリは、データディレクトリの `executions/<Execution ID>/<段>-<attempt>/` に作る。中身は `request.json`、`result.json`、`out/`、`scratch/`、通話口のソケット（3.7）である。`scratch/` は段が終わったら消す。`out/` は Execution が終端になるまで残す（3.3 の再開に使う）。

### 3.6 鍵の受け渡し

鍵の保管と登録は [`01-architecture.md`](./01-architecture.md) 3.5 で定める。Runner は段を動かすときに次のとおり渡す。

1. 段を始める前に、Profile の `stages.<段>.secrets` に宣言したすべての鍵が保管（`<スペシャルハーネス ID>/<slot>`）にあることを確かめる。1つでも無ければ、段を始めずに `config_error` にする。
2. prepare の段には、登録時の使い方が `read` の鍵だけを渡す。Profile が `access: read` と宣言しても、`write` で登録した鍵は渡さず `config_error` にする。
3. work と check の段、振り分けの提案には、どの鍵も渡さない（ミクロハーネスのモデル認証を除く、[`08-harness-adapter.md`](./08-harness-adapter.md) 3.5）。
4. 渡し方は環境変数 `EVALOOM_SECRET_<slot を英大文字にした名前>` とする（契約 11章、slot の形は 03 の Q138）。コンテナの起動引数に値を書かない。値はランタイムの CLI の環境にだけ置き、起動引数には名前だけを渡す。要求 JSON、ファイル、ログにも書かない。
5. 鍵の値は段が終わるまで Runner のメモリにだけ持ち、段が終わったら捨てる。渡した値は、その段のログと成果物の機密情報の除外（3.9）の対象に加える。

### 3.7 通話口の中継

段の Sandbox の中に通話口（`EVALOOM_CHANNEL`、契約 7.4）を置き、段からの要求を Runner 用 API に中継する。方式は Unix ドメインソケットとする案である（01 の Q114）。

| 段 | 受け付ける要求 | Runner の処理 |
|---|---|---|
| prepare、check | `{ "op": "cancel_check" }` | 最新の heartbeat の応答で知っている中止の指示から `{ "cancelRequested": true \| false }` を返す。Control Plane には問い合わせない（01 の 5.2） |
| publish | `cancel_check` | 同上 |
| publish | `{ "op": "authorize", "operation", "approvalId", "contentId" }` | `POST /api/executions/{id}/authorize` に中継し、応答の `{ "allowed", "reason" }` をそのまま返す |
| work | なし | 通話口を置かない |

- 通話口は段ごとに作り、段が終わったら閉じる。
- 1行は 64 KiB までとする。表に無い `op`、段で使えない `op`、形の合わない要求、長すぎる行には応答せず、その接続を閉じる。段は応答が無いので操作しない（契約 7.4）。閉じた理由は `log` イベント（stream=`runner`）に残す。
- `authorize` の中継で Control Plane に届かない、10 秒以内に応答が無い、応答の形が合わないときは、`{ "allowed": false, "reason": "unavailable" }` を返す（Fail Closed、01 の 3.7）。Runner は自分で `allowed: true` を作らない。
- Runner は `contentId` と `operation` の意味を解釈しない。照合と承認の消費は Control Plane が1つの書き込みで行う（[`17-approval.md`](./17-approval.md)）。

### 3.8 ログ収集と成果物の登録

- 段のプロセスとミクロハーネスの標準出力と標準エラーを行単位で読み、機密情報の除外（3.9）を通してから次の2か所に出す。
  - `POST /api/executions/{id}/events` に `log` イベント（stream は段の名前、Runner 自身の出力は `runner`）として送る（管理画面の SSE 表示用）
  - データディレクトリのログファイルに追記し、段の終了時に `POST /api/executions/{id}/artifacts` で成果物（kind=`log`）として登録する
- 送信に失敗してもログファイルへの書き込みは続ける。送れなかった分は後でまとめて送る。
- 再送の照合: Runner は `log` イベントごとに Execution の中で一意な `sourceId` を付ける。Control Plane は既存の execution_events の payload の `sourceId` で重複を判定する。同じ `sourceId` で同じ内容の再送は保存せず成功を返し、内容が違えば 409 を返す。
- データディレクトリは Git の管理外に置く（requirements 3.1）。

#### 3.8.1 成果物の登録（`POST /api/executions/{id}/artifacts`）

管理 DB に書くのは Control Plane だけなので（[`01-architecture.md`](./01-architecture.md) 3.1）、Runner は成果物のファイルを API で送り、Control Plane が保存と登録を行う。Runner を別のマシンに置く将来の構成でもこの経路を変えない。

- 要求: 本文はファイルの中身そのもの（`application/octet-stream`）。クエリで `artifactId`（Runner が採番する ID）、`stage`、`kind`（02 の artifacts.kind の値）、`name`、`redacted`（`0` / `1`）を渡す。
- Control Plane は本文をデータディレクトリに保存し、`sha256` と `size_bytes` を自分で計算して artifacts に1行書く。
- 冪等性: 同じ Execution・同じ `artifactId` で同じ内容（sha256 が一致）の再送は登録済みとして成功を返す。内容が違えば 409 を返す。
- 権限: その Execution を claim した Runner だけが登録できる。ほかの Runner からの要求は 403 とし、監査記録を残す。
- 本文の大きさの上限は実装時に確定する。

### 3.9 ログと成果物の機密情報の除外（requirements 15.2、契約 11章）

- 次の値と完全一致する文字列を `[REDACTED]` に置き換える。Runner トークン、その段に渡した鍵の値、ミクロハーネスの認証ファイルの中のトークン。
- 加えて、よく知られた形式（`Authorization:` ヘッダ、`-----BEGIN ... PRIVATE KEY-----` など）を置き換える。パターンの一覧は実装時に確定する（Q24）。
- 対象は、ログ、`log` イベント、段の `out` のファイル、結果 JSON、作業の段の概要と質問である。結果 JSON は置き換えた後に Control Plane へ送る。
- 置き換えたものには artifacts.redacted=1 を付ける。置き換える前の内容は保存しない。

鍵を書かない義務はスペシャルハーネスにある（契約 11章・12章）。work と check の段には鍵が無い。この除外は、その前提が守られなかったときの二重の防御である。

### 3.10 タイムアウトと中止

#### タイムアウト

- 段ごとの上限時間は claim の `limits.timeoutSec`（Profile の `stages.<段>.timeoutSec`）とする（契約 10.2）。
- 上限を超えたら、プログラムの段には SIGTERM を送り、10 秒たっても終わらなければ SIGKILL を送り、Sandbox ごと止める。作業の段は Adapter のキャンセル（[`08-harness-adapter.md`](./08-harness-adapter.md)）でミクロハーネスのセッションを止める。
- `runnerError` を `timeout` にして段の終了を報告する。

#### 中止

- heartbeat の応答に中止の指示があれば、動いている段をタイムアウトと同じ手順で止め、`runnerError` を `cancelled` にして報告する（契約 10.1）。
- publish の段の通話口は、中止の指示を受けた後の `authorize` に Control Plane の応答（`cancelled`）を返す。段が `cancel_check` で中止を知って自分で終えた場合は、段の結果 JSON（`errorClass: "cancelled"`）をそのまま送る。

どちらの場合も作業領域は削除せずに残す。

### 3.11 使用量の記録

段の終了の報告の `usage` に、Runner が測った段の実行時間と、取れたときのトークン数と費用を入れる。作業の段は Adapter が返す値、プログラムの段は結果 JSON の `usage` を使う。Control Plane が usage_records に段ごとに記録する（requirements 12.5）。費用の上限による停止を行うかは未定（Q16）。

### 3.12 Runner が行わない操作

Runner は段の枠を動かすだけで、仕事の中身と外への操作を行わない。

- 外の状態を変える操作（push、PR の作成、マージ、コメントの投稿、メールの送信、デプロイなど）を、Runner 自身のコードで行わない。外へ出す操作は、publish の段のスペシャルハーネスのプログラムが、通話口の許可を受けて行う（契約 7.4）。
- Linear に接続しない。Linear への書き戻しは Control Plane が行う（[`16-linear-feedback.md`](./16-linear-feedback.md)）。
- Runner 自身が接続するのは Control Plane だけである。段の外への接続は、段ごとに Sandbox のネットワークの許可（[`09-sandbox.md`](./09-sandbox.md)）で絞る。
- AI を、Work Sandbox と Proposal Sandbox のほかで動かさない。prepare と publish の段にモデルの API の宛先を通さない（本人決定 4）。
- 段の合否を決めない。check の段の結果 JSON と、自分で得た値（終了コード、時間）だけを報告する（契約 15章の3）。作業中の AI の自己申告（ログの「テストが通った」など）を合否に使わない。
- `params`、`contentId`、`outputs`、`target` の意味を解釈しない。
- タスクの文や `params` をコマンドやパスとして使わない（契約 6.6）。
- 本人の HOME、本人の設定とメモリ、キーチェーン、SSH エージェントを段に見せない（契約 6.2）。
- `workspaceRoot` とデータディレクトリの外に書き込まない。スペシャルハーネスのリポジトリと、本人のほかのリポジトリを変えない。
- 承認を省く設定、Tool Permissions、Profile の版を変えない。変える経路を持たない（requirements 15.3）。
- 段を自動でやり直さない。止まった Execution を自動で再開しない。

### 3.13 振り分けの提案の実行

claim の `job.kind` が `route_proposal` のときは、段の枠を使わず、Proposal Sandbox で振り分けの提案を1回動かす（[`01-architecture.md`](./01-architecture.md) 3.1）。

- Proposal Sandbox には、鍵と、モデルの API のほかの外への接続を渡さない（本人決定 7）。
- 全体設定 `routing.proposer` のミクロハーネスとモデルを Adapter で起動し、Issue の本文を外から来た文として区切って渡す（[`15-linear-intake.md`](./15-linear-intake.md)）。
- 上限時間は claim の `proposal.timeoutSec`。中止とタイムアウトの止め方は 3.10 と同じとする。
- ログは機密情報の除外を通して `POST /api/route-proposals/{id}/artifacts` で登録し、結果を `POST /api/route-proposals/{id}/result` で返す。Runner は選ばれたラベルが候補にあるかを確かめない。確かめるのは Control Plane である（02 の route_proposals）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 仕事の受け取り方 | Runner からの pull だけ | requirements 2.3、8.4 |
| 同時実行（全体） | 2本。振り分けの提案を含む（正本は Control Plane の claim） | requirements 8.7、02 の Q111 |
| 同時実行（振り分け先ごと） | 1本（正本は Control Plane の claim） | requirements 8.7 |
| 段 | `prepare`（任意）→ `work` → `check` →（承認）→ `publish` | 本人決定 7、契約 2章 |
| 鍵を渡す段 | prepare（`read` だけ）と publish。どちらも AI を呼ばない | 本人決定 4・7 |
| 作業の段の外への接続 | モデルの API だけ | 本人決定 7 |
| 自動リトライ | 0回 | requirements 8.3 |
| 中断・失敗・中止の後の作業領域 | 保持する | requirements 8.3、契約 10.1 |
| 段の上限時間 | Profile の `stages.<段>.timeoutSec`。全体設定 `limits.stageTimeoutMaxSec` 以下 | 契約 10.2 |
| 止め方 | SIGTERM、10 秒後に SIGKILL | 契約 10.1 |
| Sandbox の資源上限 | 1つあたり CPU 4、メモリ 8 GiB（4.1 の `container.cpus` / `container.memoryMiB` の既定値） | Q68 |
| 隔離方式 | コンテナで隔離する。Profile が `stages.<段>.runtime` に `macos_vm` を宣言した段は macOS VM で隔離する（[`09-sandbox.md`](./09-sandbox.md) 3.4。macOS VM の実装までは、その Profile を登録で拒否する、Q147） | Q1 |

### 4.1 Runner の設定ファイル

Runner の設定は、起動時に `-config` で渡す JSON ファイル1つにまとめる。各部品の仕様が「Runner の設定で指定する」としている値の正本はこの表とする。

- パスはすべて絶対パスで書く。launchd から起動しても対話シェルから起動しても同じ意味になるようにするためである。
- 表に無いキーがあれば起動時にエラーで止める。必須のキーが無い場合も同じとする。既定値は表に書いたものだけで、ほかの値を暗黙に選ばない。
- 設定ファイルには秘密情報の値を書かない。秘密情報はファイルのパスで指す。

| キー | 型 | 必須 | 内容 | 出典 |
|---|---|---|---|---|
| `runnerId` | 文字列 | 必須 | Runner ID | 3.1 |
| `controlPlaneUrl` | 文字列 | 必須 | Control Plane の URL | 3.1、requirements 2.3 |
| `tokenFile` | パス | 必須 | Runner トークンのファイル | 3.1 |
| `workspaceRoot` | パス | 必須 | 作業領域のルート。`dataDir` と重ねない | 3.5 |
| `dataDir` | パス | 必須 | データディレクトリ（ログ、段の作業ディレクトリ、展開した Profile、ミクロハーネスの認証ファイル）。Git の管理外に置く | 3.4、3.5、requirements 3.1 |
| `secretsDir` | パス | 必須 | 鍵の保管（`<スペシャルハーネス ID>/<slot>` の 0600 のファイル）。`dataDir` の中に置く案 | 3.6、01 の 3.5・Q116 |
| `microHarnessAuthFiles` | オブジェクト（ミクロハーネスの名前 → パス） | 必須 | ミクロハーネスごとの Evaloom 専用の認証ファイル。パスは `dataDir` の中に置き、権限は 0600。Profile の `stages.work.microHarness` が表に無ければ `config_error` で止める | [`08-harness-adapter.md`](./08-harness-adapter.md) 3.5、Q139 |
| `container.runtimePath` | パス | 必須 | コンテナのランタイムの CLI | [`09-sandbox.md`](./09-sandbox.md) |
| `container.image` | 文字列 | 必須 | 段の Sandbox と Proposal Sandbox の既定のイメージ。work の段と Proposal Sandbox は常にこのイメージを使う。prepare・check・publish の段は、Profile が `stages.<段>.runtime` を宣言していればそのイメージ、無ければこのイメージを使う（契約 5.2、[`09-sandbox.md`](./09-sandbox.md) Q147） | [`09-sandbox.md`](./09-sandbox.md) |
| `container.proxyImage` | 文字列 | 必須 | ネットワークの許可先を絞るプロキシのイメージ | [`09-sandbox.md`](./09-sandbox.md) |
| `container.proxyBinary` | パス | 必須 | プロキシのコンテナに入れる補助プログラム（Linux 用にビルド済みのもの） | [`09-sandbox.md`](./09-sandbox.md) |
| `container.runtimeEnv` | オブジェクト（名前 → 値） | 任意 | ランタイムの CLI だけに渡す環境変数（例 `DOCKER_HOST`）。コンテナには渡さない | [`09-sandbox.md`](./09-sandbox.md) |
| `container.cpus` | 数値 | 任意（既定 4） | Sandbox 1つあたりの CPU 数の上限。0 以下は起動時にエラー | [`09-sandbox.md`](./09-sandbox.md)、Q68 |
| `container.memoryMiB` | 整数（MiB） | 任意（既定 8192） | 同じくメモリの上限。swap への超過は許さない。0 以下は起動時にエラー | [`09-sandbox.md`](./09-sandbox.md)、Q68 |

heartbeat と claim の間隔、通話口の応答の待ち時間（10 秒）、強制終了までの猶予（10 秒）など、本人が変える必要のない値は設定に出さず、実装の定数とする。

## 5. インターフェース

- Runner 用 API: `POST /api/runners/{id}/claim`、`POST /api/runners/{id}/heartbeat`、`POST /api/executions/{id}/events`、`POST /api/executions/{id}/artifacts`、`POST /api/executions/{id}/result`、`POST /api/executions/{id}/context`、`POST /api/executions/{id}/authorize`、`POST /api/route-proposals/{id}/artifacts`、`POST /api/route-proposals/{id}/result`（[`01-architecture.md`](./01-architecture.md) 5.2）
- イベント種別: [`02-data-model.md`](./02-data-model.md) 5.3
- 本人用の参照: `GET /api/executions/{id}/events`（SSE）、`GET /api/executions/{id}/logs`、`GET /api/runners`
- 段とのやり取り: 契約 6.1（要求 JSON）、6.5（結果 JSON）、7.4（通話口）

heartbeat の要求と応答は次のとおりとする。

```json
// 要求
{ "bootId": "...", "runningExecutionIds": ["..."], "runningRouteProposalIds": ["..."], "cleanedWorkspaceIds": ["..."] }
// 応答
{ "cancelExecutionIds": ["..."], "cancelRouteProposalIds": ["..."], "cleanWorkspaceIds": ["..."] }
```

`cancelRouteProposalIds` は Q133、`cleanWorkspaceIds` と `cleanedWorkspaceIds` は Q137 の案である。

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Control Plane に接続できない | 動いている段は続ける。報告は再送待ちに積む。通話口の `authorize` には `allowed: false`（`unavailable`）を返す（Fail Closed） |
| タグの付け替え、Profile の中身を読めない、参照するパスが無い | 段を始めずに `config_error` |
| 宣言した鍵が保管に無い、`write` の鍵を prepare に求めた | 段を始めずに `config_error` |
| 作業領域のパスが `workspaceRoot` の外に解決される | 段を始めずに `config_error` |
| 引き継ぐ作業領域が無い | 再開なら Control Plane が `blocked`（`workspace_missing`）、最初から始める Execution なら `config_error`（3.5） |
| Sandbox の作成の失敗 | `sandbox_error` |
| プログラムの段の結果のファイルが無い・JSON として読めない | `contract_error`（終了コードにかかわらず、契約 6.4） |
| ミクロハーネスの異常終了 | `work_failed` |
| ミクロハーネスの認証ファイルが設定に無い | `config_error` |
| ログ送信の失敗 | ファイルへの書き込みは続け、後で再送する |
| 成果物の登録の失敗 | ローカルのファイルを残し、同じ `artifactId` で後で再送する |
| Runner 自身の異常終了 | 再起動の後の最初の heartbeat で Control Plane が `interrupted` にする |

## 7. スコープ外

- 複数 Runner、別のマシンでの Runner（Phase 6。ただし pull 方式と設定による接続先の切り替えは最初から満たす）
- 評価の実行（評価と改善の Phase）
- 費用の上限による自動停止（Q16 で決まるまで）
- 段の中身（開発なら worktree の作成、setup、テスト、push、PR の作成）。スペシャルハーネスが行い、software-engineer へ移した（本人決定 1）

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約（段、要求 JSON、結果 JSON、通話口、鍵、中止）
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) Runner 用 API、構成要素と権限、鍵の保管
- [`02-data-model.md`](./02-data-model.md) executions、task_workspaces、stage_runs、execution_events、artifacts、usage_records
- [`03-registry.md`](./03-registry.md) Profile の版の登録
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) claim、段の報告、再開
- [`07-context.md`](./07-context.md) 作業の段の前の文脈の固定
- [`08-harness-adapter.md`](./08-harness-adapter.md) ミクロハーネスの起動と認証ファイル
- [`09-sandbox.md`](./09-sandbox.md) 段ごとの Sandbox とネットワークの許可
- [`15-linear-intake.md`](./15-linear-intake.md) 振り分けの提案の中身
- [`17-approval.md`](./17-approval.md) 出す操作の許可と承認の消費

## 9. 実装単位

- [ ] [Backend] Runner の設定ファイルを 4.1 の表どおりに読む（必須・既定値・未知のキーの検査、パスの検査）
  - 受け入れ基準: 4.1 の必須キーが1つでも欠けた設定、表に無いキー（例 `worktreeRoot`）を含む設定、相対パスを含む設定で、`evaloom-runner` が起動時にエラーで終わる; `workspaceRoot` と `dataDir` が重なる設定を起動時に拒否する; `microHarnessAuthFiles` のパスが `dataDir` の外にある設定を起動時に拒否する; `container.cpus` と `container.memoryMiB` を省くと 4 と 8192 が使われ、0 以下で起動時にエラーになる
- [ ] [Backend] bootId の生成、heartbeat の送信と、中止・作業領域の削除の指示の受け取り
  - 受け入れ基準: 起動直後に bootId 付きの heartbeat が送られる; 応答の cancelExecutionIds の Execution の中止が呼ばれる; 応答の cleanWorkspaceIds の作業領域が消され、次の heartbeat の cleanedWorkspaceIds に入る（Q137 の決定後）; Control Plane の URL を設定で差し替えられる
- [ ] [Backend] pull ループと Runner 側の同時実行の上限（Execution と振り分けの提案）
  - 受け入れ基準: Runner 側で同時に動く Execution と提案の合計が2本を超えない; 仕事が無いときに claim を繰り返しても何も始まらない; job.kind に応じて段の枠と提案の実行が呼び分けられる
- [ ] [Backend] Profile の中身の読み出し（3.4）
  - 受け入れ基準: タグが別の commit を指すように付け替えたリポジトリで、段を始めずに config_error になる; 展開した中身が読み取り専用で、作業ツリーの変更（commit していないファイル）が展開に入らない; 参照するパスがシンボリックリンクだと config_error
- [ ] [Backend] 段の枠の実行器（3.3。段の順、`next` による進行、再開、失敗時の打ち切り）
  - 受け入れ基準: 各段をスタブにしたテストで、prepare の宣言がある Profile は prepare → work → check → publish、無い Profile は work から始まる; `next=stop` で以降の段を動かさない; resume.stage=publish のとき publish だけを動かし、`inputs/check/` に前の check の out が置かれる; resume.stage=work のとき要求 JSON の `answer` が入る; 途中の段が失敗すると後続が呼ばれず、作業領域が残る; 要求 JSON が契約 6.1 の形で、鍵の値を含まない
- [ ] [Backend] 作業領域のルート（3.5）
  - 受け入れ基準: シンボリックリンクで `workspaceRoot` の外を指す作業領域が config_error になる; 新しい作業領域が 0700 で作られる; 再開の Execution で作業領域が無いとき、Control Plane が blocked（workspace_missing）にする
- [ ] [Backend] 鍵の受け渡し（3.6）
  - 受け入れ基準: 宣言した鍵が保管に無いと段を始めず config_error; `write` で登録した鍵が prepare の段に渡らない; work・check の段と提案の環境に `EVALOOM_SECRET_` で始まる変数が無い; コンテナの起動引数（`ps` で見える値）に鍵の値が無い; 段が鍵の値を標準出力に出すテストで、ログと events と成果物に値が残らず `[REDACTED]` になる
- [ ] [Backend] 通話口の中継（3.7）
  - 受け入れ基準: prepare と check の通話口で authorize を送ると応答が無く接続が閉じる; publish の authorize が Control Plane の応答どおりに返る; Control Plane を止めた状態の authorize が allowed=false（unavailable）になる; 64 KiB を超える行で接続が閉じる; work の段に `EVALOOM_CHANNEL` が無い
- [ ] [Backend] ログ収集（行単位の読み取り、events 送信、ファイル保存、成果物の登録、送信失敗時の再送）と成果物の登録 API（3.8）
  - 受け入れ基準: 段の出力が SSE で管理画面に届く; Control Plane を止めている間もログファイルに書かれ、再開の後に送られる; 同じ artifactId・同じ内容の再送で行が増えず、内容が違う再送は 409; ほかの Runner からの登録は 403
- [ ] [Backend] タイムアウトと中止（3.10）
  - 受け入れ基準: timeoutSec を短くした Profile で段が SIGTERM、10 秒後に SIGKILL で止まり timeout が報告される; 中止の指示で cancelled が報告される; どちらも作業領域が残る
- [ ] [Backend] 使用量の記録（3.11）
  - 受け入れ基準: 段ごとに usage_records に実行時間が記録される; トークン数が取れないときは NULL になる
- [ ] [Backend] 振り分けの提案の実行（3.13）
  - 受け入れ基準: Proposal Sandbox に鍵が無く、モデルの API のほかへ接続できない; 結果が route-proposals の result で届く; 上限時間を超えると止まる
- [ ] [Backend] 起動コマンドから段の枠への接続の結合テスト
  - 受け入れ基準: `backend/cmd/evaloom-runner` が claim した Execution を段の枠で最後まで動かす; 実際の Control Plane のハンドラと SQLite に対して、テスト用のスペシャルハーネスのリポジトリ（prepare・check・publish が決まった結果 JSON を書くだけのプログラム、`profile/v1.0.0` のタグ）とダミーのミクロハーネス（`adapter/dummy`）で、claim から succeeded までを1本通す。置き換えてよいのは、コンテナのランタイム（偽 runtime）と Adapter（`adapter/dummy`）だけとする; 承認を求める check の結果を返すテストで、Execution が waiting_approval で止まり、本人が承認した後の claim で publish だけが動き、通話口の authorize が allowed=true を1回だけ返す; 質問を書く作業の段のテストで blocked になり、回答の後の claim で work から再開する

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q136 | 本人の判断の後に再開するとき、前の段の `out` が Runner のデータディレクトリに残っていない場合（Runner の入れ替えやディスクの掃除） | 作業領域が無いときと同じく `blocked`（cause=`workspace_missing`、abort だけ）にする。本人は fresh で再実行する。Runner が Control Plane から成果物を取り戻す API は作らない（Runner 用 API を増やさないため） |
| Q137 | 作業領域の削除（02 の Q121）を Runner にどう伝え、どう報告させるか | heartbeat の応答に `cleanWorkspaceIds` を載せ、Runner が削除した ID を次の heartbeat の `cleanedWorkspaceIds` で返す。Control Plane はそれを受けて task_workspaces を `cleaned` にする。Runner 用 API のエンドポイントは増やさない |
| Q139 | ミクロハーネスの Evaloom 専用の認証ファイルの単位（旧仕様はスペシャルハーネスの ID ごと） | ミクロハーネスごと（`claude_code`、`codex`）に1つ持つ。スペシャルハーネスは鍵を持たない約束（契約 11章）であり、モデルの認証はミクロハーネスの側の値であるため。スペシャルハーネスごとに課金を分けたくなったら、そのとき単位を足す |
