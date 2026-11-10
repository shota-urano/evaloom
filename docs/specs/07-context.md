---
status: draft
written: 2026-11-10
範囲: 文脈の4つの層（global・harness・task・reported）の置き場所と固定方法、記録の受け渡しの固定、Context Builder、Execution Context の形
---

# 07. Context 管理 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: 作業の段に渡す文脈を層に分けて固定し、Execution ごとに記録して、Execution Context を作る

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。対象リポジトリの `AGENTS.md` を Evaloom が固定する Project の層は持たない。スペシャルハーネスが準備の段で固定して報告したものを、reported の層として記録だけ残す（vision-alignment.md 2章の 07 の行）。テーブルと列挙値の名前は [`02-data-model.md`](./02-data-model.md)、スペシャルハーネスとの約束はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）に従う。

## 1. 目的

- 作業の段に渡す文脈を、Evaloom 全体のもの（global）、スペシャルハーネスのもの（harness）、タスクのもの（task）、スペシャルハーネスが報告したもの（reported）に分けて扱う（requirements 7.1）。
- Execution ごとに、使った文脈の版とハッシュを記録し、後から確かめられるようにする（requirements 7.1、8.3）。
- すべての文脈を毎回渡さず、その Execution の振り分け先に属するものだけを選んで渡す（requirements 7.2）。
- 仕事の種類に固有の文脈（開発ならリポジトリの `AGENTS.md`）を Evaloom が読みにいかない。読むかどうか、何を読むかはスペシャルハーネスが決める（vision.md 4章）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | Global Context のファイル（evaloom リポジトリの `context/`）、Execution に固定した Profile の commit の中身（`stages.work` で宣言したファイル）、スペシャルハーネスの記録（harness_records）、タスクの件名・本文・受け入れ条件、準備の段の結果 JSON の `reportedContext`（契約 6.5） |
| 出力 | context_revisions の行（[`02-data-model.md`](./02-data-model.md) 3.3）、Execution Context（5章）、`context_pinned` イベント |

## 3. 処理詳細

### 3.1 層と置き場所

| layer | kind | 正本 | 固定の単位 | 作業の段への渡し方 | 状態 |
|---|---|---|---|---|---|
| `global` | `global_policy` | evaloom リポジトリの `context/` のファイル | ファイルごとの内容とハッシュ | Execution Context の指示文に入れる | 案（Q20、Q140） |
| `harness` | `instructions` / `skill` / `settings` | Profile の commit の中の、`stages.work.instructions` / `skills` / `settings` で宣言したファイル | Profile の commit SHA と、ファイルごとのハッシュ | 専用設定環境に置く（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.5） | 決定（契約 6.3） |
| `harness` | `record` | 同じスペシャルハーネスの記録（harness_records） | 記録の ID と、ファイルのハッシュ | `records/` に読み取り専用のファイルとして置く（契約 9章） | 決定（契約 9章） |
| `task` | `task_input` | タスクの件名・本文・受け入れ条件 | Execution を作った時点の値 | Execution Context の中で、タスクの内容として区切って渡す | 決定（requirements 7.1） |
| `reported` | `reported` | 準備の段の結果 JSON の `reportedContext` | スペシャルハーネスが報告した版とハッシュ | 渡さない（記録して表示するだけ） | 決定（02 の 3.3） |

- Evaloom は、作業領域の中のファイル（開発なら対象リポジトリの `AGENTS.md` や `CLAUDE.md`）を読まず、固定もしない。ミクロハーネスが作業領域から自分で読むファイルは、スペシャルハーネスが準備の段で用意し、必要なら `reportedContext` で報告する。
- Evaloom 側で Project ごとの文脈を持つ仕組みは作らない。Project が持つのは6項目だけである（vision.md 4章、本人決定 6）。

### 3.2 Harness の層の固定

Runner が作業の段を始める前に行う。

1. Execution に固定した Profile の commit（executions.profile_commit_sha）から、`stages.work.instructions` / `skills` / `settings` で宣言したファイルを読む。スペシャルハーネスのリポジトリの作業ツリーは使わない（契約 4.2）。読み出し方は [`09-sandbox.md`](./09-sandbox.md) 3.7 に従う。
2. 読んだファイルの相対パスとハッシュを、Runner 用 API `POST /api/executions/{id}/context`（5章）で Control Plane に送る。
3. Control Plane は、context_revisions に layer=`harness`、kind=`instructions` / `skill` / `settings`、source_revision=Profile の commit SHA で保存する。内容は保存しない（Profile の commit から読み直せるため）。
4. 読んだファイルは、作業の段の専用設定環境に置く（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.5）。

`stages.work` で宣言していないファイルは、作業の段に置かない。確かめる段のプログラムと基準は、ここで読まない（契約 4.1、15章の1）。

### 3.3 記録の受け渡しの固定

