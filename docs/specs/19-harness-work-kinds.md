---
status: draft
範囲: 開発以外の仕事を扱うための、仕事の種類（実行の流れ）、ハーネスリポジトリの共通の形、確かめる部分と出す部分、Linear からハーネスへの直接の振り分け、受け入れ条件を任意にする設定、ハーネスごとの記録と次の実行への受け渡し
---

# 19. 仕事の種類とハーネスの共通の形 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend, Frontend ｜ **範囲**: Q101 の方針を実装の形にする。開発以外の仕事を載せる Phase（評価基盤の Phase 3 より前に入れる）で作る

> **書き直しが要る（2026-11-10）**: [`../vision.md`](../vision.md) で、開発の部品（06・10・11 と Project のテストコマンドなど）を開発ハーネス `software-engineer` へすぐに移すこと、X の運用をいったん外すことを決めた。本書は、その決定に合わせて書き直してから spec-review にかける。

この文書は下書きである。requirements にまだ無い Phase を前提にしているため、本人が requirements を直すまで confirmed にしない（9章）。2026-11-09 に本人が決めた点は Q101 と 3章の各所に書いた。決まっていない点は Q102〜Q109 に載せる。

## 1. 目的

- Linear の Issue を、Git リポジトリ（Project）を通さずにハーネスへ直接振り分けられるようにする（Q101 (1)）。
- 実行の流れを仕事の種類ごとに選べるようにする。いまの流れ（worktree → ハーネス → Gate 2・3 → PR）は開発用の種類としてそのまま残す（Q101 (2)）。
- 作業の結果を確かめる部分と、外へ出す部分を、ハーネスごとに宣言できるようにする。どちらも作業中の AI が書き換えられず、鍵にも触れられない権限で動かす（requirements 原則13・14・15）。
- 受け入れ条件を必須にするかを、振り分け先ごとに選べるようにする（Q101 (3)）。
- ハーネスの成果物を Evaloom が記録として持ち、同じハーネスの次の実行にコンテキストとして渡す（2026-11-09 本人決定、案A）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | Linear の Issue（[`15-linear-intake.md`](./15-linear-intake.md)）、ハーネスリポジトリの Profile 定義（`evaloom/profile.yaml`） |
| 出力 | 仕事の種類ごとの Execution、成果物（artifacts）、確かめた結果、外への出力（Linear コメントなど）、ハーネスの記録 |

## 3. 処理詳細

### 3.1 仕事の種類（work kind）

| 種類 | 作業する場所 | 作業の結果 | 確かめる部分 | 出す部分 | 使う例 |
|---|---|---|---|---|---|
| `code` | Project の worktree（[`06-worktree.md`](./06-worktree.md)） | commit | Gate 2・3（[`10-verification-gates.md`](./10-verification-gates.md)） | push と PR（[`11-git-publisher.md`](./11-git-publisher.md)） | いまの開発の仕事 |
| `artifact` | 実行ごとに作る空の作業ディレクトリ（3.3） | 出力ディレクトリのファイル | ハーネスが宣言した確認のコマンド（3.5） | ハーネスが宣言した出口（3.6） | 調査や文書づくりの仕事 |

- 種類は Profile 定義の `workKind` で決める（3.2）。値が無い Profile は `code` とみなし、いまの Profile と Execution の動きを変えない。
- `code` の工程表は [`05-runner.md`](./05-runner.md) 3.3 のまま変えない。`artifact` の工程表は 3.4 で定める。

### 3.2 ハーネスリポジトリの共通の形

ハーネスリポジトリは次の形にする（案、Q102）。

```
<harness>/
  evaloom/profile.yaml   版、モデル、Adapter、仕事の種類、確かめる部分、出す部分、記録の宣言
  evaloom/check/         確かめる部分のスクリプト（作業中の AI には渡さない。3.5）
  AGENTS.md              作業する部分への指示
  .claude/ または .codex/ 作業する部分の設定と Skills
  knowledge/             ハーネスがためておく情報（方針、文体、調べ方など）
```

