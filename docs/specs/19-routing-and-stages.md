---
status: draft
written: 2026-11-10
範囲: 振り分け先（Project とスペシャルハーネス）、スペシャルハーネスの登録と版の固定、段と段ごとの安全な場所、中身の識別子と承認の結び付け、鍵の受け渡し、承認の宣言、結果の受け取りと返却、記録の受け渡し、受け入れ条件の要否、同時実行の単位、software-engineer への切り替えの順番
---

# 19. 振り分け先・段・安全な場所 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend, Frontend ｜ **範囲**: 仕事を振り分ける先と、振り分けた仕事を段ごとに動かす安全な場所の全体像

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて書き直した。前の版（仕事の種類 `code` / `artifact` を Evaloom が持つ形）は、vision.md と前提が逆だったため捨てた。この文書は Phase の追加ではなく、Evaloom の中心の仕様である。

この文書は全体のつながりを示す。名前・表・列挙値は [`02-data-model.md`](./02-data-model.md)、API は [`01-architecture.md`](./01-architecture.md)、スペシャルハーネスとの約束はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）を正本とし、この文書では定義し直さない。

## 1. 目的

- 仕事を受け取ってから返すまでのうち、Evaloom が受け持つ部分（振り分け、順番待ち、承認、返す）と、スペシャルハーネスが受け持つ部分（準備・作業・確かめる・出す）の境目を示す（vision.md 3章）。
- 振り分け先を、Project とスペシャルハーネスの2種類にする。リポジトリを持たない仕事は、スペシャルハーネスに直接振り分ける（vision.md 4章）。
- 段ごとに、Evaloom が用意する安全な場所（隔離、書ける範囲、外への接続、固定した版からの読み出し、鍵）を示す（vision.md 3章、本人決定 4・7）。
- Evaloom が仕事の種類に固有の情報を持たないことを、この文書の範囲で守る。開発の中身（worktree、テスト、push、PR）は software-engineer へ移す（本人決定 1）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | Linear の Issue と手動のタスク（[`15-linear-intake.md`](./15-linear-intake.md)、[`04-task-lifecycle.md`](./04-task-lifecycle.md)）、振り分けの表（routes）、スペシャルハーネスの Profile 定義（契約 5章）、段の結果 JSON（契約 6.5） |
| 出力 | 振り分け先が決まったタスク、段ごとの Sandbox、承認待ちと本人の判断待ち（owner_decisions）、Linear への書き戻し、スペシャルハーネスの記録（harness_records） |

## 3. 処理詳細

### 3.1 振り分け先

Evaloom が仕事を頼む相手は、次の2種類である（契約 0章）。

| 振り分け先 | 中身 | 使う場面 |
|---|---|---|
| Project | 6項目（名前、振り分けのラベル、頼むスペシャルハーネス、渡す値、承認を省く設定、Runner）。表は 02 の projects | 同じスペシャルハーネスを、対象（リポジトリなど）ごとに違う値で使うとき |
| スペシャルハーネス | スペシャルハーネスそのもの。渡す値は `{}`、版は既定の版（harnesses.default_profile_id） | 対象を持たない仕事、または対象をスペシャルハーネス自身が決める仕事 |

- 振り分けの表（02 の routes）は、ラベルから振り分け先を1つ引く。ラベルは表の中で一意で、振り分け先は Project かスペシャルハーネスのどちらか一方である。1つの Project、1つのスペシャルハーネスに付けられるラベルは1つだけとする。
- 振り分けのラベルは、全体設定 `linear.routingLabelPrefix`（既定 `route:`、Q120）で始まる名前にする。Project にもスペシャルハーネスにも同じ接頭辞を使う。
- 振り分けに Linear の Team は使わない（本人決定 2）。照会する Team は全体設定 `linear.teamId` の1つだけである。
- Issue に付いた振り分けのラベルがちょうど1つで、表にあれば、振り分け先が決まる。ラベルが無い、表に無い、2つ以上のときは、AI が鍵の無い隔離（Proposal Sandbox）で振り分け先を1つ提案し、本人が管理画面で承認するまで実行しない（vision.md 3章）。取り込みと提案の手順は [`15-linear-intake.md`](./15-linear-intake.md) 3.6・3.6.4、判断の形は 02 の 5.6 に従う。
- 振り分け先は、タスクを `queued` にした時点で決まり、その後は変えない（02 の tasks）。
- Project とスペシャルハーネスの登録と、振り分けの表の編集は [`03-registry.md`](./03-registry.md) 3.1〜3.3 と [`13-management-ui.md`](./13-management-ui.md) 3.5〜3.7 で定める。