1. Control Plane は、同じスペシャルハーネスの harness_records のうち deleted_at が NULL の行を、新しい順に Profile の `records.pass.recent` 件まで選ぶ（契約 9章）。
2. 選んだ記録を、context_revisions に layer=`harness`、kind=`record` で保存する。source_path は `records/` に置くときのファイル名（harness_records.file_name）、source_revision は harness_records.id、content_hash は記録のファイル（artifacts.sha256）のハッシュとする。内容は保存しない。
3. Runner は、Execution Context の応答に入った記録の一覧に従って、成果物を `records/` に読み取り専用で置く。
4. 再開（質問への回答の後）でも、同じ Execution で固定した記録を使う。途中で本人が記録を消しても、固定した一覧は変えない（Q141）。

記録は前の AI が書いたものを含むため、指示ではなく参考資料として扱う（契約 6.6）。

### 3.4 Reported の層の記録

1. 準備の段の結果 JSON に `reportedContext` があれば、Control Plane は1件ごとに context_revisions に layer=`reported`、kind=`reported` で保存する。source_path は `<source>:<path>`、source_revision は報告された `revision`、content_hash は報告された `sha256` とする。content は NULL とする。
2. Evaloom は、報告された中身を読まず、ハッシュも確かめない。スペシャルハーネスの報告として、管理画面の Execution 詳細に表示するだけである。
3. 準備の段が無い Profile、または `reportedContext` が空の場合は、この層の行を作らない。

### 3.5 Global の層の固定

1. Control Plane が evaloom リポジトリの `context/` から、作業の段に渡すファイルを選ぶ（3.7、Q140）。
2. 選んだファイルの内容とハッシュを context_revisions（layer=`global`、kind=`global_policy`）に保存する。

### 3.6 Task の層の固定

タスクの件名・本文・受け入れ条件を、1件の context_revisions（layer=`task`、kind=`task_input`）として内容とハッシュごと保存する。Issue の本文などは外から来た文であり、信頼済みの命令として扱わない（requirements 15.1、契約 6.6）。Execution Context の中では、タスクの内容として区切って渡し、Global の安全・権限ルールと同じ扱いにしない。

### 3.7 Context Builder（requirements 7.2）

Control Plane が持つ。Runner は、準備の段が終わった後（準備の段が無ければ claim の後）、作業の段を始める前に、Harness の層の固定の結果を送り、応答として Execution Context を受け取る（`POST /api/executions/{id}/context`）。

| 処理（requirements 7.2） | 内容 |
|---|---|
| 文脈の取得 | 3.2〜3.6 の層を集める |
| 関連情報の選択 | Global は全仕事に共通のファイルだけを選び、仕事の種類では選び分けない（Q140）。Harness は Profile が `stages.work` で宣言したものだけ。記録は同じスペシャルハーネスのものを `records.pass.recent` 件まで。Task は当該タスクだけ。Reported は渡さない |
| 版の確認 | Runner が送った Profile の commit SHA が executions.profile_commit_sha と一致することを確かめる |
| 優先順位の判定 | 3.8 |
| 権限の確認 | その Execution の振り分け先（Project またはスペシャルハーネス）とスペシャルハーネスに属さない文脈を含めない。別のスペシャルハーネスの記録を含めない |
| 情報量の調整 | Global の上限を超える場合の扱いは実装時に決める。記録の件数は Profile の `records.pass.recent`（0〜50）で決まる |
| 実行先向けの整形 | 要求 JSON（契約 6.1）の作業の段に要る項目と、Global・Task の層を指示文に整形する。ミクロハーネスごとの渡し方は Adapter が決める（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.6・3.7） |

### 3.8 優先順位と矛盾（requirements 7.3）

- Global の安全・権限ルールは、ほかの層で上書きできない。安全・権限ルールは指示文ではなく、段ごとの Sandbox と Runner の権限で強制する（[`09-sandbox.md`](./09-sandbox.md)）。指示文の並びは Global → Harness → Task とする（案）。
- 記録と Reported は参考資料であり、優先順位の並びに入れない。指示文の中では、記録は `records/` にある参考資料として示すだけにする。
- 矛盾の自動検出は、構造的な確認（宣言したファイルの欠け、版の不一致）に限る。意味の矛盾を検出する方法は requirements に無い（21章「Context Builder の優先順位・競合解決」）。
- 解決できない重要な矛盾を検出したら作業の段を始めず、Execution を `blocked`（cause=`context_conflict`、[`02-data-model.md`](./02-data-model.md) 5.6）にする。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 層 | `global` / `harness` / `task` / `reported` | 02 の 3.3 |
| Project の層 | 持たない。対象リポジトリの文脈はスペシャルハーネスが固定して報告する | vision-alignment.md 2章、02 の 3.3 |
| 記録の受け渡し | layer=`harness`、kind=`record` | 02 の 3.3、契約 9章 |
| 記録する内容 | ハッシュと版。内容は global と task だけ保存する | requirements 7.1 |
| 文脈の版の固定時点 | Profile は claim の時点。Global・Harness・記録・Task は作業の段の前の context API の時点。Reported は準備の段の結果を受けた時点 | requirements 8.3 |
| 全文脈の毎回の受け渡し | しない | requirements 7.2 |
| Global の安全・権限ルール | ほかの層で上書きできない | requirements 7.3 |
| 作業領域の中のファイル | Evaloom は読まず、固定しない | vision.md 4章 |