[`02-data-model.md`](./02-data-model.md) 4.3 の Profile 定義に、次の項目を足す（案）。

```yaml
workKind: "artifact"              # code | artifact。無ければ code
acceptanceCriteria: "optional"    # required | optional。無ければ required（3.8）
output:
  files: ["report.md", "summary.md"]   # 作業する部分が出力ディレクトリに書くファイル
check:
  command: ["python3", "evaloom/check/check_report.py", "--out", "{out}"]
  timeoutSec: 120
publish:
  - type: "linear_comment"        # 3.6 の出口の種類
    body: "summary.md"
record:
  keep: ["report.md"]             # 記録として残すファイル（3.7）
  pass: { recent: 10 }            # 次の実行に渡す件数
```

- `check`・`publish`・`record` は `workKind: artifact` のときだけ読む。`code` では無視する。
- `output.files` に無いファイルは、確かめる部分にも出す部分にも渡さない。
- 登録時（[`03-registry.md`](./03-registry.md) 3.5）に、`check.command` が参照する `evaloom/check/` 配下のファイルが、その commit に存在することを確かめる。無ければ登録を拒否する。

### 3.3 artifact の作業する場所

1. Runner が Execution ごとに空の作業ディレクトリを作る（`<workRoot>/<task-id>-<execution-seq>/`）。中に `out/`（出力）と `records/`（3.7 の過去の記録、読み取り専用）を置く。
2. ハーネスの指示・設定・Skills・`knowledge/` は、[`07-context.md`](./07-context.md) 3.3 と同じく、固定した commit から読んで Harness Sandbox 内の専用設定環境に置く。`evaloom/check/` は置かない。
3. ハーネスは作業ディレクトリの中だけに書ける。ハーネスリポジトリには書けない（[`09-sandbox.md`](./09-sandbox.md) の書き込み範囲を、worktree の代わりに作業ディレクトリにする）。
4. 作業ディレクトリは Execution の終了後も保持し、保持期間と削除は worktree と同じ扱いにする（[`06-worktree.md`](./06-worktree.md) 3.6、案）。

ネットワークの扱い（調査で Web を読むか）は Q103。

### 3.4 artifact の工程表

| # | 工程 | Execution 状態 | 詳細 |
|---|---|---|---|
| 1 | 作業ディレクトリを作る | `preparing` | 3.3 |
| 2 | Harness Context と記録を固定し、Control Plane に送る | `preparing` | [`07-context.md`](./07-context.md) 3.3、3.7 |
| 3 | Control Plane から Execution Context を受け取る | `preparing` | [`07-context.md`](./07-context.md) 3.6 |
| 4 | Adapter でハーネスを Harness Sandbox 内で起動し、終了まで監視する | `running` | [`08-harness-adapter.md`](./08-harness-adapter.md) |
| 5 | `output.files` を集め、成果物（artifacts、kind=`harness_output_file`）として保存する。ハッシュを記録する | `verifying` | 3.5 |
| 6 | 確かめる部分を Test Sandbox で実行する | `verifying` | 3.5 |
| 7 | 出す部分を実行する | `publishing` | 3.6 |
| 8 | 記録を保存し、使用量を記録して完了を報告する | `succeeded` | 3.7、[`05-runner.md`](./05-runner.md) 3.7 |

- どの工程でも失敗したら以降の工程を行わない（[`05-runner.md`](./05-runner.md) 3.3 と同じ）。
- 工程7は、工程5で保存した成果物（ハッシュが一致するもの）だけを使う。作業ディレクトリのファイルを読み直さない。工程6と7の間でファイルが変わることを防ぐためで、`code` の Gate 3 と同じ考え方である。

### 3.5 確かめる部分

1. Runner が、固定した commit の `evaloom/check/` を `git show` で読み、Test Sandbox 内の一時ディレクトリに置く。作業中の AI が見た作業ディレクトリのファイルは使わない。
2. 工程5で保存した成果物を、Test Sandbox 内の読み取り専用の `{out}` に置く。
3. `check.command` を実行する。終了コード 0 なら合格、それ以外は不合格とする。標準出力と標準エラーは成果物（kind=`check_log`）に保存する。
4. 不合格なら Execution を `failed`（errorClass=`check_failed`、案）にする。本人は管理画面で結果を見て、再実行（[`04-task-lifecycle.md`](./04-task-lifecycle.md)）を選ぶ。

