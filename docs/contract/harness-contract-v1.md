---
status: draft
written: 2026-11-10
contractVersion: "1"
範囲: Evaloom とスペシャルハーネスの約束（ハーネス契約）の第1版。スペシャルハーネスのリポジトリの形、Profile 定義、段ごとの環境、段の入力と結果、承認、質問と再開、記録、中止とタイムアウト、エラー分類、双方の義務
---

# ハーネス契約 第1版

**正本**: この文書（evaloom リポジトリの `docs/contract/harness-contract-v1.md`） ｜ **根拠**: [`../vision.md`](../vision.md) 3章・4章・6章、本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）、requirements 1.3 原則5・13・14・15

この文書は、Evaloom とスペシャルハーネスの約束を定める。スペシャルハーネスを作る人は、Evaloom の仕様全体ではなく、この文書の決まった版だけを読めば作れるようにする（vision.md 6章、requirements 原則5）。Evaloom 側の仕様（`docs/specs/`）は、この文書に従う。食い違ったときは、この文書を先に直してから仕様を直す。

## 0. 用語

| 呼び名 | 指すもの |
|---|---|
| Evaloom | 仕事を受け取り、振り分け、順番を待たせ、本人の判断を取り、結果を返すもの。層の関係ではマクロハーネスと呼ぶ |
| スペシャルハーネス | software-engineer など、仕事の種類ごとのやり方一式。指示、スキル、準備・確かめる・出すのプログラムを持つ |
| ミクロハーネス | Claude Code、Codex。指示を受けて実際に考えて手を動かす AI の道具 |
| Profile | スペシャルハーネスのある版の宣言（`evaloom/profile.yaml`）と、その版の commit の中身 |
| 段 | 1回の実行（Execution）の区切り。`prepare`（準備、任意）→ `work`（作業）→ `check`（確かめる）→ `publish`（出す）の4つ |
| 振り分け先 | Evaloom が仕事を頼む相手。Project（スペシャルハーネスと渡す値の組）か、スペシャルハーネスそのもの |
| 渡す値（params） | Project がスペシャルハーネスに渡す JSON。中身は Evaloom が解釈しない |
| 中身の識別子（contentId） | スペシャルハーネスが決める、確かめた中身・出す中身を表す文字列。開発なら commit SHA など |
| 承認 | 本人が管理画面で、特定の操作と中身の識別子に対して与える許可 |
| 作業領域（workspace） | タスクごとに Evaloom が用意するディレクトリ。準備と作業の段が書き、確かめると出すの段は読むだけ |

この文書では、単に「ハーネス」とは書かない。

## 1. 目的

- スペシャルハーネスを、Evaloom の内部を知らずに作れるようにする。
- 仕事の中身（準備・作業・確かめる・出す）はスペシャルハーネスが決め、その中身を動かす安全な場所（隔離、鍵の保管、固定した版からの読み出し）は Evaloom が用意する、という分担を固定する（vision.md 3章・4章）。
- 作業中の AI が合否の基準と鍵に触れられない形を、契約として守らせる（requirements 原則13・14・15、vision.md 5章）。

## 2. 全体の流れ

```
[Evaloom] 受け取る → 振り分ける → 順番を待たせる → Profile の版を固定する
   │
   ▼  ここから段。Evaloom は段ごとに別の隔離を用意する
[prepare]  任意。スペシャルハーネスの固定版のプログラム。AI を呼ばない。読み取り用の鍵と宣言した宛先だけ
   ▼
[work]     ミクロハーネス（AI）。鍵なし。外への接続はモデルの API だけ
   ▼
[check]    スペシャルハーネスの固定版のプログラム。鍵なし。外への接続なし。中身の識別子を報告する
   ▼       承認が要る操作があれば、ここで承認を求める（7章）
[Evaloom]  本人が管理画面で承認する（承認を省く設定の操作は待たない）
   ▼
[publish]  スペシャルハーネスの固定版のプログラム。AI を呼ばない。書き込み用の鍵と宣言した宛先だけ
   │       外へ出す操作の直前に、毎回 Evaloom に許可を問い合わせる（7.4）
   ▼
[Evaloom] 結果を返す（Linear、通知、管理画面、記録）
```

## 3. 契約の版と互換の規則

- 契約の版は整数の文字列で表す（第1版は `"1"`）。Profile 定義の `contractVersion` に、従う版を書く。
- 同じ版の中で Evaloom が足してよいのは、省略してよい項目（Profile 定義の任意項目、要求 JSON の項目、段の結果の任意項目）だけとする。既存の項目の意味、必須の項目、列挙値の意味を変えるときは版を上げる。
- 要求 JSON（6.1）に知らない項目があっても、スペシャルハーネスは無視して動かなければならない。
- Evaloom は、Profile 定義と段の結果に知らない項目があれば受け付けない（Fail Closed。登録の拒否、または `contract_error`）。スペシャルハーネスは、宣言した版に無い項目を書かない。
- Evaloom は、対応する契約の版の一覧を持つ。一覧に無い版の Profile は登録を拒否する。
- この文書を同じ版の中で書き足したときは、末尾の改訂の記録に日付と中身を残す。

