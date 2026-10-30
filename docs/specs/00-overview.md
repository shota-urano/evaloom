# Evaloom システム全体仕様書（Overview） — Phase 1

**一次情報源**: [`../requirements.md`](../requirements.md)（基本仕様書 v1.2）
**対象範囲**: 19章「Phase 1：基本実行基盤」と 20章「Phase 1 受け入れ条件 1〜18」、および全Phase共通の条件31
**本書の役割**: Phase 1 詳細仕様書群の索引。各詳細仕様書のファイル名はこの文書の「3. 詳細仕様書一覧」から辿る。

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
│  ├ 検証 Gate 1〜3                      → 10-verification-gates.md │
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
| 11 | テスト・検証 | Gate 1〜3 | 10 |
| 12 | push・PR 作成 | Git Publisher | 11 |
| 13 | 評価 | 行わない（Phase 3） | - |
| 14 | 承認 | 行わない（Phase 2）。PR マージは本人が GitHub で行う | - |
| 15 | 結果返却 | 管理画面表示 + macOS 通知 | 12, 13 |
| 16 | 履歴保存 | 実施 | 02, 05 |

## 3. 詳細仕様書一覧（索引）

この表が Phase 1 で有効な仕様の唯一の一覧である。下流（spec-to-beads）はこの表に載る仕様だけを使う。

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
| 10 | [`10-verification-gates.md`](./10-verification-gates.md) | テストの弱体化の検出（Gate 1）、テスト実行と既存テストの照合（Gate 2）、SHA と作業ツリーの検証（Gate 3） | Backend |
| 11 | [`11-git-publisher.md`](./11-git-publisher.md) | 検証済み commit の push と PR 作成、ブランチ命名、再実行時の PR の扱い | Backend |
| 12 | [`12-notification.md`](./12-notification.md) | macOS 通知（完了・失敗・Blocked・タイムアウト） | Backend |
| 13 | [`13-management-ui.md`](./13-management-ui.md) | Phase 1 の管理画面（Dashboard / Projects / Harnesses / Tasks / Settings） | Frontend |
| 14 | [`14-security-tests.md`](./14-security-tests.md) | 設定環境の分離テスト、権限境界テスト、隔離突破テスト、Gate の受け入れテスト | Backend |

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
| 17 | テストの削除・無効化・変更、skip/only 追加、実行設定・CI 定義の変更で push せず止まり、通知される | 10, 12, 14 |
| 18 | 既存テストが欠けて実行された場合に push せず止まる | 10, 14 |
| 31 | 新しい Adapter・モデルの追加で中央のコードは登録処理以外を変更しない | 08 |

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

- Linear Webhook 受信、Team + Label ルーティング、Linear へのコメント返却（Phase 2）
- 承認の仕組み（approvals、15.8 の承認と操作内容の結び付け）（Phase 2）
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

## 7. 要確認リスト（Phase 1 全体）

requirements で確定しておらず、本仕様書群で「実装時に確定」または「提案」とした事項。各詳細仕様の該当節にも同じ内容を書いている。