### 3.2 スペシャルハーネスの登録と版の固定

- スペシャルハーネスは、独立した Git リポジトリとして登録する（契約 4章、requirements 原則2）。Evaloom が読むのは `evaloom/profile.yaml` と、そこから参照されたファイルだけである。
- 版は、タグ `profile/v<SemVer>` を登録時に commit SHA へ解決して記録する（02 の harness_profiles、契約 4.2）。公開したタグの付け替えは認めない。
- Profile 定義の `contractVersion` が、Evaloom が対応する契約の版（02 の 4.3）に無ければ登録を拒否する（契約 3章）。
- Project は profile_id で版を固定する。新しい版を登録しても、Project の版は自動では変わらない（requirements 4.3）。スペシャルハーネスへ直接振り分けた仕事は、harnesses.default_profile_id の版を使う。既定の版が無ければ取り込みを保留する（02 の 5.7 `config_missing_profile`）。
- 版は claim の時点で Execution に固定する（02 の executions）。実行に使うのは、登録時に記録した commit の中身だけである。
- 登録の手順と検証は [`03-registry.md`](./03-registry.md) 3.3・3.4 で定める。

### 3.3 段と安全な場所

1回の Execution は、次の段で進む（02 の 4.1、契約 2章）。

```
[prepare]（任意） → [work] → [check] → （承認） → [publish]
```

| 段 | 中身を決めるもの | 動かすもの | Evaloom が用意する場所の要点 |
|---|---|---|---|
| prepare | スペシャルハーネス | 固定版のプログラム。AI を呼ばない | 作業領域に書ける。読み取り用の鍵と、宣言した宛先だけ |
| work | スペシャルハーネス（指示・スキル・設定） | ミクロハーネス（Claude Code・Codex） | 作業領域に書ける。鍵は無い。外への接続はモデルの API だけ。確かめる段と出す段のプログラムは見せない |
| check | スペシャルハーネス | 固定版のプログラム。AI を呼ばない | 作業領域は読むだけ。鍵も外への接続も無い |
| publish | スペシャルハーネス | 固定版のプログラム。AI を呼ばない | 作業領域は読むだけ。宣言した鍵と宛先だけ。外へ出す操作の直前に毎回 Evaloom に問い合わせる |

- 段ごとに見える範囲の正本は契約 6.2 の表である。上の表は要点だけを示す。
- Evaloom が段ごとに行うのは次のことである。どれも仕事の種類によって変わらない（vision.md 5章の基準）。
  1. 段ごとに Sandbox を作り直す。前の段の Sandbox を使い回さない（[`01-architecture.md`](./01-architecture.md) 3.2）。
  2. Profile の固定版を、登録時に記録した commit から読み出して置く。作業の段には `stages.work` で宣言したファイルだけを、専用設定環境に置く（[`07-context.md`](./07-context.md) 3.2、[`08-harness-adapter.md`](./08-harness-adapter.md) 3.5）。
  3. 書ける範囲を、契約 6.2 のとおりに絞る。作業領域（task_workspaces）はタスクごとに1つで、確かめる段と出す段では読み取り専用にする。
  4. 外への接続を、契約 6.2 のとおりに絞る。どの段からも Control API、管理 DB、ホストのループバックに届かないようにする。実現方法は [`09-sandbox.md`](./09-sandbox.md) で定める。
  5. 段の間の受け渡しは、作業領域と、Runner が成果物として保存した `out` だけにする。