## 4. スペシャルハーネスのリポジトリ

### 4.1 形

スペシャルハーネスは、独立した Git リポジトリにする（requirements 原則2）。Evaloom が読むのは `evaloom/profile.yaml` と、そこから参照されたファイルだけである。

```
<special-harness>/
  evaloom/profile.yaml   Evaloom が読む宣言（必須、5章）
  evaloom/prepare/       準備の段のプログラム（任意。置き場所は自由で、profile.yaml から参照する）
  evaloom/work/          作業の段でミクロハーネスに渡す指示・スキル・設定
  evaloom/check/         確かめる段のプログラムと、合否の基準
  evaloom/publish/       出す段のプログラム
  そのほか               スペシャルハーネスが自由に使ってよい
```

- `evaloom/profile.yaml` から参照するパスは、すべて同じ commit の中の相対パスにする。リポジトリの外、シンボリックリンク、サブモジュールは参照できない。
- 確かめる段の基準とプログラムは、作業の段に渡すファイル（`stages.work` で宣言したもの）と重ならない場所に置く。Evaloom は、作業の段には `stages.work` で宣言したファイルだけを置く（6.3）。

### 4.2 版とタグ

- 版は SemVer で表し、Git のタグ `profile/v<SemVer>`（例 `profile/v1.2.0`）を付ける。
- `evaloom/profile.yaml` の `profileVersion` は、タグの版と一致させる。一致しなければ Evaloom は登録を拒否する。
- 公開したタグは付け替えない。Evaloom は登録時にタグを commit SHA に解決して記録し、以後はタグを読み直さない。同じ版を別の commit で登録しようとしたら拒否し、登録済みのタグが別の commit を指していることを見つけたら、その版での実行を止める（`config_error`）。
- Evaloom が実行に使うのは、登録時に記録した commit の中身だけである。スペシャルハーネスのリポジトリの作業ツリーは使わない。

## 5. Profile 定義（`evaloom/profile.yaml`）

### 5.1 形

```yaml
contractVersion: "1"
profileVersion: "1.2.0"           # タグの版と一致させる
name: "software-engineer"          # 表示名
description: |                     # 何の仕事を受けるか。本人と、振り分けを提案する AI が読む
  Git リポジトリのコードを変更し、テストで確かめ、PR を出す。
acceptanceCriteria: required       # required | optional。受け入れ条件の要否
params:                            # Project から受け取る値（任意）
  schema: "evaloom/params.schema.json"   # JSON Schema（draft 2020-12）。無ければ params は {} だけを受け付ける
stages:
  prepare:                         # 任意。無ければ準備の段を行わない
    command: ["evaloom/prepare/run"]
    timeoutSec: 600
    network:
      allow: ["github.com:443"]    # host:port の完全一致。ワイルドカード、IP、ローカルの名前は書けない
    secrets:
      - { slot: "github_read", access: "read" }
    runtime: { kind: "container", image: "<ダイジェスト付きのイメージ>" }   # 任意。無ければ Evaloom の既定の環境
  work:                            # 必須
    microHarness: "claude_code"    # claude_code | codex
    model: "<正式な Model ID>"
    instructions: ["evaloom/work/AGENTS.md"]
    skills: ["evaloom/work/skills/"]
    settings: ["evaloom/work/settings.json"]
    toolPermissions: { allow: ["Read", "Edit", "Bash(git commit:*)"] }
    resume: optional               # required | optional。質問の後に同じセッションを再開できないときの扱い（8章）
    timeoutSec: 3600
  check:                           # 必須
    command: ["evaloom/check/run"]
    timeoutSec: 900
  publish:                         # 必須。外へ何も出さない仕事でも、結果を報告する最小のプログラムを置く
    command: ["evaloom/publish/run"]
    timeoutSec: 600
    network:
      allow: ["api.github.com:443", "github.com:443"]
    secrets:
      - { slot: "github_write", access: "write" }
approvals:                         # 本人の承認が要る操作（任意）
  - operation: "merge_pull_request"
    description: "PR をベースブランチへマージする"
    waivable: true                 # Project の設定で承認を省いてよいか
records:                           # 記録として残すもの（任意、9章）
  keep: ["check/report.md"]        # <段>/<その段の out からの相対パス>
  pass: { recent: 10 }             # 次の仕事に渡す件数（0〜50）
```

### 5.2 項目の規則

