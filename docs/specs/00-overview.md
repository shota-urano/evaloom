# Evaloom システム全体仕様書（Overview） — Phase 1（Phase 2 は下書き）

**一次情報源**: [`../requirements.md`](../requirements.md)（基本仕様書 v1.2）
**対象範囲**: 19章「Phase 1：基本実行基盤」と 20章「Phase 1 受け入れ条件 1〜18」、および全Phase共通の条件31。Phase 2（19章「Phase 2：Linear連携」、受け入れ条件19〜22）は 15〜17 に下書き（status: draft）がある
**本書の役割**: Phase 1 詳細仕様書群と Phase 2 の下書きの索引。各詳細仕様書のファイル名はこの文書の「3. 詳細仕様書一覧」から辿る。

---

## 1. プロダクト概要

Evaloom は、複数のAIハーネス（Phase 1 では Codex と Claude Code）を中央から統括し、本人が管理画面から作った開発タスクを、プロジェクトに割り当てた Harness Profile で実行して PR を作るシステムである。

Phase 1 の前提は次のとおり。

- 利用者は本人1人。管理画面と Control API は Mac mini のループバック（127.0.0.1）からだけ接続できる（requirements 15.5）。
- タスクの入力元は管理画面からの手動指示だけ。Linear 連携は Phase 2（requirements 19章）。
- 扱うタスクは開発タスク（コード変更と PR 作成）だけ（requirements 8.1）。
- 危険な操作は指示で禁じず、権限の外に置く（requirements 1.3 原則13）。ハーネスは commit まで、push と PR 作成は Runner の Git Publisher が行い、PR のマージは本人が GitHub 上で行う（requirements 8.5）。
- 評価基盤は Phase 3。Phase 1 の Profile はすべて「暫定運用」として登録・実行する（requirements 11.1、20章 条件23）。

## 2. システム全体像

### 2.1 構成要素と対応仕様

```
                 本人（Mac mini 本体のブラウザ）
                          │  127.0.0.1 のみ / 本人用認証 + CSRF
                          ▼
┌──────────────────────────────────────────────────────────────┐
│ Management UI (React)                 → 13-management-ui.md   │
└──────────────────────────────────────────────────────────────┘
                          │ 本人用 API / SSE
                          ▼
┌──────────────────────────────────────────────────────────────┐
│ Control Plane (Go, launchd 常駐)        → 01-architecture.md   │
│  ├ Control API（本人用 / Runner用を分離）→ 01-architecture.md   │
│  ├ Registry（Project / Harness / Profile）→ 03-registry.md     │
│  ├ Task / Execution 管理・同時実行制御  → 04-task-lifecycle.md │
│  ├ Context Builder                     → 07-context.md        │
│  ├ 通知                                → 12-notification.md   │
│  └ SQLite (WAL)                        → 02-data-model.md     │
└──────────────────────────────────────────────────────────────┘
                          ▲ Runner 用 API（Runner から pull のみ）
                          │ Runner 専用の認証情報
┌──────────────────────────────────────────────────────────────┐
│ Trusted Runner (Go, launchd 常駐)       → 05-runner.md         │
│  ├ Worktree 準備                       → 06-worktree.md       │
│  ├ Harness Adapter (Codex / Claude)    → 08-harness-adapter.md│
│  ├ 検証 Gate 2・3                      → 10-verification-gates.md │
│  └ Git Publisher（push / PR 作成）      → 11-git-publisher.md  │
│                                                                │
│  ┌ Harness Sandbox ┐  ┌ Test Sandbox ┐  → 09-sandbox.md        │
│  │ AI がコード変更 │  │ setup / test │                         │
│  └────────────────┘  └──────────────┘                         │
└──────────────────────────────────────────────────────────────┘

隔離・権限境界を突破できないことの自動テスト → 14-security-tests.md
```

### 2.2 タスク処理フロー（Phase 1）と担当仕様

requirements 8.2 の16工程のうち、Phase 1 で動かす工程と担当仕様の対応。

| # | 工程（requirements 8.2） | Phase 1 の扱い | 担当仕様 |
|---|---|---|---|
| 1 | イベント受信 | 管理画面からの手動タスク作成のみ | 04 |
| 2 | 認証・署名確認 | 本人用 API の認証 + CSRF | 01 |
| 3 | 重複排除 | 手動タスク作成の二重送信防止 | 04 |
| 4 | プロジェクト特定 | 本人が作成時に指定 | 04 |
| 5 | Context 取得 | Execution 確定時に版を固定 | 07 |
| 6 | タスク分類 | 行わない（Default Harness で実行） | 04 |
| 7 | Harness 選択 | プロジェクトの Default Harness | 03, 04 |
| 8 | Collaboration Mode 判定 | Solo 固定 | 04 |
| 9 | Worktree 準備 | 実施 | 06 |
| 10 | Harness 実行（commit まで） | 実施 | 08, 09 |
| 11 | テスト・検証 | Gate 2・3（Gate 1 は Q36 で廃止） | 10 |
| 12 | push・PR 作成 | Git Publisher | 11 |
| 13 | 評価 | 行わない（Phase 3） | - |
| 14 | 承認 | 行わない（Phase 2）。PR マージは本人が GitHub で行う | - |
| 15 | 結果返却 | 管理画面表示 + macOS 通知 | 12, 13 |
| 16 | 履歴保存 | 実施 | 02, 05 |

## 3. 詳細仕様書一覧（索引）

この表が有効な仕様の唯一の一覧である。下流（spec-to-beads）はこの表に載る仕様だけを使う。15〜17 は Phase 2 の下書き（status: draft）で、本人が /spec-review で確認するまでタスク化しない。