| # | 事項 | 選択肢・提案 | 記載仕様 |
|---|---|---|---|
| Q1 | ハーネスとテストの隔離方式（OS ユーザー分離 / macOS sandbox / コンテナ / VM） | 決定（2026-10-29、一部）: macOS 専用ツールチェーンが要らない Project はコンテナ。Xcode 等が要る Project は、実行ごとに使い捨てる macOS VM（Tart + Softnet）を第一案とし、VM 内での `xcodebuild test`・シミュレータ・起動時間を実測してから採否を決める。比較表は 09 の 3.4 | 09 |
| Q2 | 本人用 API の認証方式と CSRF 対策 | 起動時生成のセッショントークン + SameSite=Strict Cookie + CSRF トークン（提案） | 01 |
| Q3 | Runner 専用 API のエンドポイント（18章には claim だけがある） | heartbeat / events / result / cancel 確認を追加（提案） | 01 |
| Q4 | 18章に無い本人用 API（Project 更新、Harness・Profile 登録、Blocked からの続行 など） | 01 の API 一覧で「追加案」として明記 | 01 |
| Q5 | Harness Profile の定義ファイルの置き場所と読み込み方式（21章） | ハーネスリポジトリ内の定義ファイル + SemVer タグ（提案） | 02, 03 |
| Q6 | 共通契約の各項目の厳密な定義（9.5, 21章） | 02 に Phase 1 案を記載 | 02, 08 |
| Q7 | Claude Code・Codex の認証方式（サブスクリプション / API キー）と各社規約での自動実行の可否 | 決定（2026-10-29）: サブスクリプション。各社規約での自動実行の可否は本人が確認する | 08, 09 |
| Q8 | モデル API の認証情報をハーネスから見えないようにする方式（15.6） | 決定（2026-10-29）: Q7 でサブスクリプションを採ったため、認証付与プロキシは使わず、Evaloom 専用で用途を絞った認証情報を渡す。詳細は 09 の 3.5 | 09 |
| Q9 | ハーネスの専用設定環境の作り方（7.5） | 実行ごとの専用 HOME と各 CLI の設定ディレクトリ指定（提案）。実際に読み込んだ指示を 14 のテストで検証する | 08 |
| Q10 | テストの弱体化として検出する具体的な判定規則（8.5, 21章） | 10 にパターン案。プロジェクトごとに追加パターンを登録できるようにする | 10 |
| Q11 | 既存テストの ID 一覧・件数をどこから得るか | 開始点 SHA でテストを1回実行して基準値を取る案と、テストランナーの一覧出力を使う案。10 に比較 | 10 |
| Q12 | テストランナーの結果形式（ID・実行数・skip 数の取り出し方） | Project Registry にレポート形式（JUnit XML 等）と出力パスを登録（提案） | 03, 10 |
| Q13 | Gate 1 の基準 SHA：再実行で同じブランチに追加 commit するとき、前回本人が確認済みの差分を再び止めるか | 本人が続行を選んだ時点の SHA を記録し、それ以降の差分だけを検査する案 | 10 |
| Q14 | Blocked から続行を選ぶ操作を、どの SHA に結び付けるか | 続行の判断を、その時点の HEAD SHA に結び付け、SHA が変わったら無効（提案） | 04, 10 |
| Q15 | タイムアウト値（ハーネス実行・setup・テスト） | requirements に値なし。Project Registry と全体設定で持つ | 03, 05 |
| Q16 | 費用上限（15.2 必須対策）を Phase 1 で実装するか | 15.2 は必須対策に挙げるが Phase 1 の範囲が不明。トークン数・実行時間の記録（usage_records）は Phase 1 で行う案 | 02, 05 |
| Q17 | macOS 通知の実現方式（21章） | 決定（2026-10-27 実機確認）: 自作の通知用ヘルパーアプリ。クリックでタスク詳細を開く。詳細は 12 の 3.3 | 12 |
| Q18 | Linear コメントによる通知（8.8）を Phase 1 で行うか | Linear API キーの利用が Phase 2（6.6）のため、Phase 1 は macOS 通知だけとする案 | 12 |
| Q19 | Model Registry（5.3）を Phase 1 で作るか | Phase 1 ロードマップに無い。Profile に Model ID 文字列を持たせ、models テーブルは後の Phase とする案 | 02, 03 |
| Q20 | Global Context と Harness Context の Phase 1 での置き場所 | Global は evaloom リポジトリの `context/`、Harness はハーネスリポジトリ内（Profile の commit SHA で固定）とする案 | 07 |
| Q21 | Go バックエンドのモジュール境界（21章） | 01 に構成要素ごとのパッケージ案 | 01 |
| Q22 | 最初からやり直す場合のブランチ名（8.5, 21章） | `evaloom/<task-id>-<短い名前>-r<n>` 案 | 11 |
| Q23 | Blocked のタスクが同時実行の枠を占有するか | 占有しない案（プロセスは止まっているため）。worktree は保持する | 04 |
| Q24 | ログの機密情報除外（15.2）の判定規則と保存期間（21章） | 既知の秘密情報の値との一致で伏せる案。保存期間は未定 | 05 |
| Q25 | 権限設定（許可操作・承認ポリシー・Tool Permissions）の Phase 1 での中身 | 承認が Phase 2 のため、Phase 1 では「値の保存と表示」と、Runner が拒否する操作の固定リストまでとする案 | 03, 09 |
| Q26 | worktree のクリーンアップの契機（自動か本人の操作か） | PR がマージ・クローズ済みでタスクが終端状態なら削除してよい案。ブランチは残す | 06 |
| Q27 | worktree の元を本人のローカルリポジトリにするか、Evaloom の作業用クローンにするか | 決定（2026-10-29）: 作業用クローン（06 の 3.2.2 の A）。worktree は `.git` を共有するため、ハーネスが本人のリポジトリの ref や設定を書き換えられる | 06, 09 |
| Q28 | setup とテスト実行のネットワークポリシー（パッケージレジストリをどこまで許可するか） | 決定（2026-10-29）: setup はプロジェクトごとに登録した宛先だけ許可、テスト実行は全遮断 | 09 |
| Q29 | Git Publisher の GitHub 認証方式（fine-grained token / GitHub App） | 対象リポジトリに限った権限。マージの防止は GitHub 側のベースブランチ保護で行う | 11 |
| Q30 | 開始点で既存テストの基準値を取るための実行が、requirements 8.5 の「既存テストを別に固定して再実行する仕組みは作らない」に反しないか | 反しないと解釈した（Gate 2 の後の再実行ではないため）。本人の確認が要る | 10 |
| Q31 | ハーネスが commit を1つも作らなかった場合の扱い | failed（execution_failed、理由「変更なし」）の案 | 05 |
| Q32 | fresh で再実行するときに前の PR が開いていたら、Evaloom が自動でクローズするか | 自動ではクローズせず Blocked にして本人に整理を求める案 | 11 |
| Q33 | 実行中にハーネスが `AGENTS.md` 等の指示ファイルを変更した場合に Block するか | 知らせるだけで Block しない案 | 07, 10 |
| Q34 | ハーネスが Web で調べものをしたいときの接続方法（ハーネス用 Sandbox はモデル API にしかつながらない） | 1. モデル側の Web 検索機能に任せる（検索とページ取得はモデル提供元のサーバー側で行われ、Sandbox の接続先は増えない）を推奨。足りなければ 2. コードも認証情報も持たせず結果を文章で返すだけの調査専用 Sandbox を別に用意する。ハーネス用 Sandbox を Web に開放する案は採らない（プロンプトインジェクションとコード・認証情報の持ち出しのため）。ハーネスを Sandbox で実際に動かすタスクの前に決める | 08, 09 |