| 項目 | 必須 | 規則 |
|---|---|---|
| `contractVersion` | 必須 | Evaloom が対応する版（3章） |
| `profileVersion` | 必須 | SemVer。タグと一致 |
| `name` / `description` | 必須 | `description` は 2000 文字以内。Issue の振り分けの提案に使う（Evaloom の仕様 15） |
| `acceptanceCriteria` | 必須 | `required` なら、受け入れ条件の無いタスクを Evaloom は実行しない |
| `params.schema` | 任意 | Evaloom は Project の渡す値をこのスキーマで形だけ確かめ、中身は解釈しない（Evaloom の要確認 Q115） |
| `stages.<段>.command` | prepare・check・publish で必須 | 実行ファイルと引数の配列。先頭は commit の中の相対パス。シェルを通さずに実行する |
| `stages.<段>.timeoutSec` | 必須 | 1 以上、Evaloom の全体設定の上限以下 |
| `stages.<段>.network.allow` | 任意 | prepare と publish だけに書ける。work はモデルの API だけ、check は接続なしで、書けない。Evaloom が知っているモデルの API の宛先を書いた Profile は登録で拒否される（Evaloom の要確認 Q146）。Evaloom が知らないモデルの宛先は見分けられないため、AI を呼ばないことは 12章の5の義務で守る |
| `stages.<段>.secrets` | 任意 | prepare は `access: read` だけ、publish は `read` と `write` を書ける。work と check には書けない。`slot` は `^[a-z][a-z0-9_]{0,31}$`（英小文字で始まり、英小文字・数字・`_` で 32 文字以内）で、同じ段の中で一意（Evaloom の要確認 Q138） |
| `stages.<段>.runtime` | 任意 | prepare・check・publish だけに書ける。段を動かす環境で、`{ "kind": "container", "image": "<ダイジェスト付きのイメージ>" }` または `{ "kind": "macos_vm", "image": "<Tart のイメージ>" }`。省略したら Evaloom の既定のコンテナのイメージで動く。work には書けない（Evaloom の既定の環境だけ）。`macos_vm` を受け付けるかは Evaloom の版による（Evaloom の要確認 Q147） |
| `stages.work.microHarness` | 必須 | Evaloom に登録されたミクロハーネスの名前 |
| `stages.work.instructions` / `skills` / `settings` | 任意 | 作業の段に置くファイルまたはディレクトリ。ここに無いファイルは作業の段から見えない |
| `stages.work.toolPermissions` | 任意 | ミクロハーネスに許す道具。形はミクロハーネスごとに Evaloom の Adapter が決める。Adapter が変換できない値は登録で拒否される（Evaloom の要確認 Q145） |
| `stages.work.resume` | 任意 | `required` / `optional`（既定 `optional`）。質問の後に同じセッションを再開できないときの扱い（8章の4、Q124） |
| `approvals[].operation` | 任意 | 英小文字・数字・`_` で 64 文字以内。Profile の中で一意 |
| `approvals[].waivable` | 任意 | 既定 `false`。`false` の操作は、Project の設定で省けない |
| `records.keep` | 任意 | `<段>/<相対パス>`。段は prepare・check・publish のどれか（作業の段の出力は記録にしない。9章） |

## 6. 段

### 6.1 段の入力（要求 JSON）

Evaloom は段ごとに要求 JSON を読み取り専用のファイルとして置き、パスを環境変数 `EVALOOM_REQUEST` で渡す。作業の段では、Evaloom がこの中身をミクロハーネスへの指示に整形して渡す（6.3）。

```json
{
  "contractVersion": "1",
  "stage": "publish",
  "execution": {
    "id": "01J...",
    "seq": 2,
    "mode": "continue",
    "resumeStage": "publish"
  },
  "task": {
    "id": "task-001",
    "title": "...",
    "body": "...",
    "acceptanceCriteria": "...",
    "source": { "kind": "linear", "identifier": "ENG-123", "url": "https://linear.app/..." }
  },
  "route": { "kind": "project", "projectId": "customer-system", "projectName": "顧客システム" },
  "params": { "repository": "owner/repo", "baseBranch": "main" },
  "profile": { "harnessId": "software-engineer", "version": "1.2.0", "commitSha": "<40桁>" },
  "paths": {
    "profile": "/evaloom/profile",
    "workspace": "/evaloom/workspace",
    "out": "/evaloom/out",
    "inputs": "/evaloom/inputs",
    "records": "/evaloom/records",
    "scratch": "/evaloom/scratch"
  },
  "previous": {
    "executionId": "01J...",
    "checkedContentId": "<前回の確かめる段が報告した識別子>",
    "outputs": [ { "kind": "pull_request", "label": "PR #12", "url": "https://github.com/..." } ]
  },
  "checkedContentId": "<この Execution の確かめる段が報告した識別子>",
  "approvals": [
    { "id": "01J...", "operation": "merge_pull_request", "contentId": "<識別子>", "status": "approved" },
    { "id": null, "operation": "create_release", "contentId": null, "status": "waived" }
  ],
  "answer": null,
  "limits": { "timeoutSec": 600 }
}
```