| # | ファイル名 | 範囲 | 担当 |
|---|-----------|------|------|
| 01 | [`01-architecture.md`](./01-architecture.md) | 整合性アンカー：構成要素、プロセス、権限境界、認証、API 一覧と共通規約 | Backend |
| 02 | [`02-data-model.md`](./02-data-model.md) | 整合性アンカー：Phase 1 のテーブル、状態の列挙値、共通契約・Profile のスキーマ | Backend |
| 03 | [`03-registry.md`](./03-registry.md) | Project / Harness / Harness Profile の登録、パス検証、版の固定、割り当て | Backend |
| 04 | [`04-task-lifecycle.md`](./04-task-lifecycle.md) | 手動タスク作成、タスク・Execution の状態遷移、同時実行制御、再起動時の回収、再実行、中止、Blocked からの続行 | Backend |
| 05 | [`05-runner.md`](./05-runner.md) | Local Runner の pull ループ、Execution の実行手順、ログ収集、タイムアウト、キャンセル | Backend |
| 06 | [`06-worktree.md`](./06-worktree.md) | worktree の作成・再利用、開始点 SHA の記録、セットアップコマンド、コピー許可リスト | Backend |
| 07 | [`07-context.md`](./07-context.md) | 4階層 Context の Phase 1 での扱い、Context Builder、AGENTS.md と設定の固定 | Backend |
| 08 | [`08-harness-adapter.md`](./08-harness-adapter.md) | 共通契約、Adapter インターフェース、Codex / Claude Code Adapter、ハーネスの専用設定環境 | Backend |
| 09 | [`09-sandbox.md`](./09-sandbox.md) | Harness Sandbox / Test Sandbox の隔離条件、ネットワークポリシー、秘密情報の扱い | Backend |
| 10 | [`10-verification-gates.md`](./10-verification-gates.md) | テスト実行と既存テストの照合（Gate 2）、SHA と作業ツリーの検証（Gate 3）。Gate 1 は Q36 で廃止 | Backend |
| 11 | [`11-git-publisher.md`](./11-git-publisher.md) | 検証済み commit の push と PR 作成、ブランチ命名、再実行時の PR の扱い | Backend |
| 12 | [`12-notification.md`](./12-notification.md) | macOS 通知（完了・失敗・Blocked・タイムアウト） | Backend |
| 13 | [`13-management-ui.md`](./13-management-ui.md) | Phase 1 の管理画面（Dashboard / Projects / Harnesses / Tasks / Settings） | Frontend |
| 14 | [`14-security-tests.md`](./14-security-tests.md) | 設定環境の分離テスト、権限境界テスト、隔離突破テスト、Gate の受け入れテスト | Backend |
| 15 | [`15-linear-intake.md`](./15-linear-intake.md) | Phase 2（draft）：Linear API の定期照会（ポーリング）による ai-ready 付き Issue の取得、付与・取り外しの検出と重複排除、Team + Label ルーティング、自動タスク作成、ai-ready の取り外し・キャンセルによる中止 | Backend, Frontend |
| 16 | [`16-linear-feedback.md`](./16-linear-feedback.md) | Phase 2（draft）：Linear Issue への結果返却（コメント）、受け入れ条件の記入依頼、送信の再試行、循環トリガーの防止 | Backend |
| 17 | [`17-approval.md`](./17-approval.md) | Phase 2（draft）：承認対象と承認ポリシー、承認と操作内容の結び付け（head SHA が変わったら無効）、承認後の PR マージ、ベースブランチへの直接 push（Q35） | Backend, Frontend |

### 3.1 受け入れ条件（requirements 20章）と担当仕様

| 条件 | 内容（要約） | 担当仕様 |
|---|---|---|
| 1 | 複数のローカル Git プロジェクトを登録できる | 03 |
| 2 | 中央・ハーネス・プロジェクトを独立リポジトリで管理できる | 03 |
| 3 | 各リポジトリを任意のディレクトリへ配置できる | 03 |
| 4 | 共有ハーネスを複数プロジェクトから利用できる | 03 |
| 5 | プロジェクトごとに Profile の版を固定できる | 03, 04 |
| 6 | Codex と Claude Code を指定の worktree で実行できる | 06, 08 |
| 7 | 実行状態とログを確認できる | 05, 13 |
| 8 | Global / Harness / Project / Task Context を分離できる | 07 |
| 9 | 各ハーネスが共通契約に従って実行できる | 08 |
| 10 | 再起動後に未完了タスクを検出し、中断を失敗として記録できる | 04 |
| 11 | ハーネスが本人のグローバル設定を読み込んでいないことをテストで確認できる | 08, 14 |
| 12 | ハーネスの commit から Runner が PR を作成できる | 11 |
| 13 | 同時実行の上限（全体2本・同一プロジェクト1本）が守られる | 04 |
| 14 | ハーネスから Control API・DB・GitHub 書き込み認証情報にアクセスできないことをテストで確認できる | 09, 14 |
| 15 | テストした commit と同じ commit だけが push される | 10, 11 |
| 16 | セットアップとテストのコードから管理 DB・Control API・秘密情報にアクセスできないことをテストで確認できる | 09, 14 |
| 17 | （欠番。Q36 で Gate 1 を廃止した） | - |
| 18 | 既存テストが欠けて実行された場合に push せず止まる | 10, 14 |
| 31 | 新しい Adapter・モデルの追加で中央のコードは登録処理以外を変更しない | 08 |

### 3.2 Phase 2 の受け入れ条件（requirements 20章）と担当仕様（draft）

| 条件 | 内容（要約） | 担当仕様 |
|---|---|---|
| 19 | Linear Team と Issue Label から実行対象を識別できる | 15 |
| 20 | Linear Project を使用しなくても自動実行できる | 15 |
| 21 | 重要操作を人間の承認対象にできる | 17 |
| 22 | 承認した後に対象の内容（PR の head commit SHA など）が変わると、その承認では操作が実行されない | 17 |

Phase 2 で動かす requirements 8.2 の工程: 1 イベント受信（Linear API の定期照会）・2 確認（Linear からの受信口を持たないため署名の確認は無く、API キーで Linear に接続する）・3 重複排除（同じ付与から二重にタスクを作らない）・4 プロジェクト特定（15）、14 承認（17）、15 結果返却（16）。Phase 2 の実装に入るときに書き換える必要がある確定済みの仕様は、15 の 3.9 と 17 の 3.10 にまとめた。

## 4. 確定済みの初期値（横断・変更禁止）

requirements で確定している値。各詳細仕様にも再掲する。変更するときは requirements を先に改訂する。