ハーネスが作業中に同じ確認を自分で行うことは妨げない（確認の手順を `knowledge/` に書いておけばよい）。ただし合否を決めるのは工程6だけである（requirements 原則15）。

不合格のとき自動でハーネスに直させるか（再試行の上限など）は Q104。

### 3.6 出す部分

出口の種類は Evaloom が持つ実装から選ぶ。ハーネスは種類と、渡すファイルだけを宣言する。鍵は Control Plane または Runner が持ち、ハーネスには渡さない（requirements 原則13）。

| 種類 | 内容 | 鍵 | この Phase で作るか |
|---|---|---|---|
| `linear_comment` | 宣言したファイルの中身を、元の Issue にコメントとして返す。送信は [`16-linear-feedback.md`](./16-linear-feedback.md) の linear_outbox を使う | Linear の API キー（Q49） | 作る |
| `typefully_draft` | Typefully に下書きとして登録する（Q75） | Typefully の API キー | 作らない（ポスト作成を別のトリガーで動かすときに作る） |

- `linear_comment` の本文は、16 の 3.3 の結果コメントとは別に1件だけ出す。dedupe_key は Execution とファイルの組にする（案）。
- Linear 以外から来たタスク（手動作成）では、`linear_comment` は何もしない。

### 3.7 ハーネスの記録（2026-11-09 本人決定、案A）

ハーネスの成果物を Evaloom が記録として持ち、同じハーネスの次の実行に渡す。ハーネスのリポジトリにも、ハーネスの外の別のフォルダにも書かない。

**保存**

1. 工程8で、`record.keep` に宣言したファイルの成果物を、ハーネスの記録として登録する。記録はハーネス単位で持ち（Profile の版をまたぐ）、Execution・タスク・元の Issue への参照を持つ。
2. 記録を残すのは、確かめる部分に合格して出す部分まで成功した Execution だけとする。

**受け渡し**

1. 工程2で、Control Plane が同じハーネスの記録から、新しい順に `record.pass.recent` 件を選ぶ。
2. Runner が作業ディレクトリの `records/` に読み取り専用で置く。ファイル名は `<記録の日付>-<Issue の識別子>-<ファイル名>` にする（案）。
3. 渡した記録の ID とハッシュを context_revisions（layer=`harness`、kind=`record`）に保存し、どの記録を見て作業したかを後から追えるようにする。

記録を Harness Context の一部として扱うのは、ハーネス単位の情報だからである。ただし版を固定した Harness Context（07 の 3.3）とは違い、実行のたびに中身が変わる。指示としてではなく参考資料として渡す（Task Context と同じく区切って渡す、07 の 3.5）。

選び方を件数以外にするか（Issue の内容に近いものを選ぶなど）は Q105。管理画面で記録を一覧・削除できるようにするかは Q106。

### 3.8 受け入れ条件を任意にする

- Profile 定義の `acceptanceCriteria` が `optional` のハーネスに振り分けたタスクでは、受け入れ条件の見出しが無くても保留にしない（[`15-linear-intake.md`](./15-linear-intake.md) 3.6 の手順5を飛ばす）。
- 見出しがあれば、これまでどおり取り出して tasks.acceptance_criteria に入れる。無ければ空文字列にする。
- 手動作成（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.1）でも、振り分け先のハーネスが `optional` なら受け入れ条件を必須にしない。
- 振り分け先が Project のタスク（`code`）は、これまでどおり必須とする。

### 3.9 Linear からハーネスへの直接の振り分け

いまの振り分け（[`15-linear-intake.md`](./15-linear-intake.md) 3.6）は、実行先ラベル（接頭辞 `repo:`）と Team で Project を引く。これに、ハーネスを引く経路を足す（案、Q107）。