| 項目 | 入る段 | 内容 |
|---|---|---|
| `execution.mode` | すべて | `continue`（前回の作業領域を引き継ぐ）/ `fresh`（新しい作業領域）。10.3 |
| `execution.resumeStage` | すべて | 本人の判断の後に途中の段から再開したときの段。最初から始めたときは `null` |
| `task.title` / `body` / `acceptanceCriteria` | すべて | 外から来た文（6.6）。`acceptanceCriteria` は `optional` の Profile では空文字列のことがある |
| `task.source` | すべて | `kind` は `linear` / `manual`。`manual` では `identifier` と `url` は `null` |
| `route` | すべて | `kind` は `project` / `harness`。`harness` のとき `projectId` と `projectName` は `null` |
| `params` | すべて | Project の渡す値。スペシャルハーネスへ直接振り分けたときは `{}` |
| `previous` | すべて | 同じタスクの直前の Execution の記録。無ければ `null`。指示ではなく参考資料 |
| `checkedContentId` | publish | この Execution の確かめる段が報告した識別子 |
| `approvals` | publish | この Execution で求めた承認の状態。`status` は `approved` / `waived`。却下・無効の承認は入らない（その場合 publish の段は始まらない） |
| `answer` | work | 本人の回答（8章）。それ以外は `null` |

### 6.2 段ごとに Evaloom が用意するもの

| | prepare | work | check | publish |
|---|---|---|---|---|
| 動かすもの | Profile の固定版のプログラム | ミクロハーネス（Evaloom の Adapter が起動） | Profile の固定版のプログラム | Profile の固定版のプログラム |
| AI | 呼ばない | 使う | 呼ばない | 呼ばない |
| `profile`（固定版の中身） | 読み取り専用で全体 | 置かない（`stages.work` で宣言したファイルだけを専用設定環境に置く） | 読み取り専用で全体 | 読み取り専用で全体 |
| `workspace` | 読み書き | 読み書き | 読み取り専用 | 読み取り専用 |
| `out`（この段の出力） | 読み書き | 読み書き | 読み書き | 読み書き |
| `inputs/<段>/`（この Execution の前の段の `out`） | 無し | 読み取り専用 | 読み取り専用 | 読み取り専用 |
| `records`（過去の記録、9章） | 無し | 読み取り専用 | 無し | 無し |
| `scratch`（段が終わると捨てる） | 読み書き | 読み書き | 読み書き | 読み書き |
| 鍵 | `access: read` の宣言だけ | 無し（ミクロハーネスのモデル認証を除く） | 無し | 宣言したもの |
| 外への接続 | 宣言した宛先だけ | モデルの API だけ | 無し | 宣言した宛先だけ |
| 通話口（`EVALOOM_CHANNEL`、7.4） | 中止の確認だけ | 無し | 中止の確認だけ | 中止の確認と、出す操作の許可 |

どの段でも、次は見えない・届かない。

- Evaloom の Control API（ホストのループバックを含む）、管理 DB、Evaloom のデータディレクトリ
- 本人の HOME、本人の設定・メモリ、キーチェーン、SSH エージェント
- ほかのタスクの作業領域、宣言していない鍵
- Docker ソケットなど、隔離の外を操作できるインターフェース

`out` の中身は、段が終わると Evaloom が成果物として保存し、後の段の `inputs/<段>/` に読み取り専用で置く。後の段に渡したい情報は `out` に書く。`workspace` は確かめる段と出す段では読むだけなので、確かめた後に作業領域が変わることは無い。ただし、確かめたものと出すものが同じであることを保証するのはスペシャルハーネスである（12章）。

### 6.3 作業の段

- Evaloom は、`stages.work` で宣言した指示・スキル・設定を固定版から読み、作業の段ごとに作る専用設定環境に置く。本人のグローバル設定と個人のメモリは読ませない。
- Evaloom は、要求 JSON の中身を指示文に整形してミクロハーネスに渡す。Issue の本文などの外から来た文は、タスクの内容として区切って渡し、指示としては扱わない（6.6）。
- ミクロハーネスの作業ディレクトリは `workspace` とする。
- 作業の段の出力として、Evaloom は次のファイルだけを読む。どちらも任意である。
  - `out/summary.md`: 作業の概要。管理画面と Linear のコメントに載せる（機密情報の除外を通す）。
  - `out/question.json`: 判断できないことの質問（8章）。
- 作業の段の結果（成功・失敗）は、Evaloom がミクロハーネスの終了コードとセッションから決める。作業の段が書いたファイルは、合否には使わない。

### 6.4 プログラムの段（prepare・check・publish）

- `command` を、固定版を展開した `profile` を作業ディレクトリにして実行する。シェルは通さない。
- 渡す環境変数は次だけとする。`EVALOOM_REQUEST`、`EVALOOM_RESULT`（結果を書くファイルのパス）、`EVALOOM_CHANNEL`（通話口、6.2 の表のとおり）、`EVALOOM_SECRET_<SLOT>`（宣言した鍵、11章）、`HOME`（`scratch` の中の空のディレクトリ）、`PATH`、`LANG`、`TZ=UTC`。
- 段は終了する前に、`EVALOOM_RESULT` に結果の JSON（6.5）を書く。結果のファイルが無い、または形が合わないときは、終了コードにかかわらず `contract_error` とする。
- 標準出力と標準エラーは、Evaloom がログとして保存する（機密情報の除外を通す）。結果をログで伝えない。