| 項目 | 値 | 出典 | 再掲先 |
|---|---|---|---|
| 同時実行の上限（全体） | 2本 | 8.7 | 04, 05 |
| 同時実行の上限（同一プロジェクト） | 1本 | 8.7 | 04, 05 |
| 自動リトライ回数 | 0回 | 8.3 | 04 |
| 1タスクあたりのアクティブな PR | 最大1本 | 8.5 | 11 |
| ブランチ名 | `evaloom/<task-id>-<短い名前>` | 8.5 | 06, 11 |
| Collaboration Mode | Solo 固定（Phase 5 まで） | 8.2 | 04 |
| タスク分類 | 行わない。Default Harness で実行 | 8.2 | 04 |
| task_steps | Phase 1 では使わない | 8.2 | 02, 04 |
| 接続元 | 127.0.0.1 のみ | 2.2, 15.5 | 01 |
| Profile の版 | SemVer を正本とし、Git commit SHA を記録。公開済みタグの付け替え禁止 | 5.4 | 02, 03 |
| Profile の状態 | Phase 1 はすべて暫定運用 | 11.1 | 03 |
| Project Context の正本 | 各プロジェクトリポジトリの `AGENTS.md` | 7.1 | 07 |
| テストコマンドの正本 | Project Registry（Evaloom 側）。ハーネスは変更できない | 8.5 | 03, 10 |
| worktree へのファイルコピー | 許可リストのみ。`.env` 等を無条件にコピーしない | 5.1, 8.6 | 03, 06 |
| setup / test の実行場所 | Test Sandbox（ホスト上で直接実行しない） | 8.6, 15.7 | 06, 09, 10 |
| push・PR 作成の実行者 | Runner（Git Publisher）。ハーネスは commit まで | 8.5 | 08, 11 |
| PR マージ | 本人が GitHub 上で行う。Evaloom はマージしない | 8.5, 15.3 | 11 |
| 再起動で中断した Execution | 失敗（中断）として記録し、タスクを Failed。worktree は保持 | 8.3 | 04, 06 |
| Profile・Context の版の固定時点 | Runner がタスクを取得した時点 | 8.3 | 04, 07 |
| 承認対象操作（15.3） | Phase 1 では Runner の実行権限で拒否する | 15.3 | 09 |
| 通知先（Phase 1） | macOS 通知 | 8.8 | 12 |
| `users` テーブル | Phase 1 では作らない | 17 | 02 |

## 5. スコープ外（Phase 1 では作らない）

次のうち Phase 2 の項目（最初の3つ）は、15〜17 に下書きがある。

- Linear の取り込み（API の定期照会）、Team + Label ルーティング、Linear へのコメント返却（Phase 2）
- 承認の仕組み（approvals、15.8 の承認と操作内容の結び付け）（Phase 2）
- 承認ポリシーで PR マージを承認対象から外した Project の、ベースブランチへの直接 push（8.5, 15.3。Phase 2、Q35）
- 評価（ルーブリック、LLM-as-a-Judge、人間による評価入力、非公開テスト）（Phase 3）
- 改善候補、比較実験、試験運用、Profile のロールバック API（Phase 4）
- AI 協議（Review / Debate / Adaptive）（Phase 5）
- Model Intelligence、複数 Runner、Gmail / GitHub 等の連携（Phase 6）
- 調査・文書タスク（8.1）
- タスク分類・task_steps（8.2）
- 利用者管理・`users` テーブル（15.5, 17）
- 別端末・遠隔からのアクセス（15.5）
- Evaloom 側での Project Context 管理（7.1。Phase 1 は `AGENTS.md` が正本）
- 作業開始時点の既存テストを別に固定して再実行する仕組み（8.5 Gate 2）

## 6. 用語

| 用語 | 意味 |
|---|---|
| Control Plane | Control API、Registry、Task 管理、SQLite を持つ中央のプロセス群 |
| Trusted Runner | ジョブ管理、隔離環境の作成、成果物の検証を行う Runner。ハーネスより強い権限を持つが、本人用 API の認証情報は持たない |
| Harness Sandbox | AI（Codex・Claude Code）がコードを変更する隔離環境 |
| Test Sandbox | セットアップコマンドとテストを実行する隔離環境 |
| Git Publisher | 検証済み commit の push と PR 作成だけを行う構成要素 |
| Task | 本人が作った1件の作業依頼。複数の Execution を持てる |
| Execution | Task の1回の実行。Profile と Context の版はこの単位で固定する |
| Harness Profile | Harness Version、Model ID、Prompt Version などを一体で定義したもの。SemVer で版を管理する |
| 割り当て（Project Harness） | プロジェクトの役割（developer 等）に Harness と Profile の版を結び付けたもの |
| 開始点 SHA（base SHA） | worktree 作成時に取得したリモート Base Branch の commit SHA |
| 検証 SHA | Runner がテストを実行した commit の SHA。push する commit と一致しなければならない |
| 暫定運用 | 評価仕様を持たない Profile の状態（11.1） |
| Blocked | 自動では先へ進めず、本人の確認を待つ状態 |

## 7. 要確認リスト（Phase 1 全体と Phase 2 の下書き。Q42〜Q67 が Phase 2、Q68 以降は Phase 1 の工程表の接続（05 の 3.9））

requirements で確定しておらず、本仕様書群で「実装時に確定」または「提案」とした事項。各詳細仕様の該当節にも同じ内容を書いている。

