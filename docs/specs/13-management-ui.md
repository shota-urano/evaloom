---
status: confirmed
範囲: Phase 1 の管理画面（Dashboard / Projects / Harnesses / Tasks / Settings）、ログイン、SSE によるログ表示、Blocked の確認画面
---

# 13. 管理画面 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Frontend ｜ **範囲**: React 管理画面のうち Phase 1 で作る画面

## 1. 目的

- 本人がプロジェクト・ハーネス・Profile を登録し、手動で開発タスクを作れるようにする（requirements 16.2〜16.4）。
- 実行状態とログを確認できるようにする（条件7）。
- Blocked のタスクについて、本人が変更内容を確認して続行・中止を選べるようにする（requirements 8.5）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | 本人の操作 |
| 出力 | 本人用 API の呼び出し（[`01-architecture.md`](./01-architecture.md) 5.1）、SSE の購読 |

## 3. 処理詳細

### 3.1 技術と共通の作り

| 項目 | 内容 | 出典 |
|---|---|---|
| フレームワーク | React + Vite + TypeScript | requirements 2.2 |
| UI | Tailwind CSS + shadcn/ui | requirements 2.2 |
| 配信 | `evaloomd` が静的ファイルを配信する（同一オリジン） | [`01-architecture.md`](./01-architecture.md) 3.1 |
| リアルタイム | SSE（`GET /api/executions/{id}/events`） | requirements 2.2 |
| 認証 | 一回限りのログイン URL からセッション Cookie を受け取る。状態を変える要求には `X-CSRF-Token` を付ける（Q2 案） | [`01-architecture.md`](./01-architecture.md) 3.4 |
| 開発サーバの中継 | `localhost:5173` / `127.0.0.1:5173` から `evaloomd` へ中継する。照合に通らない要求は転送せずに 403 で拒否し、`data/security/` に記録する | [`01-architecture.md`](./01-architecture.md) 3.4「開発時の例外」 |
| 入力検証 | 画面でも検証するが、正本はサーバー側。サーバーの `details` をフォームの該当項目に表示する | [`01-architecture.md`](./01-architecture.md) 5.3 |
| 二重送信 | タスク作成では送信ごとに `Idempotency-Key` を生成する | [`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.1 |
| 状態の表示名 | [`02-data-model.md`](./02-data-model.md) 5.1 の表示名を使う | - |

データ取得のライブラリ、ルーティングのライブラリは実装時に確定する。

### 3.2 画面一覧（Phase 1）

| 画面 | 主な表示・操作 | 使う API | 出典 |
|---|---|---|---|
| Dashboard | Runner 状態、実行中タスク、待機中タスク、Blocked のタスク、エラー状況（失敗・通知失敗） | `GET /api/runners`, `GET /api/tasks` | requirements 16.1 |
| Projects 一覧 | 登録済みプロジェクト、割り当て中の Harness と版 | `GET /api/projects` | 16.2 |
| Project 登録・編集 | Project ID、名称、パス、Git URL、Runner、Base Branch、テストコマンド、レポート形式と出力パス、セットアップコマンド、コピー許可リスト、テストファイルの追加パターン、タイムアウト、許可操作、承認ポリシー | `POST /api/projects`, `PATCH /api/projects/{id}` | 16.2 |
| Project 詳細 | 上記の表示、役割ごとの Harness Profile 割り当て、Project Context（固定対象の `AGENTS.md` の説明のみ） | `GET /api/projects/{id}`, `POST /api/projects/{id}/harnesses` | 16.2 |
| Harnesses 一覧 | Harness、Adapter、使用中プロジェクト | `GET /api/harnesses`, `GET /api/projects` | 16.3 |
| Harness 登録・編集 | ID、名称、専門分野、Adapter、リポジトリ、対応モデル、利用可能ツール、Tool Permissions | `POST /api/harnesses`, `PATCH /api/harnesses/{id}` | 16.3 |
| Harness 詳細 | Profile の版の一覧（版、commit SHA、Model ID、状態=暫定運用）、版の登録、使用中プロジェクト | `GET /api/harness-profiles`, `POST /api/harness-profiles` | 16.3 |
| Tasks 一覧 | タスク、状態、プロジェクト、最新 Execution、PR へのリンク。状態とプロジェクトで絞り込み | `GET /api/tasks` | 16.4 |
| タスク作成 | プロジェクト、件名、本文、受け入れ条件（必須）、短い名前 | `POST /api/tasks` | 16.4, requirements 8.1 |
| タスク詳細 | 3.3 | 3.3 | 16.4 |
| Settings | 全体の既定タイムアウト、同時実行の上限（表示のみ、変更不可）、通知先、Runner 一覧、Global Context（表示のみ） | `GET/PATCH /api/settings`, `GET /api/runners` | 16.8 |

Phase 1 で作らない画面: Evaluations（16.5）、Improvements（16.6）、Collaborations（16.7）、Approvals（16.9）。

### 3.3 タスク詳細

| 区画 | 内容 |
|---|---|
| 概要 | 件名、本文、受け入れ条件、状態、プロジェクト、PR へのリンク |
| Execution 履歴 | 1タスクに複数。seq、mode、状態、Profile の版と commit SHA、開始・終了時刻、エラー分類 |
| ログ | 選んだ Execution のログを SSE で表示する。実行中は追記し、終了後は保存済みログを表示する。stream（harness / setup / test / runner）で切り替える |
| 固定した Context | context_revisions の一覧（階層、パス、ハッシュ） |
| 検証結果 | Gate 1〜3 の結果、Finding、Gate 2 の実行数・skip 数と確認方法（ID による照合か件数による確認か） |
| 成果物 | 差分、テストレポートなど |
| 操作 | 停止（中止）、再実行（継続 / 最初から）。最初からを選ぶときは確認ダイアログを出す |

### 3.4 Blocked の確認と続行

requirements 8.5「止まったタスクは、本人が変更内容を確認し、続行を選んだ場合だけ先へ進む」。

1. Block 原因と Finding（ファイル、行、理由）を表示する。
2. Gate 1 の Block では、該当ファイルの差分を表示する。
3. 本人が「続行」か「中止」を選ぶ。送信時に、画面に表示していた HEAD SHA を `headSha` として送る（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.8）。
4. SHA 不一致（422）が返ったら、内容が変わったことを表示して画面を読み直させる。
5. `context_conflict` と `git_state` の Block では続行ボタンを出さず、中止と再実行だけを出す。

### 3.5 状態の表示

- 状態は色だけでなく文字でも示す。
- 実行中のタスクは SSE の `status_changed` で表示を更新する。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 接続元 | Mac mini 本体のブラウザ（127.0.0.1）のみ | requirements 15.5 |
| 受け入れ条件 | タスク作成の必須項目 | requirements 8.1 |
| 同時実行の上限 | 全体2本・同一プロジェクト1本。画面からは変更させない | requirements 8.7 |
| Profile の状態表示 | Phase 1 はすべて「暫定運用」 | requirements 11.1 |
| 権限設定の変更 | 本人だけが管理画面から行える | requirements 15.3 |

## 5. インターフェース

本人用 API の一覧は [`01-architecture.md`](./01-architecture.md) 5.1、エラー形式は 5.3、SSE は 5.4。データの形は [`02-data-model.md`](./02-data-model.md)。

## 6. エラー処理

| 応答 | 表示 |
|---|---|
| 400 `validation_failed` | `details` の項目ごとにフォームへ表示 |
| 401 | ログインが必要であることを表示する |
| 403 | 操作できないことを表示する（CSRF 失敗を含む） |
| 409 | 状態が変わったことを表示し、最新を読み直す |
| 422 | 前提条件の不成立（パス検証の失敗理由など）を表示する |
| SSE の切断 | 自動で再接続し、Last-Event-ID から再開する |

## 7. スコープ外

- Evaluations / Improvements / Collaborations / Approvals 画面
- Linear Label 連携の設定（Phase 2）
- Project Context の編集（Phase 1 は `AGENTS.md` が正本）
- Model Registry 画面（Q19）
- スマートフォン・別端末向けの表示

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API と認証
- [`02-data-model.md`](./02-data-model.md) 状態とデータの形
- [`03-registry.md`](./03-registry.md) 登録項目と検証
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) タスク作成・中止・再実行・続行
- [`05-runner.md`](./05-runner.md) ログとイベント
- [`10-verification-gates.md`](./10-verification-gates.md) 検証結果
- [`12-notification.md`](./12-notification.md) 通知

## 9. 実装単位

- [ ] [Frontend] プロジェクトの雛形（React + Vite + TypeScript、Tailwind CSS、shadcn/ui）、API クライアント、ログイン、CSRF トークンの付与、エラー表示の共通部品
  - 受け入れ基準: ビルド成果物を evaloomd が配信して表示できる; 状態を変える要求に X-CSRF-Token が付く; 400 の details がフォームの項目に表示される
- [ ] [Frontend] Projects 一覧・登録・編集・詳細（Profile 割り当てを含む）
  - 受け入れ基準: 2つのプロジェクトを画面から登録できる; パス検証の失敗理由が表示される; 役割に Harness と版を割り当てられる
- [ ] [Frontend] Harnesses 一覧・登録・編集・詳細（Profile の版の登録と一覧）
  - 受け入れ基準: Profile の版を登録すると commit SHA と「暫定運用」が表示される; 同じ版の別 SHA 登録で 409 の内容が表示される
- [ ] [Frontend] Tasks 一覧とタスク作成
  - 受け入れ基準: 受け入れ条件が空だと送信できない; 二重クリックでタスクが2件にならない; 状態とプロジェクトで絞り込める
- [ ] [Frontend] タスク詳細（Execution 履歴、SSE ログ、固定した Context、検証結果、成果物、中止・再実行）
  - 受け入れ基準: 実行中のログが逐次表示される; 再接続で欠けなく続きが表示される; 再実行で新しい Execution が履歴に増える
- [ ] [Frontend] Blocked の確認と続行・中止
  - 受け入れ基準: Finding と差分が表示される; 続行の送信に表示中の headSha が含まれる; context_conflict では続行ボタンが出ない; 422 で読み直しを促す
- [ ] [Frontend] Dashboard と Settings
  - 受け入れ基準: Runner の online / offline、実行中・待機中・Blocked の件数が表示される; 同時実行の上限が表示されるが変更できない