### 6.5 段の結果（結果 JSON）

```json
{
  "contractVersion": "1",
  "stage": "check",
  "status": "succeeded",
  "errorClass": null,
  "message": "テスト 120 件を実行し、すべて通った",
  "contentId": "<40桁の commit SHA など>",
  "summary": "確かめた内容の短い説明",
  "findings": [],
  "approvalRequests": [
    {
      "operation": "merge_pull_request",
      "contentId": "<40桁>",
      "target": { "label": "owner/repo PR #12", "url": "https://github.com/..." },
      "summary": "PR #12 を main へマージする"
    }
  ],
  "outputs": [],
  "reportedContext": [],
  "usage": null
}
```

| 項目 | 書ける段 | 内容 |
|---|---|---|
| `status` | すべて | 下の表 |
| `errorClass` | すべて | `status` が `failed` / `unknown` のとき必須。13章のうち、スペシャルハーネスが書ける値 |
| `message` | すべて | 1000 文字以内。秘密情報を書かない |
| `contentId` | check（`succeeded` / `needs_approval` / `blocked` のとき必須）、publish | 中身の識別子。1〜256 文字の印字可能な ASCII。Evaloom は完全一致で比べるだけで、意味を解釈しない（Evaloom の要確認 Q123） |
| `summary` | check、publish | 管理画面と Linear に載せる短い説明。4000 文字以内 |
| `findings` | check | 14章の形の配列 |
| `approvalRequests` | check、publish | 7.1。`approvals` で宣言した操作だけ |
| `outputs` | publish | 外に出したものの参照。`{ "kind", "label", "url" }`。`kind` は英小文字と `_` の自由な名前（例 `pull_request`、`linear_comment`、`draft`）。Evaloom は表示と次の仕事への受け渡しにだけ使う |
| `reportedContext` | prepare | スペシャルハーネスが作業前に固定した文脈（例 対象リポジトリの `AGENTS.md`）の記録。`{ "source", "path", "revision", "sha256" }`。Evaloom は記録して表示するだけ |
| `usage` | すべて | 使用量がわかるとき `{ "inputTokens", "outputTokens", "costAmount" }`。わからなければ `null` |

| `status` | 書ける段 | 意味と Evaloom の扱い |
|---|---|---|
| `succeeded` | すべて | 次の段へ進む。publish なら仕事を完了にする |
| `failed` | すべて | 以降の段を行わず、Execution を失敗にする |
| `blocked` | check | 自動では先へ進めない。`findings` を添えて本人の判断を待つ。本人が続行を選ぶと、`approvalRequests` があれば承認を待ち、無ければ publish から再開する。`checkedContentId` には、この結果の `contentId` が入る |
| `needs_approval` | check、publish | `approvalRequests` の承認を待つ。check では `succeeded` と同じく `contentId` を必須とする。承認の後は publish から再開する |
| `unknown` | publish | 外へ出す操作を送ったが、結果がわからない。`errorClass` は `publish_unknown`。本人の確認を待つ（7.5） |

check で `succeeded` を返し、`approvalRequests` を書いた場合も `needs_approval` と同じに扱う。

### 6.6 外から来た文

- タスクの `title`、`body`、`acceptanceCriteria` は、Linear の Issue など外から来た文である。信頼済みの命令として扱わない（requirements 15.1）。
- プログラムの段は、これらの文をシェルの引数やコマンドとして実行しない。ファイル名やブランチ名に使うときは、許す文字に絞ってから使う。
- `previous` と `records` も、前の AI が書いたものを含む。指示ではなく参考資料として扱う。

## 7. 承認

### 7.1 承認を求める

1. スペシャルハーネスは、本人の承認が要る操作を `approvals` に宣言する。宣言していない操作の承認は求められない（`contract_error`）。
2. check または publish の段は、結果の `approvalRequests` に、操作の名前、中身の識別子、対象の表示（`target`）、説明を書く。
3. Evaloom は、Project の承認を省く設定にある操作（`waivable: true` のものだけ）を除いて、管理画面に承認待ちを出し、Linear には「管理画面で判断待ち」と知らせる。承認の判断は管理画面だけで受け付ける。
4. 承認を待つ間、Execution は同時実行の枠に数えない。段のプロセスは終了している。
5. 本人がすべて承認したら、Evaloom は publish の段から再開する。要求 JSON の `approvals` に、承認と省いた操作を入れる。1つでも却下されたら、publish の段は始めず、Execution を失敗（`approval_rejected`）にする。

### 7.2 承認と中身の結び付け