- 全体設定に `linear.harnessLabelPrefix`（既定 `harness:`）を足す。
- harnesses に `linear_team_id` と `linear_routing_label` を足す（例 `harness:software-engineer`）。`UNIQUE(linear_team_id, linear_routing_label)`。
- Issue に付いた実行先ラベル（`repo:` と `harness:` を合わせて）がちょうど1つのときだけ振り分ける。2つ以上なら、いまの `routing_multiple_labels` で保留にする。
- `harness:` で振り分けたタスクは tasks.project_id を NULL にし、tasks.harness_id にハーネスを入れる。実行に使う Profile の版は、ハーネスに「既定の版」を1つ登録して決める（[`03-registry.md`](./03-registry.md) 3.6 の Project への割り当てに相当するもの）。
- 同時実行の制御（[`04-task-lifecycle.md`](./04-task-lifecycle.md)）は、Project 単位に加えてハーネス単位でも数える（上限は案で1）。

### 3.10 Linear の状態の変更（16 の 3.6 への追加）

`artifact` のタスクは PR を作らないため、16 の 3.6 の「PR 作成・承認待ち → PR Review」は当てはまらない。出す部分まで成功したら Done にする案とする（Q108）。採用するかどうかは、本人が報告を読んで別に決める。

## 4. 設定値・確定値

| 項目 | 値 | 状態 |
|---|---|---|
| 仕事の種類 | `code` / `artifact`。無ければ `code` | 案 |
| ハーネスへの振り分けラベルの接頭辞 | `harness:` | 案（Q107） |
| 記録を渡す件数の既定 | 10 | 案（Q105） |
| ハーネス単位の同時実行の上限 | 1 | 案 |
| 確かめる部分の上限時間の既定 | 120秒 | 案 |

## 5. インターフェース

- Profile 定義のスキーマ（[`02-data-model.md`](./02-data-model.md) 4.3）に 3.2 の項目を足す。
- tasks に `harness_id`（FK→harnesses、NULL 可）を足す。project_id と harness_id のどちらか一方だけが入る。
- harnesses に `linear_team_id`・`linear_routing_label`・`default_profile_id` を足す。
- ハーネスの記録の表 `harness_records`（id、harness_id、artifact_id、execution_id、task_id、linear_issue_identifier、created_at）を足す（案）。
- artifacts の kind に `harness_output_file`・`check_log` を足す。
- errorClass に `check_failed` を足す。
- claim の応答に `workKind`・`check`・`publish`・`records`（渡す記録の一覧）を足す。Runner 用 API のエンドポイントは増やさない（[`05-runner.md`](./05-runner.md) 3.9 と同じ方針）。

## 6. エラー処理

| 状況 | 扱い |
|---|---|
| `output.files` に宣言したファイルが無い | Execution を `failed`（errorClass=`check_failed`）。確かめる部分は実行しない |
| 確かめる部分が上限時間を超えた | `failed`（errorClass=`check_failed`）。ログは保存する |
| 出す部分の失敗（Linear の送信） | 16 の 3.4 の再試行に任せる。5回目も失敗したら notification_failed |
| 振り分け先のハーネスに既定の版が無い | タスクを Blocked（`config_missing_assignment`）。15 の 3.6 の手順6と同じ |

## 7. スコープ外

- 開発の仕事（`code`）を dev-specialist に移すこと。06・10・11 のコードを開発の部品として切り離すこと（Q101、別の仕様で扱う）
- Linear 以外のきっかけ（定期実行など）で仕事を頼むこと
- `typefully_draft` の出口
- 記録の意味による選び方（Q105）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) Q75、Q101
- [`02-data-model.md`](./02-data-model.md) 4.3 Profile 定義
- [`03-registry.md`](./03-registry.md) ハーネスの登録と版
- [`05-runner.md`](./05-runner.md) 工程表
- [`07-context.md`](./07-context.md) Harness Context と Context Builder
- [`09-sandbox.md`](./09-sandbox.md) Harness Sandbox と Test Sandbox
- [`15-linear-intake.md`](./15-linear-intake.md) 振り分けと受け入れ条件
- [`16-linear-feedback.md`](./16-linear-feedback.md) コメントと状態の変更