- 準備の段は任意である。Profile に `stages.prepare` が無ければ行わない（本人決定 7）。
- 作業の段の AI が判断できないことを質問として残したら、Evaloom は確かめる段へ進まず、本人の判断待ち（owner_decisions の kind=`question`）にする。本人が管理画面で答えたら、同じセッションを再開して答えを渡す（契約 8章、[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.6、[`08-harness-adapter.md`](./08-harness-adapter.md) 3.4）。
- 段の実行手順は [`05-runner.md`](./05-runner.md)、状態の遷移は 02 の 5.2 で定める。

### 3.4 中身の識別子と承認の結び付け

- 確かめる段は、確かめた中身の識別子（`contentId`）を報告する。Evaloom は executions.content_id に記録する。値の意味は解釈しない（02 の 3.1、契約 6.5）。
- 確かめたものと出すものが同じであることは、スペシャルハーネスが見張る（本人決定 5、契約 12章 3）。Evaloom は、確かめる段と出す段の識別子を照合しない。
- Evaloom が比べるのは、承認を求めたときの識別子と、出す段が操作の直前に送る識別子だけである（契約 7.2・7.4）。違えば承認を無効にして、操作を許可しない。詳細は [`17-approval.md`](./17-approval.md) 3.4・3.5。

### 3.5 鍵の保管と受け渡し

- スペシャルハーネスが外とやり取りするときに使う鍵は、Evaloom が保管する（vision.md 6章）。スペシャルハーネスは、鍵の名前（slot）と使い方（`read` / `write`）を Profile に宣言するだけで、値を持たない（契約 11章）。
- 鍵を渡すのは、宣言した準備の段と出す段だけである。準備の段には読み取り用の鍵だけを渡す。作業の段と確かめる段には、どの鍵も渡さない（本人決定 4・7）。
- 保管の場所、登録の方法、渡し方は [`01-architecture.md`](./01-architecture.md) 3.5 で定める。Linear の API キーは Control Plane だけが使い、スペシャルハーネスには渡さない。

### 3.6 承認の宣言

- 本人の承認が要る操作は、スペシャルハーネスが Profile の `approvals` に宣言する（契約 7.1）。Evaloom は、宣言していない操作の承認を受け付けない。
- 承認の画面と記録は Evaloom が持つ。本人の判断は管理画面だけで受け付け、Linear には「管理画面で判断待ち」と知らせるだけにする（本人決定 3）。
- Project は、`waivable: true` と宣言された操作だけを、承認を省く設定（projects.approval_waivers）に入れられる（契約 7.3）。スペシャルハーネスへ直接振り分けた仕事では、スペシャルハーネスの登録の承認を省く設定（harnesses.approval_waivers）を使う。既定は空で、すべての操作で止まる。本人が、もう止めなくてよい操作を後から足していく（Q113）。
- 承認の作成、照合、消費、有効期限は [`17-approval.md`](./17-approval.md) 3.3〜3.6 で定める。

### 3.7 結果の受け取りと返却

- Evaloom は、段の結果を結果 JSON（契約 6.5）と、自分で得た値（終了コード、使用量、時間）だけから決める（契約 15章 3）。作業中の AI の自己申告は使わない。
- 出す段が報告した外に出したもの（`outputs`）は、executions.outputs に記録し、表示と次の Execution への受け渡しにだけ使う。中身は解釈しない。
- 結果を返すのは Evaloom の役目である（vision.md 3章の 8）。返し方は次の仕様で定める。
  - Linear へのコメントと状態の変更、振り分けのラベルの付与: [`16-linear-feedback.md`](./16-linear-feedback.md) 3.2・3.6・3.6.5
  - macOS 通知: [`12-notification.md`](./12-notification.md)
  - 管理画面: [`13-management-ui.md`](./13-management-ui.md) 3.3・3.4
- Linear に結果をコメントするのは Evaloom であり、スペシャルハーネスの出す段ではない。スペシャルハーネスは、載せたい内容を `out/summary.md`（作業の段）や結果 JSON の `summary`（確かめる段・出す段）に書く（契約 6.3・6.5）。

### 3.8 記録と次の仕事への受け渡し（2026-11-09 本人決定、案A）

スペシャルハーネスの成果物を Evaloom が記録として持ち、同じスペシャルハーネスの次の仕事に参考として渡す（vision.md 3章）。スペシャルハーネスのリポジトリにも、その外の別のフォルダにも書かない。

**保存**

1. Profile の `records.keep` に宣言したファイルを、出す段まで成功した Execution のものだけ、スペシャルハーネスの記録（02 の harness_records）として保存する（契約 9章）。
2. 記録はスペシャルハーネス単位で持ち、Profile の版をまたぐ。Project をまたいでも同じスペシャルハーネスの記録として持つ（Q171）。
3. 作業の段の出力は記録にしない。固定版のプログラムが `out` に書いたものに限る（契約 9章）。

**受け渡し**

1. claim の時点で、Control Plane が同じスペシャルハーネスの記録から、消していないものを新しい順に `records.pass.recent` 件選ぶ。
2. Runner が作業の段の `records/` に読み取り専用で置く。ファイル名は契約 9章のとおりとする。
3. 渡した記録の ID とハッシュを context_revisions（layer=`harness`、kind=`record`）に残し、どの記録を見て作業したかを後から追えるようにする（[`07-context.md`](./07-context.md) 3.3）。
4. 記録は指示ではなく参考資料として渡す（契約 6.6）。

**本人の操作**

- 管理画面で記録を一覧し、消せる（`GET /api/harnesses/{id}/records`、`DELETE /api/harness-records/{id}`、01 の 5.1）。消した記録は次の仕事に渡さない。

### 3.9 受け入れ条件の要否

- 受け入れ条件を必須にするかは、振り分け先の Profile が `acceptanceCriteria`（`required` / `optional`）で宣言する（契約 5.2）。Project の設定では変えない（Q172）。
- `required` の振り分け先で受け入れ条件が無いタスクは、実行せずに保留する（02 の 5.7 `missing_acceptance_criteria`）。Linear からの取り込みでは [`15-linear-intake.md`](./15-linear-intake.md) 3.6.1〜3.6.3、手動作成では [`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.1 で扱う。
- `optional` では、受け入れ条件の見出しが無くても保留にしない。見出しがあれば取り出して tasks.acceptance_criteria に入れ、無ければ空文字列にする。
- 振り分け先が決まる前（`routing` の間）は判定しない。本人が振り分けを承認した後に判定する。

### 3.10 同時実行の単位

- 同時に動かす Execution は、全体で2本、振り分け先ごとに1本までとする（02 の 4章、requirements 8.7）。
- 振り分け先は、Project なら Project ごと、スペシャルハーネスへの直接の振り分けならスペシャルハーネスごとに数える。同じスペシャルハーネスを使う2つの Project は、別の振り分け先として数える。
- 承認待ちと本人の判断待ちの Execution は枠に数えない（02 の 5.2）。
- 振り分けの提案は全体2本の枠に数え、振り分け先ごとの枠には数えない（Q111）。
- claim での数え方は [`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.3 で定める。

### 3.11 software-engineer への切り替え

開発の仕事の中身は、スペシャルハーネス software-engineer へ移す（本人決定 1）。いまの実装との食い違いは扱わず、仕様は目標の形で書く。現状は何も運用で動いていないため、切り替えまでの間の扱いを Evaloom の仕様には残さない。

移したもの（一覧の正本は `~/workspace/harnesses/software-engineer/docs/design.md` の「evaloom から移すもの」）。

- 仕様 06（worktree）、10（検証 Gate）、11（Git Publisher）の全文。2026-11-10 に software-engineer の `docs/from-evaloom/` へ写し、evaloom からは消した。
- requirements 8.5・8.6・11.2・15.4 と 15.7 の GitHub CI。受け入れ条件 12・15・18。
- 前の 03 のコピー許可リスト、setup の許可宛先、テストコマンド。前の 07 の Project Context（`AGENTS.md` の固定）。前の 08 の 3.3 の 6（commit と差分の取得）。前の 09 の 3.6（Git 領域の防御）と 3.8（GitHub CI）。前の 14 の Gate と Git フックのテスト。前の 17 の 3.5〜3.8（GitHub のマージ、直接 push、ルールセット）。

移す前の版は、evaloom の git 履歴（main の 558549e）で読める。

切り替えの順番は次のとおりとする（design.md の「進める順番」と同じ）。

1. evaloom の requirements と仕様を vision.md に合わせ、ハーネス契約を切り出す（この書き直し）。
2. Evaloom の古い開発の部品（worktree、Gate、Git Publisher のコードと、projects の開発の列）を消す。運用で動いているものがまだ無いため、software-engineer への切り替えを待たない（Q170、2026-11-10 本人決定）。
3. Evaloom 側で、契約に従う段の仕組み（3.3〜3.8）を作る。契約に合っているかを、試験用の最小のスペシャルハーネスで確かめる（Q173）。
4. software-engineer を別のリポジトリとして作り、契約の決まった版だけを読んで段のプログラムを書く。
5. software-engineer の最初の版を Evaloom に登録し、開発の Project を software-engineer に振り分けるようにする。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 振り分け先 | Project かスペシャルハーネスの一方 | vision.md 3章・4章、02 の routes |
| 振り分けのキー | ラベルだけ。Linear Team は使わない | 本人決定 2 |
| 振り分けのラベルの接頭辞 | `route:`（全体設定 `linear.routingLabelPrefix`） | Q120 |
| 段 | `prepare`（任意）→ `work` → `check` → `publish` | 本人決定 7、02 の 4.1 |
| 鍵を受け取る段 | `prepare`（読み取り用だけ）と `publish`。どちらも AI を呼ばない | 本人決定 4・7 |
| 確かめたもの＝出すもの | スペシャルハーネスが見張る | 本人決定 5 |
| 承認と中身の結び付け | Evaloom が比べる | 本人決定 5、17 |
| 本人の判断の場所 | 管理画面だけ | 本人決定 3 |
| 受け入れ条件の要否 | Profile の `acceptanceCriteria` | 契約 5.2 |
| 同時実行の上限 | 全体2本、振り分け先ごとに1本 | requirements 8.7 |
| 記録の単位 | スペシャルハーネスごと。Profile の版をまたぐ | 契約 9章 |
| 記録を渡す件数 | Profile の `records.pass.recent`（0〜50） | 契約 5.1 |

## 5. インターフェース

この文書で新しく定める API、表、列挙値は無い。

- API: 振り分けの表（`/api/routes`）、本人の判断（`/api/decisions`）、記録（`/api/harnesses/{id}/records`、`/api/harness-records/{id}`）、出す段の問い合わせ（`POST /api/executions/{id}/authorize`）は [`01-architecture.md`](./01-architecture.md) 5章。
- 表: projects、harnesses、harness_profiles、routes、tasks、executions、task_workspaces、stage_runs、owner_decisions、approvals、harness_records、context_revisions は [`02-data-model.md`](./02-data-model.md) 3章。
- Profile 定義、要求 JSON、結果 JSON、通話口は契約 5章・6章・7.4。

## 6. エラー処理

| 状況 | 扱い | 定める仕様 |
|---|---|---|
| 振り分けのラベルが無い・表に無い・2つ以上 | 振り分けの提案に回し、本人の承認を待つ（保留にはしない） | 15、02 の 5.6 |
| スペシャルハーネスへ直接振り分けたが既定の版が無い | 取り込みを保留する（`config_missing_profile`） | 02 の 5.7 |
| `required` の振り分け先で受け入れ条件が無い | 取り込みを保留する（`missing_acceptance_criteria`） | 02 の 5.7、15 |
| Profile が宣言した鍵が保管に無い | 段を始めず `config_error` | 01 の 3.5 |
| 段の結果 JSON が無い・形が合わない | `contract_error` | 契約 6.4、02 の 5.5 |
| 承認したときと出す直前で識別子が違う | 承認を無効にして操作を許可しない（`content_changed`） | 契約 7.4、17 |
| 記録のファイルが `out` に無い | その記録だけを残さず、Execution の結果は変えない。理由を実行ログ（execution_events の `log`）に残す（Q174） | この文書 |

## 7. スコープ外

- 仕事の中身（準備・作業・確かめる・出すで何をするか）。スペシャルハーネスが決める（vision.md 4章）。
- 開発の仕事の中身（worktree、テスト、push、PR、マージ、ブランチ）。software-engineer へ移した（3.11）。
- Linear 以外のきっかけ（定期実行など）で仕事を受け取ること（vision.md 3章「後で足す」）。
- 記録を内容の近さで選ぶこと。いまは新しい順に件数だけで選ぶ。

前の版から次を消した。どれも vision.md と前提が逆だったためである。

- 仕事の種類（`workKind`、`code` / `artifact`）と、種類ごとの工程表。段は種類によらず 3.3 の4つで、中身はスペシャルハーネスが決める。
- Evaloom が持つ出口の実装（`linear_comment`、`typefully_draft`）。外へ出すのはスペシャルハーネスの出す段であり、Linear への結果の返却は Evaloom の 16 で行う（3.7）。
- スペシャルハーネスへ振り分けるラベルの接頭辞 `harness:`。振り分けのラベルは `route:` の1種類にした（Q120）。
- harnesses の `linear_team_id` と `linear_routing_label` の列。振り分けの表（routes）に置き換え、Team は使わない（本人決定 2）。

## 8. 関連仕様

- [`../vision.md`](../vision.md) Evaloom とは（正本）
- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) 段ごとの Sandbox、鍵の保管、API
- [`02-data-model.md`](./02-data-model.md) 表と列挙値
- [`03-registry.md`](./03-registry.md) Project・スペシャルハーネス・Profile・振り分けの表の登録
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 状態の遷移、同時実行、本人の判断の後の再開
- [`05-runner.md`](./05-runner.md) 段の実行手順
- [`07-context.md`](./07-context.md) 固定した文脈と記録の受け渡しの記録
- [`08-harness-adapter.md`](./08-harness-adapter.md) ミクロハーネスの Adapter と専用設定環境
- [`09-sandbox.md`](./09-sandbox.md) 段ごとの Sandbox の実現方法
- [`15-linear-intake.md`](./15-linear-intake.md) 取り込みと振り分けの提案
- [`16-linear-feedback.md`](./16-linear-feedback.md) Linear への書き戻し
- [`17-approval.md`](./17-approval.md) 承認
- `~/workspace/harnesses/software-engineer/docs/design.md` software-engineer へ移したものの一覧

## 9. 実装単位

- [ ] [Backend] 段の組み立て（`internal/stage`）。Profile の段の宣言から、段ごとの Sandbox の見える範囲・書ける範囲・外への接続・鍵を組み立てる（3.3、3.5）
  - 受け入れ基準: Profile に prepare が無いと準備の段を行わない; 作業の段の Sandbox に `stages.work` で宣言していない Profile のファイル（確かめる段のプログラムを含む）が無い; 確かめる段と出す段から作業領域に書けない; 作業の段と確かめる段にどの鍵も渡らない; 準備の段に `write` の鍵が渡らない
- [ ] [Backend] 受け入れ条件の要否の判定（3.9）
  - 受け入れ基準: `required` の振り分け先で受け入れ条件の無いタスクが `missing_acceptance_criteria` で保留になる; `optional` では保留にならず acceptance_criteria が空文字列になる; `routing` の間は判定しない
- [ ] [Backend] 振り分け先ごとの同時実行の枠（3.10）
  - 受け入れ基準: 同じスペシャルハーネスを使う2つの Project の Execution が同時に動ける; 同じ Project の2つ目は claim されない; スペシャルハーネスへ直接振り分けたタスクがスペシャルハーネスごとに1本に制限される
- [ ] [Backend] 記録の保存と受け渡し（3.8）
  - 受け入れ基準: 出す段まで成功した Execution の `records.keep` のファイルだけが記録になる; 作業の段の出力が記録にならない; 次の仕事の作業の段の `records/` に新しい順に `records.pass.recent` 件が読み取り専用で置かれる; 消した記録が渡らない; 渡した記録が context_revisions に残る
- [ ] [Backend] 契約テスト用の最小のスペシャルハーネス（Q173）
  - 受け入れ基準: 4つの段と承認の宣言を持つ最小のスペシャルハーネスで、prepare から publish まで、承認待ち、質問と再開、出す直前の問い合わせの拒否（`content_changed`）が通しで確かめられる
- [ ] [Frontend] スペシャルハーネスの記録の一覧と削除（3.8）
  - 受け入れ基準: スペシャルハーネスの画面で記録を新しい順に読める; 消した記録が一覧で消したと分かり、次の仕事に渡らない

## 10. 要確認

### 10.1 前の版の要確認の扱い

| # | 前の内容 | 扱い |
|---|---|---|
| Q102 | スペシャルハーネスのリポジトリの共通の形 | 解決。契約 4.1 で決めた |
| Q103 | 作業の段からの外への接続（調査で Web を読む） | 解決。作業の段の外への接続はモデルの API だけ（本人決定 7）。Web の調べものは 00 の Q34 の案（モデル側の検索機能）による |
| Q104 | 確かめる段で不合格のとき、自動で直させるか | 解決。自動の再実行はしない。本人が管理画面で再実行を選ぶ（契約 10.3） |
| Q105 | 記録の選び方 | 解決。新しい順に `records.pass.recent` 件（契約 9章）。内容の近さで選ぶのはスコープ外 |
| Q106 | 管理画面で記録を一覧・削除できるか | 解決。一覧と削除を作る（01 の 5.1、契約 9章） |
| Q107 | スペシャルハーネスへの振り分けのラベル | 廃止。振り分けのラベルは `route:` の1種類（Q120） |
| Q108 | PR を作らない仕事の Linear の状態 | 廃止。状態の変更の契機を PR から段の結果に直したため、16 で扱う |
| Q109 | 記録を Profile の版をまたいで渡すか | 解決。渡す（契約 9章） |

### 10.2 新しい要確認

| # | 内容 | 案 |
|---|---|---|
| Q170 | Evaloom の古い開発の部品（worktree、Gate、Git Publisher のコードと projects の開発の列）をいつ消すか | 決定（2026-11-10 本人、案を変更）: software-engineer への切り替えを待たずに消してよい。運用で動いているものがまだ無いため。消す作業は、仕様が confirmed になった後の最初の実装タスクとして行う。消す前の版は git 履歴で読める
| Q171 | 同じスペシャルハーネスを使う複数の Project の記録を分けるか | 決定（2026-11-10 本人、案を採用）: 分けない。記録はスペシャルハーネス単位で1つにまとめる（契約 9章）。Project ごとに分けたくなったら、スペシャルハーネスが `params` を見て `out` のファイル名を分ける |
| Q172 | 受け入れ条件の要否を Project の設定で上書きできるか | 決定（2026-11-10 本人、案を採用）: できない。Profile の宣言だけで決める（Project の6項目を増やさないため、本人決定 6） |
| Q173 | 契約に合っているかを確かめる試験用のスペシャルハーネスをどこに置くか | 決定（2026-11-10 本人、案を採用）: evaloom リポジトリの試験用データ（例 `testdata/special-harness-fixture/`）に、AI を呼ばない最小のものを置く。作業の段はミクロハーネスの代わりに決まった出力を返す試験用の Adapter で動かす。requirements 20章の条件12・15・18 の代わりの受け入れ条件にする |
| Q174 | `records.keep` に宣言したファイルが `out` に無いとき | 決定（2026-11-10 本人、案を採用）: その記録だけを残さず、Execution は成功のままにする。記録しなかった理由を実行ログ（execution_events の `log`）に残す |