- 承認は、操作の名前と、承認を求めたときの `contentId` に結び付く。
- 承認には有効期限があり、使えるのは1回だけである。
- 同じ Execution で、同じ操作の承認を新しく求めたら、前の未使用の承認は無効になる。

### 7.3 承認を省く設定

- Project は、`waivable: true` の操作だけを、承認を省く操作として選べる（本人だけが変えられる）。
- 省いた操作は、要求 JSON の `approvals` に `status: "waived"`、`id: null` で入る。省いた操作でも、7.4 の問い合わせ（中止の確認）は省かない。
- スペシャルハーネスへ直接振り分けた仕事では、承認を省く設定は無い（Evaloom の要確認 Q113）。

### 7.4 出す直前の問い合わせ（通話口）

publish の段は、外へ出す操作（push、PR の作成、マージ、コメントの投稿など、外の状態を変えるもの）を1つ行うたびに、その直前に通話口へ許可を問い合わせる。

- 通話口は、`EVALOOM_CHANNEL` が指す Unix ドメインソケットである。1行1件の JSON で要求を送り、1行の JSON の応答を受け取る（方式は Evaloom の要確認 Q114）。
- 要求の形

```json
{ "op": "authorize", "operation": "merge_pull_request", "approvalId": "01J...", "contentId": "<40桁>" }
```

| 項目 | 内容 |
|---|---|
| `operation` | 行う操作の名前。`approvals` で宣言した操作なら、その名前。宣言していない操作（承認の要らない操作）は `publish` と書く |
| `approvalId` | 承認が要る操作では、要求 JSON の `approvals` の `id`。省いた操作と承認の要らない操作では `null` |
| `contentId` | これから外へ出す中身の識別子。スペシャルハーネスが、出す直前に実物から求めた値を書く |

- 応答の形

```json
{ "allowed": true }
{ "allowed": false, "reason": "content_changed" }
```

| `reason` | 意味 |
|---|---|
| `cancelled` | 中止の要求がある |
| `content_changed` | 承認したときの `contentId` と違う。承認は無効になる |
| `not_approved` | 承認が無い、却下された、無効になった |
| `already_used` | 承認はすでに使われた |
| `expired` | 承認の有効期限が切れた |
| `unavailable` | Evaloom に問い合わせられなかった |

- Evaloom は、承認の照合と消費を1つの書き込みの中で行う（Fail Closed）。`allowed: true` が返った承認は使用済みになる。
- スペシャルハーネスは、`allowed: true` を受け取ったときだけ、その操作を1回行う。応答が無い、形が合わない、`allowed: false` のときは操作しない。
- 操作をしなかったときは、`status: "failed"`、`errorClass: "publish_failed"` を返す。`reason` は `message` に書く。

prepare と check の段の通話口では `{ "op": "cancel_check" }` だけを使える。応答は `{ "cancelRequested": true | false }` で、`true` なら段を終えて `status: "failed"`、`errorClass: "cancelled"` を返してよい。

### 7.5 結果がわからないとき

外へ出す操作を送った後に、応答が無いなどで結果がわからなくなったら、次のとおり扱う。

1. 同じ承認で、同じ操作をもう一度送らない。
2. 外の状態を読み直せるなら読み直す。操作が行われていたら `succeeded`（行われた結果を `outputs` に書く）、行われていなければ `failed`（`publish_failed`）を返す。
3. 読み直せないときは `status: "unknown"`、`errorClass: "publish_unknown"` を返し、わかっていることを `message` に書く。Evaloom は本人の確認を待つ。本人が続行を選ぶと publish の段を新しく始めるので、publish の段は始めに外の状態を確かめ、すでに行った操作を繰り返さないように作る。

## 8. 作業中の AI の質問と再開

1. 作業の段の AI が、判断できないことがあって先へ進めないときは、`out/question.json` に質問を書いて終わる。

```json
{ "question": "既存の API を残すか消すか決められない。残すと…、消すと…", "options": ["残す", "消す"] }
```

   `question` は 4000 文字以内、`options` は任意で 10 個以内。どう書かせるかは、スペシャルハーネスの指示（`stages.work.instructions`）に書く。

2. Evaloom は、作業の段が終わった後に `out/question.json` があれば、確かめる段へ進まず、本人の判断待ちにする。質問は管理画面に出し、Linear には「管理画面で判断待ち」と知らせる。
3. 本人が管理画面で答えたら、Evaloom は同じタスクの作業領域で作業の段を再開する。ミクロハーネスが同じセッションの再開に対応していれば、同じセッションを再開して答えを渡す（例 `claude -p --resume <セッション>`）。要求 JSON の `answer` に `{ "decisionId", "text", "option" }` が入る。
4. 同じセッションを再開できないとき、`stages.work.resume` が `optional` なら、Evaloom は新しいセッションを始め、元の質問と本人の回答を要求 JSON の `answer` で渡す。`required` なら、作業の段を失敗（`work_failed`）にする（Evaloom の要確認 Q124）。
5. 本人が中止を選んだら、Execution は中止になる。