## 9. 確定の前に本人が行うこと

- requirements を直す。基本原則2（ハーネスと業務プロジェクト）、6.1・6.3（振り分けと受け入れ条件）、9.5（手動作成の受け入れ条件）、19章（Phase の一覧に「開発以外の仕事」を Phase 3 の前に足す）。
- Q102〜Q109 に答える。

## 10. 要確認（00-overview の要確認リストに載せる）

| # | 内容 | 案 |
|---|---|---|
| Q102 | ハーネスリポジトリの共通の形（3.2） | 案のとおり。`evaloom/` の下に Evaloom が読むものを、それ以外にハーネスの中身を置く |
| Q103 | artifact の Harness Sandbox からの外部ネットワーク（調査の仕事は Web の記事を読む） | 許可宛先を Profile に宣言し、09 の宛先の許可と同じ仕組みで通す。本人の確認が要る |
| Q104 | 確かめる部分で不合格のとき、自動でハーネスに直させるか | この Phase では直させない。本人が再実行する |
| Q105 | 記録の選び方 | 新しい順に件数だけ。内容の近さで選ぶのは後の Phase |
| Q106 | 管理画面で記録を一覧・削除できるようにするか | 一覧と削除を作る。消した記録は次の実行に渡さない |
| Q107 | ハーネスへの振り分けのラベル | `harness:<ハーネス名>`。`repo:` と合わせて1つだけ付ける |
| Q108 | artifact のタスクの Linear の状態 | 出す部分まで成功したら Done |
| Q109 | 記録を Profile の版をまたいで渡すか | 渡す。記録はハーネス単位で持つ |

## 11. 実装単位

- [ ] [Backend] Profile 定義のスキーマに workKind・acceptanceCriteria・output・check・publish・record を足し、登録時に検証する（3.2）
  - 受け入れ基準: workKind の無い既存の Profile が code として登録できる; check.command が参照するファイルが commit に無い Profile は登録を拒否する
- [ ] [Backend] tasks.harness_id、harnesses の振り分け列と既定の版、harness_records の表を足す（5章）
  - 受け入れ基準: project_id と harness_id の両方が入った行、両方が NULL の行を書けない; 同じ Team とラベルの組を2つのハーネスに登録できない
- [ ] [Backend] Linear からハーネスへの振り分けと、受け入れ条件を任意にする判定（3.8、3.9）
  - 受け入れ基準: `harness:` のラベルが1つだけの Issue がそのハーネスのタスクになる; `repo:` と `harness:` が両方付いた Issue は routing_multiple_labels で保留になる; optional のハーネスでは見出しの無い Issue も保留にならない
- [ ] [Backend] artifact の工程表（作業ディレクトリ、成果物の収集、確かめる部分、出す部分）（3.3〜3.6）
  - 受け入れ基準: ハーネスが作業ディレクトリの外とハーネスリポジトリに書けない; 確かめる部分は commit から読んだスクリプトで動き、作業ディレクトリの同名ファイルを書き換えても結果が変わらない; 確かめる部分が不合格なら出す部分を行わない
- [ ] [Backend] linear_comment の出口（3.6）
  - 受け入れ基準: 宣言したファイルの中身が元の Issue に1回だけコメントされる; 手動作成のタスクでは何もしない
- [ ] [Backend] ハーネスの記録の保存と受け渡し（3.7）
  - 受け入れ基準: 成功した Execution の keep のファイルだけが記録になる; 次の実行の records/ に新しい順に recent 件が読み取り専用で置かれる; 渡した記録が context_revisions に残る
- [ ] [Frontend] ハーネスへの振り分けの設定と、記録の一覧（3.9、Q106）
  - 受け入れ基準: ハーネスの画面で Team・ラベル・既定の版を設定できる; 記録を一覧で読める
- [ ] （人間） requirements の原則2・6.1・6.3・9.5・19章を直す（9章）