## 5. インターフェース

- Runner 用 API `POST /api/executions/{id}/context`（[`01-architecture.md`](./01-architecture.md) 5.2）
  - 要求: `{ "profileCommitSha": "<40桁>", "files": [ { "kind": "instructions" | "skill" | "settings", "path": "<Profile の commit の中の相対パス>", "sha256": "<小文字16進>" } ] }`
  - 応答: Execution Context

```json
{
  "instructions": "<ミクロハーネスに渡す指示文。Global と Task の層、要求 JSON の作業の段の項目を含む>",
  "records": [ { "fileName": "2026-11-09-ENG-120-report.md", "artifactId": "01J..." } ],
  "revisionIds": ["01J...", "01J..."]
}
```

- 同じ Execution で2回目以降に呼ばれたとき（質問への回答の後の再開）は、新しく固定せず、最初に固定した context_revisions から同じ Execution Context を返す（Q141）。
- テーブル: context_revisions（[`02-data-model.md`](./02-data-model.md) 3.3）
- イベント: `context_pinned`（[`02-data-model.md`](./02-data-model.md) 5.3）

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Runner が送った Profile の commit SHA が Execution に固定した値と違う | `failed`（`config_error`） |
| `stages.work` で宣言したファイルが Profile の commit に無い | `failed`（`config_error`） |
| Global Context のファイルが読めない | `failed`（`config_error`） |
| 記録の成果物のファイルが無い | その記録を渡さず、Runner のログに残して続ける（記録は参考資料のため） |
| `reportedContext` が契約の形に合わない | `failed`（`contract_error`。準備の段の結果として扱う） |
| 解決できない重要な矛盾 | `blocked`（`context_conflict`） |

## 7. スコープ外

- Evaloom 側での Project ごとの文脈の管理
- 作業領域の中のファイル（対象リポジトリの `AGENTS.md` など）の読み取りと固定。スペシャルハーネスが行う
- 文脈の更新候補の提出と承認（requirements 7.4）
- 意味の矛盾の自動検出
- 管理画面での Global Context の編集（requirements 16.8 の Global Context は表示だけとする案）

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) 6.1 要求 JSON、6.3 作業の段、6.5 `reportedContext`、9章 記録の受け渡し
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) Runner 用 API
- [`02-data-model.md`](./02-data-model.md) context_revisions、harness_records
- [`05-runner.md`](./05-runner.md) 段の実行手順
- [`08-harness-adapter.md`](./08-harness-adapter.md) 専用設定環境への配置、指示文の渡し方
- [`09-sandbox.md`](./09-sandbox.md) 固定版の読み出し、作業の段の見える範囲

## 9. 実装単位

- [ ] [Backend] Profile の commit からの `stages.work` のファイルの読み出しと固定
  - 受け入れ基準: スペシャルハーネスのリポジトリの作業ツリーやタグを動かしても、固定した commit の内容が読まれる; `stages.work` で宣言していないファイル（確かめる段のプログラムを含む）が専用設定環境に置かれない; 宣言したファイルが無いと failed(config_error) になる
- [ ] [Backend] context API と context_revisions への保存
  - 受け入れ基準: 1つの Execution について global / harness / task の行が保存され、準備の段が `reportedContext` を返したときだけ reported の行が保存される; layer=project の行を書けない; 同じ Execution で2回呼ぶと新しい行を作らず同じ Execution Context が返る
- [ ] [Backend] 記録の受け渡しの固定
  - 受け入れ基準: 同じスペシャルハーネスの記録だけが新しい順に `records.pass.recent` 件まで選ばれ、layer=harness・kind=record で保存される; 本人が消した記録が選ばれない; 別のスペシャルハーネスの記録が含まれない
- [ ] [Backend] Context Builder（選択、版の確認、整形、矛盾の時の Blocked）
  - 受け入れ基準: 別の振り分け先の文脈が含まれない; Execution に固定した版と違う commit SHA を送るとエラーになる; Issue の本文がタスクの内容として区切られて指示文に入る; 構造的な矛盾のテストケースで blocked(context_conflict) になる

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q140 | Global Context をどう選ぶか（以前は「開発タスク向けに指定したファイル」） | 決定（2026-11-10 本人、案を採用）: `context/global/` の下のファイルを全部、すべての仕事の作業の段に渡す。仕事の種類で選び分けない（Evaloom は仕事の種類を知らないため。vision.md 4章）。仕事の種類ごとの指示は、スペシャルハーネスの `stages.work.instructions` に書く |
| Q141 | 質問への回答の後に作業の段を再開するとき、文脈を固定し直すか | 決定（2026-11-10 本人、案を採用）: 固定し直さない。同じ Execution で最初に固定した Global・Harness・記録・Task をそのまま使い、本人の回答は要求 JSON の `answer`（契約 8章）だけで渡す。同じ Execution の中で文脈が変わると、再開した AI の前提がずれるため |