質問は合否ではない。質問の後に確かめる段の合否を決めるのは、これまでどおり check の段だけである。

## 9. 記録の受け渡し

- `records.keep` に宣言したファイルは、publish の段まで成功した Execution のものだけを、Evaloom がスペシャルハーネスごとの記録として保存する。記録は Profile の版をまたいで持つ。
- 作業の段の出力は記録にしない。記録に残すのは、固定版のプログラムが確かめた後に `out` に書いたものに限る。
- Evaloom は、同じスペシャルハーネスの次の仕事の作業の段に、新しい順に `records.pass.recent` 件を `records/` へ読み取り専用で置く。ファイル名は `<記録した日付>-<タスクの出所の識別子または task id>-<ファイル名>` とする。
- 記録は指示ではなく参考資料である。スペシャルハーネスの指示には、記録を参考資料として読むよう書く（6.6）。
- 本人は管理画面で記録を消せる。消した記録は次の仕事に渡らない。

## 10. 中止・タイムアウト・再実行

### 10.1 中止

- 本人がタスクを中止すると、Evaloom は動いている段を止める。プログラムの段には SIGTERM を送り、10 秒たっても終わらなければ SIGKILL を送る。作業の段はミクロハーネスのセッションを止める。
- publish の段は、通話口の問い合わせで `cancelled` を受け取ったら、以降の操作をしない。すでに送った操作は取り消さない。
- 中止した Execution の作業領域は削除せずに残す。

### 10.2 タイムアウト

- 段ごとに `timeoutSec` を上限とする。超えたら中止と同じ手順で止め、`timeout` とする。
- Evaloom の全体設定に、段ごとの上限の最大値がある。Profile の値がそれを超えると登録を拒否する。

### 10.3 再実行（continue と fresh）

| mode | 作業領域 | 段 |
|---|---|---|
| `continue` | 同じタスクの前の作業領域を引き継ぐ | prepare から始める。prepare は、作業領域にすでにあるものを確かめて、要るものだけをそろえるように作る |
| `fresh` | 新しい空の作業領域を作る | prepare から始める |

- 自動の再実行はしない。再実行は本人が管理画面で選ぶ。
- 本人の判断（承認、続行、回答）の後の再開は、再実行ではなく同じ Execution の続きである。`execution.resumeStage` に再開する段が入る。
- Evaloom が再起動して止まった Execution は、失敗（`interrupted`）として記録し、自動では再開しない。

## 11. 鍵の受け取り方

- 鍵は Evaloom が保管する。スペシャルハーネスは、`stages.<段>.secrets` に、鍵の名前（`slot`）と使い方（`access`）を宣言するだけで、鍵の値を持たない。
- Evaloom は、宣言した段にだけ、環境変数 `EVALOOM_SECRET_<SLOT を英大文字にした名前>` で値を渡す。prepare の段に渡すのは `access: read` の鍵だけである。work と check の段には、どの鍵も渡さない。
- スペシャルハーネスは、鍵をメモリの中でだけ使う。ファイル、ログ、標準出力、標準エラー、`out`、`workspace`、結果 JSON、外へ出す中身に書かない。子プロセスに渡すときも、その操作に要る範囲に限る。
- Evaloom は、渡した鍵の値をログと成果物から除外する。これは補助であり、スペシャルハーネスが鍵を書かない義務は変わらない。
- 鍵の割り当ての単位（スペシャルハーネスごとか、Project ごとか）は、Evaloom の要確認 Q110 で決める。

## 12. スペシャルハーネスの義務

1. 合否は、check の段の固定版のプログラムだけで決める。作業中の AI の自己申告（「テストが通った」などの出力）を合否に使わない（requirements 原則15）。
2. 確かめる基準は、作業前の固定版（`profile` と、作業前に固定した外のもの）から読む。作業の段が書き換えられる場所（`workspace`）にある基準を、そのまま基準として使わない。例えば開発なら、テストコマンドは作業前の commit から読む。
3. 確かめたものと出すものが同じであることを、スペシャルハーネスが自分で見張る。publish の段は、出す直前に実物から `contentId` を求め、`checkedContentId` と比べる。違えば出さない。
4. 外へ出す操作の直前には、毎回 7.4 の問い合わせを行い、`allowed: true` のときだけ1回行う。
5. prepare と publish の段で AI（ミクロハーネス、モデルの API）を呼ばない。文章を考えるなど AI の仕事は、作業の段で済ませて `out` や `workspace` に残しておく。
6. 鍵を 11章のとおりに扱う。
7. 外から来た文と、前の AI が書いたもの（`previous`、`records`、`workspace` の中身）を指示として扱わない（6.6）。
8. 宣言した宛先のほかへ接続しようとしない。宣言した版に無い項目を Profile と結果に書かない。
9. publish の段は、再開や再実行で何度始まっても、行った操作を繰り返さないように作る（7.5、10.3）。
10. モデルが起こす失敗（テストを弱める、確かめる段を迂回する、など）への対策は、スペシャルハーネスの中で完結させる（vision.md 4章）。