| # | 事項 | 選択肢・提案 | 記載仕様 |
|---|---|---|---|
| Q1 | ハーネスとテストの隔離方式（OS ユーザー分離 / macOS sandbox / コンテナ / VM） | 決定（2026-10-29）: macOS 専用ツールチェーンが要らない Project はコンテナ。決定（2026-11-05、実測後）: Xcode 等が要る Project は、実行ごとに使い捨てる macOS VM（Tart + Softnet）。Softnet は `0.0.0.0/0` を遮断して要る宛先だけ許可する。実測結果と条件は 09 の 3.4 | 09 |
| Q2 | 本人用 API の認証方式と CSRF 対策 | 起動時生成のセッショントークン + SameSite=Strict Cookie + CSRF トークン（提案） | 01 |
| Q3 | Runner 専用 API のエンドポイント（18章には claim だけがある） | heartbeat / events / result / cancel 確認を追加（提案） | 01 |
| Q4 | 18章に無い本人用 API（Project 更新、Harness・Profile 登録、Blocked からの続行 など） | 01 の API 一覧で「追加案」として明記 | 01 |
| Q5 | Harness Profile の定義ファイルの置き場所と読み込み方式（21章） | ハーネスリポジトリ内の定義ファイル + SemVer タグ（提案） | 02, 03 |
| Q6 | 共通契約の各項目の厳密な定義（9.5, 21章） | 02 に Phase 1 案を記載 | 02, 08 |
| Q7 | Claude Code・Codex の認証方式（サブスクリプション / API キー）と各社規約での自動実行の可否 | 決定（2026-10-29）: サブスクリプション。規約の確認（2026-11-05）: 本人だけが自分の Mac mini で使う範囲なら、両社の規約で自動実行してよいと判断した。条件は 09 の 3.5 | 08, 09 |
| Q8 | モデル API の認証情報をハーネスから見えないようにする方式（15.6） | 決定（2026-10-29）: Q7 でサブスクリプションを採ったため、認証付与プロキシは使わず、Evaloom 専用で用途を絞った認証情報を渡す。詳細は 09 の 3.5 | 09 |
| Q9 | ハーネスの専用設定環境の作り方（7.5） | 実行ごとの専用 HOME と各 CLI の設定ディレクトリ指定（提案）。実際に読み込んだ指示を 14 のテストで検証する | 08 |
| Q10 | テストの弱体化として検出する具体的な判定規則（8.5, 21章） | Q36 で Gate 1 を廃止したため不要 | 10 |
| Q11 | 既存テストの ID 一覧・件数をどこから得るか | 開始点 SHA でテストを1回実行して基準値を取る案と、テストランナーの一覧出力を使う案。10 に比較 | 10 |
| Q12 | テストランナーの結果形式（ID・実行数・skip 数の取り出し方） | Project Registry にレポート形式（JUnit XML 等）と出力パスを登録（提案） | 03, 10 |
| Q13 | Gate 1 の基準 SHA：再実行で同じブランチに追加 commit するとき、前回本人が確認済みの差分を再び止めるか | Q36 で Gate 1 を廃止したため不要 | 10 |
| Q14 | Blocked から続行を選ぶ操作を、どの SHA に結び付けるか | 続行の判断を、その時点の HEAD SHA に結び付け、SHA が変わったら無効（提案） | 04, 10 |
| Q15 | タイムアウト値（ハーネス実行・setup・テスト） | 決定（2026-11-06）: Project の timeouts を優先し、無ければ Runner の設定ファイルの既定値（setup 15分、ハーネス 60分、テスト 30分）を使う（[`05-runner.md`](./05-runner.md) 4.1） | 03, 05 |
| Q16 | 費用上限（15.2 必須対策）を Phase 1 で実装するか | 15.2 は必須対策に挙げるが Phase 1 の範囲が不明。トークン数・実行時間の記録（usage_records）は Phase 1 で行う案 | 02, 05 |
| Q17 | macOS 通知の実現方式（21章） | 決定（2026-10-27 実機確認）: 自作の通知用ヘルパーアプリ。クリックでタスク詳細を開く。詳細は 12 の 3.3 | 12 |
| Q18 | Linear コメントによる通知（8.8）を Phase 1 で行うか | Linear API キーの利用が Phase 2（6.6）のため、Phase 1 は macOS 通知だけとする案。Phase 2 では 16 で Linear コメントの Notifier を足す案（Q52） | 12, 16 |
| Q19 | Model Registry（5.3）を Phase 1 で作るか | Phase 1 ロードマップに無い。Profile に Model ID 文字列を持たせ、models テーブルは後の Phase とする案 | 02, 03 |
| Q20 | Global Context と Harness Context の Phase 1 での置き場所 | Global は evaloom リポジトリの `context/`、Harness はハーネスリポジトリ内（Profile の commit SHA で固定）とする案 | 07 |
| Q21 | Go バックエンドのモジュール境界（21章） | 01 に構成要素ごとのパッケージ案 | 01 |
| Q22 | 最初からやり直す場合のブランチ名（8.5, 21章） | `evaloom/<task-id>-<短い名前>-r<n>` 案 | 11 |
| Q23 | Blocked のタスクが同時実行の枠を占有するか | 占有しない案（プロセスは止まっているため）。worktree は保持する | 04 |
| Q24 | ログの機密情報除外（15.2）の判定規則と保存期間（21章） | 既知の秘密情報の値との一致で伏せる案。保存期間は未定 | 05 |
| Q25 | 権限設定（許可操作・承認ポリシー・Tool Permissions）の Phase 1 での中身 | 承認が Phase 2 のため、Phase 1 では「値の保存と表示」と、Runner が拒否する操作の固定リストまでとする案。Phase 2 の承認ポリシーの形と効果は 17 の 3.2（Q60） | 03, 09, 17 |
| Q26 | worktree のクリーンアップの契機（自動か本人の操作か） | PR がマージ・クローズ済みでタスクが終端状態なら削除してよい案。ブランチは残す | 06 |
| Q27 | worktree の元を本人のローカルリポジトリにするか、Evaloom の作業用クローンにするか | 決定（2026-10-29）: 作業用クローン（06 の 3.2.2 の A）。worktree は `.git` を共有するため、ハーネスが本人のリポジトリの ref や設定を書き換えられる | 06, 09 |
| Q28 | setup とテスト実行のネットワークポリシー（パッケージレジストリをどこまで許可するか） | 決定（2026-10-29）: setup はプロジェクトごとに登録した宛先だけ許可、テスト実行は全遮断 | 09 |
| Q29 | Git Publisher の GitHub 認証方式（fine-grained token / GitHub App） | 決定（2026-10-30）: GitHub App（evaloom-publisher）。対象リポジトリだけにインストールし、権限は contents と pull_requests の write、metadata の read に限る。マージの防止は GitHub 側のルールセットで行う（11 の 3.5） | 11 |
| Q30 | 開始点で既存テストの基準値を取るための実行が、requirements 8.5 の「既存テストを別に固定して再実行する仕組みは作らない」に反しないか | 反しないと解釈した（Gate 2 の後の再実行ではないため）。本人の確認が要る | 10 |
| Q31 | ハーネスが commit を1つも作らなかった場合の扱い | failed（execution_failed、理由「変更なし」）の案 | 05 |
| Q32 | fresh で再実行するときに前の PR が開いていたら、Evaloom が自動でクローズするか | 自動ではクローズせず Blocked にして本人に整理を求める案 | 11 |
| Q33 | 実行中にハーネスが `AGENTS.md` 等の指示ファイルを変更した場合に Block するか | 知らせるだけで Block しない案 | 07, 10 |
| Q34 | ハーネスが Web で調べものをしたいときの接続方法（ハーネス用 Sandbox はモデル API にしかつながらない） | 1. モデル側の Web 検索機能に任せる（検索とページ取得はモデル提供元のサーバー側で行われ、Sandbox の接続先は増えない）を推奨。足りなければ 2. コードも認証情報も持たせず結果を文章で返すだけの調査専用 Sandbox を別に用意する。ハーネス用 Sandbox を Web に開放する案は採らない（プロンプトインジェクションとコード・認証情報の持ち出しのため）。ハーネスを Sandbox で実際に動かすタスクの前に決める | 08, 09 |
| Q35 | 本人が見ずに main へ入れてよい Project（例: ハーネスのリポジトリ）で、PR を経ずにベースブランチへ push するか | 決定（2026-10-30）: Phase 2 で、承認ポリシーで PR マージを承認対象から外した Project に限り、Gate 1〜3 を通った commit をベースブランチへ fast-forward で push し（Q36 の後は Gate 2・3）、PR は作らない（requirements 8.5, 15.3 に追記）。既定は今のまま PR を作り本人がマージする。Phase 1 の仕様と実装は変えない。承認ポリシーの値の形、push 前の確認（3.1 のブランチ名の接頭辞の扱い）、GitHub のルールセットで App を例外にする手順は Phase 2 の仕様で決める。Phase 2 の下書きでの案は 17 の 3.2・3.7・3.8（Q60〜Q63） | 11, 17 |
| Q36 | 作業結果の確かめ方のうち、テストの弱体化の検出（Gate 1、受け入れ条件17）を Evaloom 本体に持たせるか | 決定（2026-10-31）: Gate 1 をなくし、開発タスクの確認は Gate 2（登録したテストコマンドをハーネスの外で流し、既存テストの ID がすべて実行され skip が増えないことを照合）と Gate 3（テストした commit と push する commit の一致）、本人の PR レビューとする。理由: 差分のファイル名パターンで弱体化を検出する方式は、テストの道具・設定ファイル・期待値ファイルの種類だけ列挙が要り、codex のレビューを6回受けても影響度大の指摘が残った。Evaloom は将来開発以外のタスクも扱うため、開発に固有の判定は本体に置かない。調査（`docs/research/2026-10-31-ai-coding-verification.md`）でも、差分パターンで検出する製品は無かった。次の改善の候補は、テストを書く回と実装する回に分け、Gate 2 の前に1回目のテストを強制的に戻す方式（SWE-bench と同じ考え方）。requirements 8.5・15.3・15.7・21章、受け入れ条件17（欠番にした）、10 と関連する仕様は書き換え済み。Gate 1 の試作のブランチ（`feat/pdy.1-gate1-diff`、未マージ）は 2026-11-01 に削除した | 10 |
| Q37 | Gate の Finding の `rule` に入れる名前の一覧 | 決定（2026-11-01）: 一覧を先に全部決めず、場面ごとに名前を付ける。新しい名前が要るときは AI が候補を4つ出し、本人が選ぶ。現時点の5つ（`existing_test_not_run` / `test_report_missing` / `test_failed` / `count_only_check` / `changed_after_test`）は 10 の 3.6 | 10 |
| Q38 | JUnit XML のどの属性からテストの ID を作るか | 決定（2026-11-01）: `classname` と `name` を `.` でつなぐ。`classname` が空なら `name` だけ | 10 |
| Q39 | 開始点で基準値を取るとき、テストが失敗した・レポートが無い場合の扱い | 決定（2026-11-01）: 失敗するテストがあっても記録して続ける。レポートも件数も取れなければ作業を始めず `failed`（`config_error`） | 10 |
| Q40 | Runner が再起動した後に、保存済みの基準値のテスト ID 一覧を読む方法（`test_ids` の成果物の形式を含む） | 決定（2026-11-01）: 本人の判断は要らない。Runner と Gate をつなぐタスクで、既存の API の範囲で実装側が決め、仕様に書き足す。書き足した先: 10 の 3.3.3（形式と経路）、04 の 3.3.1（claim の応答の `worktree`） | 04, 05, 10 |
| Q41 | Dashboard のエラー状況に通知失敗を出す方法（全 Execution の `notification_failed` を集めて返す API が無かった） | 決定（2026-11-01）: 本人用 API `GET /api/notification-failures` を追加する。全 Execution の `notification_failed` を新しい順に、共通のページネーションで返す。既読・解決済みの印は持たない。書き足した先: 01 の 5.1、12 の 5（応答の形）と 9（実装単位）、13 の 3.2 | 01, 12, 13 |
| Q42 | Linear Webhook を受ける公開 HTTPS 経路の構成（requirements 6.6, 21章） | 決定を取り消し（2026-11-06）: 本人の決定で Linear の取り込みを Webhook からポーリング（Q43）に切り替えたため、公開 HTTPS 経路は不要になった。理由は、Webhook には Mac mini に外から届く住所が要り、トンネル（Cloudflare Tunnel はドメインが要る）・署名シークレット・Webhook の登録が必要になるのに対し、ポーリングは Linear の API キーだけで済み、Mac mini の停止中の取りこぼしも起きないため。失うのは即時性（照会の間隔ぶんの遅れ）だけで、受け入れ条件19〜22 は Webhook を求めていない。requirements 2.2, 6.3, 6.6, 8.1, 15.2, 18章, 19章, 21章を書き換えた | 15 |
| Q43 | Linear の変化をどう取り込むか（旧: 取りこぼした Webhook を補うか） | 決定（2026-11-07、案を採用）: 決定（2026-11-06、本人）: Control Plane が一定間隔で Linear API に照会する方式を本線にする（requirements 6.6）。照会の対象は linear_team_id を登録した Team の ai-ready 付き Issue。前回の照会結果（linear_issue_states）との差分で付与と取り外しを判定する（15 の 3.3〜3.5）。照会の間隔は1分とする案で、本人の確認が要る | 15 |
| Q44 | Issue 本文のどこを受け入れ条件とみなすか（requirements 6.3） | 決定（2026-11-06、15〜17 の確定時に案を採用）: 見出し「受け入れ条件」または「Acceptance Criteria」から、次の同じ階層以上の見出しまで。空白だけなら書かれていないとみなす案 | 15 |
| Q45 | ルーティングや受け入れ条件で保留したタスクの持ち方と、解除の手順 | 決定（2026-11-07、案を採用）: 決定（2026-11-06、15〜17 の確定時に案を採用）: tasks.project_id を NULL 可にし、intake_block_cause に理由を持つ。Execution は作らない。保留中に Issue の更新が届いたら（2026-11-06 のポーリングへの切り替え後は、照会で Issue の更新時刻が進んでいたら）判定をやり直し、通れば同じタスクを起動する案（requirements 6.2「付いたまま更新されても再び起動しない」の例外になる） | 15 |
| Q46 | 作成後の Issue の更新と、ai-ready の付け直しを、タスクにどう反映するか | 決定（2026-11-06、15〜17 の確定時に案を採用）: 本文と受け入れ条件はタスク作成時に固定し、以後の編集は反映しない。付け直したとき、前のタスクが終端状態なら新しいタスクを作り、終端でないタスクがあれば何もしない案 | 15 |
| Q47 | Linear Project を使ったルーティング（requirements 6.1 の任意機能）を Phase 2 で作るか | 決定（2026-11-06、15〜17 の確定時に案を採用）: 作らない案。条件19・20 は Team + Label で満たせる | 15 |
| Q48 | トリガーのラベル名（`ai-ready`）と実行先ラベルの接頭辞（`repo:`）を固定するか、設定にするか | 決定（2026-11-06、15〜17 の確定時に案を採用）: requirements 6.2 は推奨例としているため、全体設定で持ち、既定値をこの2つにする案 | 15 |
| Q49 | Linear の API キーの置き場所と、どの Linear ユーザーのキーを使うか | 決定（2026-11-06、15〜17 の確定時に案を採用。同日、ポーリングへの切り替えで署名シークレットは不要になった）: API キーを `~/.config/evaloom/`（権限 0600）に置き、Control Plane だけが読む案（GitHub App の秘密鍵と同じ扱い）。本人とは別の Evaloom 専用の Linear ユーザーのキーを使う案。ポーリングでは操作者で見分ける循環防止は行わないため、専用ユーザーにする理由は、Evaloom のコメントを本人のコメントと見分けられることと、キーを無効にしても本人に影響しないことになる（16 の 3.5）。キーの権限を絞れるかは実装時に Linear で確かめる ファイル名は `~/.config/evaloom/linear-api-key` とする（2026-11-07 決定）。 | 15, 16 |
| Q50 | requirements 17章の project_integrations を Phase 2 で作るか | 決定（2026-11-06、15〜17 の確定時に案を採用）: 作らず、projects に linear_team_id と linear_routing_label の列を足す案。連携先が Linear だけのため | 02, 15 |
| Q51 | Evaloom が Linear の Issue の状態（Workflow state）やラベルを変えるか | 決定（2026-11-06、15〜17 の確定時に案を採用）: Phase 2 はコメントだけを返し、状態とラベルは変えない案。Linear 側の自動化を介した循環を避けるため | 16 |
| Q52 | Linear にコメントを出す場面と、本文に載せない情報 | 決定（2026-11-06、15〜17 の確定時に案を採用）: 受付時は出さず、保留・Blocked・承認待ち・完了・失敗・マージ・中止で出す。ログ本文、テストの出力、Runner のパスは載せない案（ワークスペースの他のメンバーも読めるため） | 16 |
| Q53 | Phase 2 で承認を経て Evaloom が実際に行う操作 | 決定（2026-11-06、15〜17 の確定時に案を採用）: PR マージだけ。ほかの承認対象操作は Phase 2 でも権限で拒否する案 | 17 |
| Q54 | 承認後の PR マージを誰が、どの権限で行うか | 決定（2026-11-06、15〜17 の確定時に案を採用）: Runner の Git Publisher が claim で「承認済みの操作」を受け取って行う。GitHub はルールセット B の Bypass に evaloom-publisher を足し、A（PR 必須・CI 必須）の Bypass は空のままにする案。Phase 1 で決めた「Evaloom はマージしない」（05, 09, 11）を書き換えることになる | 11, 17 |
| Q55 | 承認の有効期限の既定値（requirements 15.8「承認ごとに設定する」） | 決定（2026-11-06、15〜17 の確定時に案を採用）: 既定 24 時間、承認時に本人が変えられる案 | 17 |
| Q56 | PR のマージ方式（merge commit / squash / rebase） | 決定（2026-11-06、15〜17 の確定時に案を採用）: merge commit 案。テストした commit がそのまま履歴に残るため。Project ごとに変えるかは未定 | 17 |
| Q57 | 本人が承認を却下したときのタスクの状態 | 決定（2026-11-06、15〜17 の確定時に案を採用）: タスクを failed（理由 approval_rejected）にし、PR は開いたまま残す。本人は continue の再実行で直せる案 | 17 |
| Q58 | 承認待ちの間に、本人が GitHub 上で直接マージ・クローズした場合 | 決定（2026-11-06、15〜17 の確定時に案を採用）: Runner が承認待ちの PR を定期的に読み、承認を invalidated にする。マージならタスクを completed、クローズなら failed にする案 | 17 |
| Q59 | `waiting_approval` と `merging` の Execution を同時実行の枠に数えるか | 決定（2026-11-06、15〜17 の確定時に案を採用）: どちらも数えない案（Blocked と同じく、ハーネスのプロセスが動いていないため） | 04, 17 |
| Q60 | 承認ポリシー（projects.approval_policy）の値の形（Q35 で Phase 2 に回した点） | 決定（2026-11-06、15〜17 の確定時に案を採用）: `{ "version": 1, "mergePullRequest": "require_approval" }`。値は `require_approval` か `direct_push`。ほかの項目は持たせない。既定は `require_approval` とする案 | 03, 17 |
| Q61 | 直接 push の判断に使う承認ポリシーを、claim の時点の値にするか、push 直前の値にするか | 決定（2026-11-06、15〜17 の確定時に案を採用）: push 直前に Control Plane から読み直し、claim の時点と違えば安全側（PR を作る）を選ぶ案。読めなければ push しない | 11, 17 |
| Q62 | 直接 push で、ベースブランチが先へ進んでいて fast-forward できない場合 | 決定（2026-11-06、15〜17 の確定時に案を採用）: 強制 push せず、PR を作る経路へ切り替えて本人のレビューに回す案 | 11, 17 |
| Q63 | 直接 push の Project で、GitHub のルールセットに App を例外として入れる手順（Q35 で Phase 2 に回した点） | 決定（2026-11-06、15〜17 の確定時に案を採用）: force push と削除の禁止を Bypass 空のルールセット C に分け、A と B の Bypass に App を足す案。Bypass の種類は実機で確かめる | 11, 17 |
| Q64 | ポーリングを始めた最初の照会で、すでに ai-ready が付いている Issue を起動するか | 決定（2026-11-07、仕様の確定時に案を採用）: 案: Team ごとの最初の照会では記録するだけで起動しない。導入前から付いている Issue を一斉に起動しないため。起動したい Issue は本人が ai-ready を外して付け直す。Project から Team を外して登録し直したときも、最初の照会からやり直す | 15 |
| Q65 | 照会と照会の間（案では1分）に ai-ready を付けて外した、または外して付け直した変化を見逃してよいか | 決定（2026-11-07、仕様の確定時に案を採用）: 案: 見逃してよい。前者は何も起きず、後者は付いたままとして扱う。Linear の Issue の履歴を読めば見分けられるが、照会の回数と処理が増えるため Phase 2 では行わない | 15 |
| Q66 | requirements 17章の event_inbox を Phase 2 で作るか | 決定（2026-11-07、仕様の確定時に案を採用）: 案: 作らない。Webhook をやめたため、受け取った本文を先に保存して後で処理する必要が無い。前回の照会結果は linear_issue_states、判定の結果は linear_issue_states と audit_logs に残す。Linear 以外のイベント入力（Phase 6）で受信口が要るようになったら作り直す | 02, 15 |
| Q67 | ai-ready が付いたまま、登録していない Team へ移された Issue をどう扱うか | 決定（2026-11-07、仕様の確定時に案を採用）: 案: 取り外しと同じく、待機中・実行中のタスクを中止する（理由 linear_issue_moved）。照会の対象から外れて、以後の取り外しやキャンセルを検出できなくなるため | 15 |
| Q68 | （Phase 1、工程表の接続）Sandbox 1つあたりの CPU・メモリの上限（09 の 3.2 が「実装時に確定」としていた値） | 決定（2026-11-07、仕様の確定時に案を採用）: 案: Runner の設定ファイルに `container.cpus`（既定 4）と `container.memoryMiB`（既定 8192）を足す。実測した Mac mini（14 コア、64 GB）で同時実行2本でも収まり、Tart の実測で使った VM の既定（4 CPU、8 GB）と同じ値。宛先を許可する実行ではプロキシのコンテナ2つにも同じ上限が付くため、1本あたり最大で3つ分の枠になる点に注意（上限であって確保ではない）。Docker の VM に割り当てたメモリより大きくしない | 05, 09 |
| Q69 | （Phase 1、工程表の接続）工程2で確かめる「前回の PR」をどう取り出すか（claim の `previous` は直前の1件だけで、fresh では null） | 決定（2026-11-07、仕様の確定時に案を採用）: 案: claim に `pullRequests`（タスクの過去の Execution が記録した PR を番号ごとに1件、新しい順）を足す。continue は引き継ぐブランチと同じブランチの最新の1件に 3.4 の表を当てる。fresh は全件を見て、1件でもマージ済みなら failed（pr_merged）、開いていれば blocked（git_state）。fresh の Block の headSha は開いている PR の head の SHA を使う | 04, 11 |
| Q70 | （Phase 1、工程表の接続）非公開リポジトリの clone・fetch・PR 取得に使う読み取り用の認証情報 | 決定（2026-11-07、仕様の確定時に案を採用）: 案: Git Publisher と同じ GitHub App のインストールアクセストークンを、権限を contents・metadata・pull_requests の read に絞って発行する。Git には環境変数の `http.extraHeader` で渡し、ファイルや URL に残さない。本人の SSH 鍵・`gh` のログイン・Project ごとの deploy key は採らない | 06, 11 |
| Q71 | （Phase 1、工程表の接続）Harness Sandbox から接続してよいモデル API の宛先 | 決定（2026-11-07、仕様の確定時に案を採用）: 案: Adapter のパッケージに定数で持つ。Codex は `chatgpt.com:443`・`auth.openai.com:443`・`api.openai.com:443`、Claude Code は `api.anthropic.com:443`・`console.anthropic.com:443`・`claude.ai:443` を候補とし、実モデル試験（14 の 3.1）で認証確認と本実行が通る最小の一覧に絞って確定する。CLI が更新確認・テレメトリの宛先に届かないと動かない場合は、宛先を足す前に CLI の設定で止められるかを先に確かめる | 08, 09 |
| Q72 | （Phase 1、工程表の接続）Tool Permissions（harnesses.tool_permissions）の形と、CLI の設定への変換 | 決定（2026-11-07、仕様の確定時に案を採用）: 案: 形は `{ "deny": [CLI の表記のツール名・規則] }`、`{}` は追加の禁止なし。Claude Code は `permissions.allow` に Profile の allowedTools、`permissions.deny` に deny を入れる。Codex（0.163.0）はツールごとの設定が無いため、allowedTools と deny がどちらも空のときだけ起動する。実際の制限は Harness Sandbox で行う | 03, 08 |
| Q73 | （Phase 1、工程表の接続）Q28 の「setup は Project ごとに登録した宛先だけ許可」の登録先（projects に列が無かった） | 決定（2026-11-07、仕様の確定時に案を採用）: 案: projects に setup_allowed_hosts（`host:port` の JSON 配列、既定は空）を足し、Project の登録・更新 API で受け付ける。決まるまでは claim が空配列を返し、setup もネットワークを全遮断する | 02, 03, 09 |
| Q74 | （Phase 1、工程表の接続）Blocked から続行した Execution の PR 本文に載せる作業概要を、どこから取るか（ハーネスを再実行しないため HarnessResult が手元に無い） | 決定（2026-11-07、仕様の確定時に案を採用）: 案: Runner が工程9で HarnessResult の JSON を既存の成果物の種類 `harness_output` で登録し、Control Plane が claim の `execution.resumeHarnessSummary` に summary を入れて返す（Q40 の test_ids と同じ経路）。読めなければ null とし、PR 本文には「作業概要を取得できなかった」と書いて PR 作成は止めない | 02, 04, 11 |
| Q75 | （開発以外のタスクを扱う Phase）タスクの種類ごとの出口と、出口の前の検証を、Project ごとに選べるようにするか。例: X の投稿文を作って Typefully に登録するタスクには、Gate 2・3（テストの弱体化への対策）は当てはまらない | 方針（2026-11-07、本人）: 選べるようにする。Typefully への登録は予約・下書きで、本人が Typefully 上で確認してから公開されるため、登録そのものが PR と同じ確認の場になる。この Project では出口の前の検証を省ける設定にしたい。案: 出口（開発は Git Publisher、X 投稿は Typefully への登録）と、出口の前の検証（開発は Gate 2・3、X 投稿は無し、または文字数などの機械的なチェック）を、タスクの種類または Project ごとに差し替えられる形にする。原則13 により、Typefully の API キーはハーネスに渡さず Runner が登録する。登録した時点で公開される出口を作る場合は、15.3 の承認対象に加えるかを別に決める。Phase 1 の仕様と実装は変えない | - |
| Q76 | Linear の全体設定（linear.triggerLabel / linear.routingLabelPrefix）をどこに保存するか（PR #66 ではプロセス内に持ち、再起動で既定値に戻っていた） | 決定（2026-11-08、本人）: DB に保存して再起動後も保つ。保存先は全体設定の表 settings（02 の 3.4、1キー1行、行が無いキーは既定値）を新しく作る。PATCH は未知のキーと null・空白だけの値を 400 にし、1つでも拒否したら何も変えない。値を変えたら変わったキーごとに audit_logs（settings.update）へ残す。DB の値が空・不正なら既定値を使いログに出す | 02, 15 |
| Q77 | Dashboard を開く macOS 通知を仕様12 に足すか（PR #66 で Linear の照会の5回連続失敗の通知がクリックで / を開くようにし、ヘルパーが受け付ける URL を広げた） | 決定（2026-11-08、本人）: 足す。ヘルパーが開いてよい URL は http://127.0.0.1:<port>/tasks/<taskId> と http://127.0.0.1:<port>/ だけ（12 の 3.6）。この通知は Execution を持たないため execution_events に記録せず、送信の失敗はログにだけ残す。配置済みのヘルパーは作り直して配置し直す | 12, 15 |
| Q78 | 削除・アーカイブされた Linear Issue の linear_issue_states の行を消すか（PR #66 では消すため attach_seq が 0 に戻り、元に戻された Issue が以前のタスクと同じ冪等キーになって何も起きなかった） | 決定（2026-11-08、本人）: 行を消さず削除済みの印（removed_at）を付け、attach_seq を保つ。元に戻されて一覧に再び現れたら ai_ready=1 として記録するだけで起動しない（終わった Issue を戻しただけで再実行しないため。Q51、Q64 と揃える）。もう一度実行したいときは本人が ai-ready を外して付け直し、attach_seq が進んで新しいタスクを作る | 15 |
| Q79 | 削除・アーカイブした Issue を元に戻した後、Evaloom が照会する前に ai-ready を外し、後で付け直した場合をどう扱うか（Evaloom からは元に戻されたことと区別できない） | 決定（2026-11-08、仕様の確定時に案を採用）: 案: 元に戻された場合と同じく記録するだけで起動しない。本人は、Evaloom が記録した後（GET /api/linear/status に出た後）にもう一度外して付け直す。Linear の Issue の履歴を読めば見分けられるが、Q65 と同じく Phase 2 では行わない | 15 |
| Q80 | Linear コメントの送信（16 の 3.4）を何回まで再試行し、間隔をどう空けるか（16 は「上限回数（実装時に確定）」としていた） | 決定（2026-11-08、本人）: 上限は5回。間隔は1分・2分・4分・8分と倍にし、5回目も失敗したら failed と notification_failed（channel=linear）にする。最初の失敗からおよそ15分で諦める | 16 |
| Q81 | 保留したタスクを、Issue の更新以外の契機でも再判定するか（15 の 3.6.3 は Issue の更新時刻が進んだときだけ再判定するため、config_missing_assignment と routing_unknown_label の保留は、管理画面で直しても Issue を更新するまで解けなかった） | 決定（2026-11-08、本人）: Project の linear_team_id・linear_routing_label、default_role、Harness Profile の割り当てを変えたときも、その変更で結果が変わりうる保留（routing_unknown_label・routing_team_mismatch と、その Project の config_missing_assignment）を再判定する。設定を変える API が同じトランザクションで linear_issue_states.recheck_requested_at に印を付け、次の照会で、その回に取得した Issue の中身で判定し直す | 15, 16 |
| Q82 | Execution の無い契機（intake_blocked・missing_acceptance_criteria・保留したタスクの中止）の Linear コメントの送信の失敗を、管理画面に出すか（execution_events に書けないため、linear_outbox の status とログにしか残らなかった） | 決定（2026-11-08、本人）: すべて通知失敗の一覧（GET /api/notification-failures）に出す。linear_outbox に execution_id・failure_reason・failed_at を足し、execution_id が NULL で failed の行を一覧に足す。混ざった一覧の並びは記録した時刻の新しい順にし、cursor はその並びの位置を表す値にする。既存の failed の行は failure_reason=other、failed_at=created_at で埋める | 12, 16 |
| Q83 | 送った結果が分からない Linear コメントを、再起動をまたいで照合するか（PR #69 は照合の要否をプロセスの中に持ち、停止で中断した送信は照合せずに再送していた） | 決定（2026-11-08、本人）: 結果が分からない状態を linear_outbox.outcome_unknown に残す。送る前に 1 にしてコミットし、結果が分かったら 0 に戻す。1 の行は再起動の後も Linear のコメントを読み、同じ dedupe_key が無いことを確かめてから送る | 16 |
| Q84 | Linear コメントの送信が API キーの認証エラーで失敗したとき、macOS 通知を出すか（16 の 6章は案で、PR #69 はログにだけ出していた） | 決定（2026-11-08、本人）: 出す。取り込み側の通知（12 の 3.5、Dashboard を開く）と同じ扱い・同じ抑止にする。取り込み側（15 の Linear Poller）と送信側（16 の送信 Worker）で認証エラーの通知の抑止を1つにまとめ、同じ API キーが無効になっても通知は1回だけ出す（2回出す案は採らない）。抑止は、取り込みの照会か送信のどちらかが1回成功したら解く | 12, 15, 16 |
| Q85 | notification_failed の reason に Linear 用の値を足すか（12 の 3.4 の分類は macOS 通知のヘルパー用で、PR #69 は Linear の失敗をすべて other にしている） | 決定（2026-11-08、本人）: channel=linear 用として auth（認証エラー）・issue_removed（Issue が削除・アーカイブされていた）・retries_exhausted（5回目も失敗した）を足し、02 の 5.3 の列挙に載せる。どれにも当てはまらない失敗は other にする | 02, 12, 16 |
| Q86 | 取り込み側と送信側で1つにまとめた認証エラーの通知の抑止（Q84）と、取り込み側の「5回続けて失敗した」通知（15 の 6章、Team ごとの抑止）をどう分けるか。API キーのファイルが無い・読めない場合も共通の抑止に含めるか | 決定（2026-11-08、本人）: 認証エラーの通知は共通の抑止だけで扱う。5回続けて失敗した通知は、5回目の失敗の分類が auth 以外のときだけ Team ごとの抑止で出す。API キーのファイルが無い・読めない・権限が 0600 でない場合も認証エラーとして共通の抑止に含める | 12, 15, 16 |
| Q87 | （Phase 1）setup の許可宛先（03 の 3.3.1）で拒否するローカルの名前の範囲、同じ宛先の重複の扱い、管理画面の入力欄 | 決定（2026-11-08、本人）: 拒否するローカルの名前は、ドットを含まない1語の名前と、最後のラベルが localhost・local・internal の名前とする（PR #71 の実装どおり）。同じ宛先を2回含む入力は 400 で拒否する（PR #71 の実装は受け付けるため直す）。管理画面の Project の登録・編集に入力欄を作る | 03, 13 |