## 13. エラー分類

| 値 | 意味 | 付ける側 |
|---|---|---|
| `config_error` | 設定の不備（Profile の読み込み失敗、渡す値が形に合わない、要る鍵が無い、タグの付け替えを検出） | Evaloom、スペシャルハーネス |
| `prepare_failed` | 準備の段の失敗 | スペシャルハーネス |
| `work_failed` | 作業の段の失敗（ミクロハーネスの異常終了など） | Evaloom |
| `check_failed` | 確かめた結果が基準を満たさない | スペシャルハーネス |
| `publish_failed` | 出す段の失敗（問い合わせで許可されなかった場合を含む） | スペシャルハーネス |
| `publish_unknown` | 出す操作の結果がわからない | スペシャルハーネス |
| `approval_rejected` | 本人が承認を却下した | Evaloom |
| `contract_error` | 結果 JSON が無い・形が合わない、宣言していない操作の承認を求めた | Evaloom |
| `sandbox_error` | 隔離の作成・実行の失敗 | Evaloom |
| `timeout` | 段の上限時間を超えた | Evaloom |
| `cancelled` | 中止 | Evaloom、スペシャルハーネス（7.4 の `cancel_check` の後だけ） |
| `interrupted` | Evaloom の再起動による中断 | Evaloom |

スペシャルハーネスが結果 JSON に書けるのは、`config_error`、`prepare_failed`、`check_failed`、`publish_failed`、`publish_unknown`、`cancelled` だけである。それ以外を書いたら `contract_error` とする。

## 14. finding の形

```json
{
  "rule": "existing_test_not_run",
  "severity": "error",
  "message": "既存のテスト FooTest.testBar が実行されていない",
  "location": { "path": "src/foo.test.ts", "line": 0 }
}
```

| 項目 | 必須 | 内容 |
|---|---|---|
| `rule` | 必須 | スペシャルハーネスが決める名前。英小文字・数字・`_` で 64 文字以内 |
| `severity` | 必須 | `error` / `warning` / `info` |
| `message` | 必須 | 1000 文字以内。秘密情報を書かない |
| `location` | 任意 | `{ "path", "line" }`。`path` は作業領域からの相対パス |

1回の結果に載せる finding は 500 件までとする。

## 15. Evaloom の義務

1. 段ごとに 6.2 の表のとおりの隔離を用意し、作業の段に鍵と、確かめる段・出す段のプログラムを見せない。
2. Profile は、登録時に記録した commit の中身だけを使う。
3. 段の結果は、結果 JSON と、Evaloom が自分で得た値（終了コード、使用量、時間）だけから決める。
4. 承認は、管理画面で本人が判断したものだけを使う。承認の照合と消費は1つの書き込みで行い、問い合わせに答えられないときは許可しない。
5. 渡した鍵の値をログと成果物から除外する。
6. Execution ごとに、使った Profile の版と commit、渡した値、段ごとの結果と成果物を記録し、後から確認できるようにする。
7. 契約の版を上げるときは、前の版の Profile をいつまで動かすかを決めて、この文書に書く。

## 16. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q110 | 鍵の割り当ての単位 | 鍵は `<スペシャルハーネス ID>/<slot>` ごとに1つ保管する。Project ごとに別の鍵が要る場合は後で足す（Project の6項目を増やさないため） |
| Q113 | スペシャルハーネスへ直接振り分けた仕事の承認を省く設定と Runner | 承認は省けない。Runner は全体設定の既定の Runner を使う |
| Q114 | 通話口の方式 | Runner が段ごとに作る Unix ドメインソケットを隔離の中に置く。コンテナと macOS VM の両方で使えるかを実装時に確かめ、使えなければ標準入力と標準出力とは別のファイル記述子で同じ形の要求と応答をやり取りする |
| Q115 | 渡す値の検証 | Profile が `params.schema` を持つなら、Project の登録・更新のときと claim のときに形だけ確かめる。中身の意味は確かめない |
| Q123 | `contentId` の形 | 1〜256 文字の印字可能な ASCII。比べ方は完全一致だけ |
| Q124 | 質問の後に同じセッションを再開できないとき | 新しいセッションを始め、元の質問と本人の回答を要求 JSON の `answer` で渡す。Profile が `stages.work.resume: required` と宣言したときは、再開できなければ失敗（`work_failed`）にする |

## 改訂の記録

| 日付 | 内容 |
|---|---|
| 2026-11-10 | 第1版の下書きを作る（vision.md と 2026-11-10 の本人決定から） |
| 2026-11-10 | 整合のため任意項目を足す。`stages.work.resume`（Q124）、`stages.<段>.runtime`（Q147）。`slot` の形（Q138）、`toolPermissions` の登録時の検査（Q145）、`network.allow` にモデルの API を書けないこと（Q146）を明記する |
