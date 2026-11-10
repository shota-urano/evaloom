---
status: draft
written: 2026-11-10
範囲: 管理画面（ログイン、Dashboard、本人の判断待ち、Projects、振り分けの表、スペシャルハーネスと版、記録の一覧、Tasks、Settings）、SSE によるログ表示
---

# 13. 管理画面 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Frontend ｜ **範囲**: React の管理画面。本人の判断はすべてこの画面で受け付ける

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。Project の登録は6項目だけにし、開発の項目（パス、Git URL、ベースブランチ、テストコマンド、setup、コピー許可リスト）は software-engineer へ移した（本人決定 1・6）。API の名前は [`01-architecture.md`](./01-architecture.md)、状態の値は [`02-data-model.md`](./02-data-model.md)、スペシャルハーネスとの約束はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）に従う。

## 1. 目的

- 本人が Project・振り分けの表・スペシャルハーネスと版を登録し、手動でタスクを作れるようにする（requirements 16.2〜16.4）。
- 本人の判断（振り分けの承認、操作の承認、止まった仕事の続行、作業中の AI の質問への回答）を、この画面の1か所で受け付ける（本人決定 3、vision.md 3章）。
- 実行状態、段ごとの結果、ログ、外に出したものを確認できるようにする（条件7）。
- スペシャルハーネスの記録を確認し、次の仕事に渡したくない記録を消せるようにする（契約 9章）。

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
| 認証 | ログイン画面で ID とパスワードを送り、セッション Cookie を受け取る。状態を変える要求には `X-CSRF-Token` を付ける。401 を受けたらログイン画面へ移り、ログインし直したら元の画面へ戻る | [`18-remote-access.md`](./18-remote-access.md) 3.4〜3.6、[`01-architecture.md`](./01-architecture.md) 3.6 |
| 開発サーバの中継 | `localhost:5173` / `127.0.0.1:5173` から `evaloomd` へ中継する。照合に通らない要求は転送せずに 403 で拒否し、`data/security/` に記録する | [`01-architecture.md`](./01-architecture.md) 3.6「開発時の例外」 |
| 入力検証 | 画面でも検証するが、正本はサーバー側。サーバーの `details` をフォームの該当項目に表示する | [`01-architecture.md`](./01-architecture.md) 5.3 |
| 二重送信 | タスク作成では送信ごとに `Idempotency-Key` を生成する | [`04-task-lifecycle.md`](./04-task-lifecycle.md) |
| 状態の表示名 | [`02-data-model.md`](./02-data-model.md) 5.1 の表示名を使う | - |
| 不透明な値 | `params`、`contentId`、`outputs`、`target` は、サーバーが返した値をそのまま表示する。画面は中身を解釈しない | [`02-data-model.md`](./02-data-model.md) 3.1 |

データ取得のライブラリ、ルーティングのライブラリは実装時に確定する。

### 3.2 画面一覧

| 画面 | 主な表示・操作 | 使う API | 出典 |
|---|---|---|---|
| ログイン | ID とパスワードの入力。パスワードが未設定のときは設定の手順を出す。ログアウトの操作 | `POST /auth/login`, `POST /auth/logout`, `GET /api/session` | 18 の 3.4〜3.6 |
| Dashboard | Runner の状態、実行中・待機中のタスク、本人の判断待ちの件数（種類ごと）、エラー状況（失敗、通知失敗、Linear の取り込みの状況） | `GET /api/runners`, `GET /api/tasks`, `GET /api/decisions`, `GET /api/notification-failures`, `GET /api/linear/status` | requirements 16.1 |
| 本人の判断待ち | 3.4。判断待ちの一覧と詳細、回答 | `GET /api/decisions`, `GET /api/decisions/{id}`, `POST /api/decisions/{id}/answer` | 本人決定 3 |
| Projects 一覧 | Project の名前、振り分けのラベル、スペシャルハーネスと版、Runner | `GET /api/projects` | 16.2 |
| Project 登録・編集 | 3.5 の6項目 | `POST /api/projects`, `PATCH /api/projects/{id}`, `POST /api/routes`, `PATCH /api/routes/{label}` | 16.2、本人決定 6 |
| Project 詳細 | 6項目の表示、その Project のタスクの一覧へのリンク | `GET /api/projects/{id}` | 16.2 |
| 振り分けの表 | 3.6。ラベルと振り分け先の一覧、登録・変更・削除 | `GET /api/routes`, `POST /api/routes`, `PATCH /api/routes/{label}`, `DELETE /api/routes/{label}` | 本人決定 2 |
| スペシャルハーネス一覧 | スペシャルハーネスの ID と名前、既定の版、使っている Project | `GET /api/harnesses`, `GET /api/projects` | 16.3 |
| スペシャルハーネス登録・編集 | ID、名前、リポジトリの URL、ローカルの複製のパス、既定の版（直接振り分けたタスクで使う版） | `POST /api/harnesses`, `PATCH /api/harnesses/{id}` | 16.3 |
| スペシャルハーネス詳細 | 3.7。版の一覧と登録、版ごとの宣言 | `GET /api/harness-profiles`, `GET /api/harness-profiles/{id}`, `POST /api/harness-profiles` | 16.3 |
| 記録の一覧 | 3.8。スペシャルハーネスごとの記録と削除 | `GET /api/harnesses/{id}/records`, `DELETE /api/harness-records/{id}` | 契約 9章 |
| Tasks 一覧 | タスク、状態、振り分け先、最新の Execution、外に出したもの（`outputs`）へのリンク、Linear の識別子。状態と振り分け先で絞り込み | `GET /api/tasks` | 16.4 |
| タスク作成 | 振り分け先（Project かスペシャルハーネスの一方）、件名、本文、受け入れ条件 | `POST /api/tasks` | 16.4、requirements 8.1 |
| タスク詳細 | 3.3 | 3.3 | 16.4 |
| Settings | 全体設定（[`02-data-model.md`](./02-data-model.md) 3.8 のキー）、同時実行の上限（表示のみ）、Runner 一覧、Global Context（表示のみ） | `GET/PATCH /api/settings`, `GET /api/runners` | 16.8 |

作らない画面: Evaluations（16.5）、Improvements（16.6）、Collaborations（16.7）。承認は「本人の判断待ち」の画面で受け付け、別の Approvals 画面は作らない。承認の履歴は判断待ちの画面の判断済みの一覧とタスク詳細で見る（requirements 16.9 をこの形で満たす）。

Dashboard の通知失敗の一覧には、Execution の無い契機の Linear への送信の失敗（linear_outbox の行）も出る（2026-11-08 決定、Q82。[`12-notification.md`](./12-notification.md) 5章、[`16-linear-feedback.md`](./16-linear-feedback.md) 3.4.2）。この項目は `eventId` と `executionId` が null で `outboxId` が入るため、画面は `outboxId` で項目を見分け、Execution の ID を表示しない。`reason` は channel=`linear` 用の値（16 の 3.4.2 の表、Q85）の説明を出す。

### 3.3 タスク詳細

| 区画 | 内容 |
|---|---|
| 概要 | 件名、本文、受け入れ条件、状態、振り分け先（Project かスペシャルハーネス）、Linear の Issue へのリンク、外に出したもの（最新の Execution の `outputs` の `label` と `url`）、開いている判断待ちへのリンク |
| Execution 履歴 | 1タスクに複数。seq、mode（`continue` / `fresh`）、状態、再開した段（resume_stage）、スペシャルハーネスの版と commit、契約の版、渡した値、開始・終了時刻、エラー分類 |
| 段の結果 | 選んだ Execution の stage_runs（段、attempt、状態、エラー分類、開始・終了時刻）。結果 JSON の `message` と `summary` |
| ログ | 選んだ Execution のログを SSE で表示する。実行中は追記し、終了後は保存済みログを表示する。stream（`prepare` / `work` / `check` / `publish` / `runner`）で切り替える |
| 確かめた結果 | check_results の result、中身の識別子（content_id）、summary、finding（rule、severity、message、location） |
| 承認 | このタスクの approvals（操作、対象の表示、content_id、状態、無効の理由、判断・使用の時刻） |
| 固定した文脈 | context_revisions の一覧（階層、パス、版、ハッシュ）。`reported`（スペシャルハーネスが報告した文脈）は記録だけを表示する |
| 成果物 | artifacts の一覧（段の `out` のファイル、作業の概要 `out/summary.md`、質問 `out/question.json`、要求 JSON、結果 JSON、ログ） |
| 操作 | 中止、再実行（`continue` / `fresh`）。`fresh` を選ぶときは確認ダイアログを出す |

### 3.4 本人の判断待ち

本人の判断は、すべてこの画面で受け付ける（本人決定 3）。Linear には「管理画面で判断待ち」とリンクだけを送り（[`16-linear-feedback.md`](./16-linear-feedback.md) 3.2）、Linear での操作を判断として扱わない。判断待ちの詳細の URL は `/decisions/<id>` とし、Linear のコメント（[`01-architecture.md`](./01-architecture.md) Q122）と macOS 通知（[`12-notification.md`](./12-notification.md) 3.1・3.6、Q149。通知からは `http://127.0.0.1:<port>/decisions/<id>` を開く）から開けるようにする。

一覧は owner_decisions のうち status=`open` の行を新しい順に出し、種類（`route` / `approval` / `blocked` / `question`）で絞り込める。判断済み（`answered` / `withdrawn`）の一覧も切り替えて見られる。

詳細の画面は、種類ごとに次を出す。回答の形は [`02-data-model.md`](./02-data-model.md) 5.6 に従う。

| kind | 表示 | 操作 |
|---|---|---|
| `route` | タスクの件名・本文・Issue へのリンク、判定したときの振り分けのラベル（seen_labels）、提案の理由と提案したラベル（提案が `failed` のときは失敗したこと）、振り分け先の候補（現在の振り分けの表のラベル、振り分け先、Profile の description） | 承認（提案したラベル、または候補から選んだラベル、Q112）、却下。承認の送信に、画面に出していた提案の ID を `basis` として送る |
| `approval` | 操作の名前と Profile の description、対象の表示（`target` の `label` と `url`）、`summary`、中身の識別子（content_id）、承認を求めた段、確かめた結果（check_results の summary と finding）、成果物へのリンク、承認の有効時間 | 承認、却下。送信に、画面に出していた content_id を `contentId` として送る。承認のときは有効時間を変えられる（既定は全体設定 `approval.ttlSec`、送信の `ttlSec`、02 の Q200） |
| `blocked`（`check_blocked`） | 確かめた結果の summary と finding、中身の識別子 | 続行、中止。送信に、画面に出していた content_id を `contentId` として送る |
| `blocked`（`publish_unknown`） | 出す段の `message`、わかっている `outputs` | 続行、中止。続行すると出す段を新しく始めることを示す（契約 7.5） |
| `blocked`（`context_conflict` / `workspace_missing`） | 原因の説明 | 中止だけ。続行のボタンは出さない。再実行（`context_conflict` は直してから、`workspace_missing` は `fresh`）への案内を出す |
| `question` | 作業中の AI の質問（`question`）と選択肢（`options`） | 回答（自由記述と、選択肢があれば1つ選ぶ）、中止 |

- 同じ Execution で承認待ちが複数あるときは、1つの画面に並べて出す（Q155）。
- 回答の後に 422（basis の不一致）が返ったら、内容が変わったことを表示して画面を読み直させる。409（判断済み・取り下げ済み）が返ったら、最新の状態を読み直して表示する。
- 判断待ちが取り下げられた（`withdrawn`）ときは、理由（タスクの中止、Linear で本人が振り分けのラベルを付けた、新しい判断待ちに置き換えた、期限切れ）を文字で出す。
- 判断待ちの画面は、スマートフォンの幅でも読めて操作できるようにする（Q156）。

### 3.5 Project の登録・編集

Project が持つのは次の6項目だけである（vision.md 4章、本人決定 6、[`02-data-model.md`](./02-data-model.md) 3.2 projects）。

| 項目 | 入力 | 送る先 |
|---|---|---|
| 名前 | 文字列（Project ID は登録時だけ入力する slug） | `name`（登録時は `id` も） |
| 振り分けのラベル | `linear.routingLabelPrefix` で始まる文字列。Project ごとに1つ | 振り分けの表（`POST /api/routes` / `PATCH /api/routes/{label}`、`projectId` を指定） |
| 頼むスペシャルハーネス | スペシャルハーネスと、固定する版を選ぶ | `harnessId`、`profileId` |
| 渡す値 | JSON の入力欄。選んだ版が `params.schema` を持つときは、スキーマの説明を横に出す | `params` |
| 承認を省く設定 | 選んだ版の Profile が `waivable: true` と宣言した操作だけを、チェックボックスで並べる | `approvalWaivers` |
| 動かすマシン | Runner を選ぶ | `runnerId` |

- 振り分けのラベルは振り分けの表の行として登録する。画面は Project の登録・更新に成功した後にラベルを登録し、ラベルの登録だけが失敗したときは、ラベルが無いことと失敗の理由を表示して入力を残す。
- 渡す値が Profile のスキーマに合わないときは 422 の `details` を入力欄に表示する（契約 5.2、Q115）。
- 承認を省く操作を足すときは確認ダイアログを出す（[`17-approval.md`](./17-approval.md) 3.2、Q165）。
- 版を変えたときに、新しい版で `waivable: true` でない操作が承認を省く設定に残っていれば、サーバーが 422 を返す。画面はその操作を示して外すよう促す。
- 鍵は管理画面から登録しない（[`01-architecture.md`](./01-architecture.md) 3.5）。スペシャルハーネス詳細では、版が宣言した鍵の名前（slot）と使い方だけを表示する。

### 3.6 振り分けの表

ラベルから振り分け先を引く表を、本人がこの画面で管理する（本人決定 2、[`02-data-model.md`](./02-data-model.md) 3.2 routes）。

- 一覧はラベル、振り分け先の種類（Project / スペシャルハーネス）、振り分け先の名前を出す。
- 登録・変更では、振り分け先に Project かスペシャルハーネスのどちらか一方を選ぶ。スペシャルハーネスへ直接振り分けるときは、そのスペシャルハーネスに既定の版が無いと実行されないことを示す（[`15-linear-intake.md`](./15-linear-intake.md) 3.6）。
- ラベルの重複、同じ振り分け先への2つ目のラベルは 409 の内容を表示する。
- 振り分けの表を変えると、振り分けを待っているタスクを次の照会で判定し直すことを示す（[`15-linear-intake.md`](./15-linear-intake.md) 3.6.3）。

### 3.7 スペシャルハーネス詳細

- 版の一覧: 版、commit SHA、契約の版、運用の状態（評価基盤ができるまで「暫定運用」、requirements 11.1）、登録時刻。
- 版の詳細: Profile 定義（契約 5章）の宣言を読みやすく並べる。description、受け入れ条件の要否、段ごとの宣言（上限時間、外への接続の許可宛先、鍵の名前と使い方、作業の段のミクロハーネスとモデル）、承認が要る操作と `waivable`、記録として残すもの。定義の全文も表示できる。
- 版の登録: 版（SemVer）を入れて送る。サーバーがタグを commit SHA に解決して検証する。同じ版の別の commit の登録は 409、契約に合わない定義は 400 / 422 の内容を表示する。
- 使っている Project と、振り分けの表でこのスペシャルハーネスに直接振り分けるラベル。

### 3.8 記録の一覧

- スペシャルハーネスごとに、harness_records を新しい順に出す。記録した日付、タスク（出所の識別子とタスク詳細へのリンク）、ファイル名、成果物の内容へのリンク。
- 本人は記録を消せる。消す前に確認ダイアログを出し、消した記録は次の仕事に渡らないことを示す（契約 9章）。消した記録は一覧で「削除済み」として区別して出す。

### 3.9 状態の表示

- 状態は色だけでなく文字でも示す。
- 実行中のタスクは SSE の `status_changed` で表示を更新する。
- 判断待ちの件数は Dashboard と画面の上部に出し、判断待ちの画面へのリンクにする。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 接続元 | Mac mini 本体のブラウザ（127.0.0.1）と、tailnet の中の本人の端末（`tailscale serve` の中継） | [`18-remote-access.md`](./18-remote-access.md) 3.1 |
| 本人の判断の場所 | この画面だけ。Linear での操作を判断として扱わない | 本人決定 3 |
| Project の登録項目 | 名前、振り分けのラベル、頼むスペシャルハーネス（と版）、渡す値、承認を省く設定、Runner の6つ | vision.md 4章、本人決定 6 |
| 受け入れ条件 | 振り分け先の Profile が `acceptanceCriteria: required` ならタスク作成の必須項目 | 契約 5.2 |
| 同時実行の上限 | 全体2本、振り分け先ごとに1本。画面からは変更させない | requirements 8.7、[`02-data-model.md`](./02-data-model.md) 4章 |
| Profile の状態表示 | 評価基盤ができるまで、すべて「暫定運用」 | requirements 11.1 |
| 権限設定の変更 | 本人だけが管理画面から行える。重要な操作でもパスワードを入れ直させない | requirements 15.3、18 の 3.7 |
| 鍵 | 画面では登録も表示もしない。名前（slot）と使い方だけを表示する | [`01-architecture.md`](./01-architecture.md) 3.5 |

## 5. インターフェース

本人用 API の一覧は [`01-architecture.md`](./01-architecture.md) 5.1、エラー形式は 5.3、SSE は 5.4。データの形は [`02-data-model.md`](./02-data-model.md)。判断待ちの回答の形は [`02-data-model.md`](./02-data-model.md) 5.6。

## 6. エラー処理

| 応答 | 表示 |
|---|---|
| 400 `validation_failed` | `details` の項目ごとにフォームへ表示 |
| 401 | ログイン画面へ移る。ログインし直したら元の画面へ戻る |
| 403 | 操作できないことを表示する（CSRF 失敗を含む） |
| 409 | 状態が変わったこと（判断済み、ラベルの重複など）を表示し、最新を読み直す |
| 422 | 前提条件の不成立を表示する（basis の不一致、渡す値がスキーマに合わない、省けない承認を省く設定に入れた、など） |
| SSE の切断 | 自動で再接続し、Last-Event-ID から再開する |

## 7. スコープ外

- Evaluations / Improvements / Collaborations 画面（requirements 10〜13章の将来の Phase）
- Model Registry 画面（Q19）
- 鍵の登録・入れ替え（Mac mini の CLI だけで行う、[`01-architecture.md`](./01-architecture.md) 3.5）
- 本人のパスワードの設定・変更（CLI だけで行う、[`18-remote-access.md`](./18-remote-access.md) Q89）
- スペシャルハーネスの中身（指示、スキル、確かめる基準）の編集。スペシャルハーネスのリポジトリで行い、版として登録する
- 開発の項目（パス、Git URL、ベースブランチ、テストコマンド、setup、コピー許可リスト）の入力。software-engineer へ移した（本人決定 1）

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API と認証
- [`02-data-model.md`](./02-data-model.md) 状態とデータの形、判断待ちの回答の形
- [`03-registry.md`](./03-registry.md) 登録項目と検証
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) タスク作成・中止・再実行・本人の判断の後の再開
- [`05-runner.md`](./05-runner.md) ログとイベント
- [`12-notification.md`](./12-notification.md) 通知
- [`15-linear-intake.md`](./15-linear-intake.md) 振り分けと振り分けの提案
- [`16-linear-feedback.md`](./16-linear-feedback.md) Linear への返却
- [`17-approval.md`](./17-approval.md) 承認
- [`18-remote-access.md`](./18-remote-access.md) ログイン

## 9. 実装単位

- [ ] [Frontend] プロジェクトの雛形（React + Vite + TypeScript、Tailwind CSS、shadcn/ui）、API クライアント、CSRF トークンの付与、エラー表示の共通部品
  - 受け入れ基準: ビルド成果物を evaloomd が配信して表示できる; 状態を変える要求に X-CSRF-Token が付く; 400 の details がフォームの項目に表示される
- [ ] [Frontend] ログイン画面とログアウト、401 のときのログイン画面への移動（18 の 3.4〜3.6）
  - 受け入れ基準: ID とパスワードでログインでき Dashboard が出る; 間違えると「ID かパスワードが違う」ことが出る; セッションが切れたら操作の途中でもログイン画面へ移り、ログインし直すと元の画面へ戻る; パスワードが未設定のとき設定の手順が出る; ログアウトでログイン画面へ移る
- [ ] [Frontend] 本人の判断待ちの一覧と詳細（4種類、3.4）
  - 受け入れ基準: open の判断待ちが種類で絞り込める; route で提案のラベルと理由、候補が出て、候補から別のラベルを選んで承認できる; approval で操作・対象・content_id・確かめた結果が出て、送信に表示中の contentId が含まれる; check_blocked の続行の送信に表示中の contentId が含まれる; context_conflict と workspace_missing では続行ボタンが出ない; question で質問と選択肢が出て、回答を送れる; 422 で読み直しを促し、409 で最新の状態を出す; withdrawn の理由が文字で出る; `/decisions/<id>` を直接開ける; 幅 375px の画面で判断待ちの詳細を読めて回答できる
- [ ] [Frontend] Projects 一覧・登録・編集・詳細（6項目、3.5）
  - 受け入れ基準: 6項目以外の入力欄が無い; 2つの Project を画面から登録でき、それぞれに振り分けのラベルが付く; 承認を省く設定に、選んだ版で waivable の操作だけが並ぶ; 承認を省く操作を足すと確認ダイアログが出て、確認しないと保存されない; 渡す値がスキーマに合わないと 422 の内容が入力欄に出る; ラベルの登録だけが失敗したとき、Project は登録されラベルが無いことが表示される
- [ ] [Frontend] 振り分けの表の画面（3.6）
  - 受け入れ基準: Project とスペシャルハーネスのどちらかを振り分け先にしたラベルを登録・変更・削除できる; 両方または両方なしでは送信できない; ラベルの重複と同じ振り分け先への2つ目のラベルで 409 の内容が出る; 接頭辞で始まらないラベルで 400 の内容が出る
- [ ] [Frontend] スペシャルハーネスの一覧・登録・編集・詳細（版の登録と宣言の表示、3.7）
  - 受け入れ基準: 版を登録すると commit SHA と「暫定運用」が表示される; 同じ版の別 SHA の登録で 409 の内容が表示される; 版の詳細に段ごとの上限時間・許可宛先・鍵の名前、承認が要る操作と waivable が出る; 鍵の値を表示する欄が無い; 既定の版を設定できる
- [ ] [Frontend] 記録の一覧と削除（3.8）
  - 受け入れ基準: スペシャルハーネスごとに記録が新しい順に出る; 確認ダイアログを経て消すと「削除済み」になる
- [ ] [Frontend] Tasks 一覧とタスク作成
  - 受け入れ基準: 振り分け先の Profile が required のとき受け入れ条件が空だと送信できない; 二重クリックでタスクが2件にならない; 状態と振り分け先で絞り込める; 一覧に outputs のリンクと Linear の識別子が出る
- [ ] [Frontend] タスク詳細（Execution 履歴、段の結果、SSE ログ、確かめた結果、承認、固定した文脈、成果物、中止・再実行）
  - 受け入れ基準: 実行中のログが逐次表示される; 再接続で欠けなく続きが表示される; ログを prepare・work・check・publish・runner で切り替えられる; 再実行で新しい Execution が履歴に増える; outputs の label と url が出る; 開いている判断待ちへのリンクが出る
- [ ] [Frontend] Dashboard と Settings
  - 受け入れ基準: Runner の online / offline、実行中・待機中のタスクの件数、判断待ちの件数（種類ごと）が表示される; Linear の照会が続けて失敗しているとき、失敗の分類と最後に成功した時刻が出る; 同時実行の上限が表示されるが変更できない; Settings で 02 の 3.8 のキーを表示・変更でき、未知のキーは送れない
- [ ] [Frontend] 通知失敗の一覧に linear_outbox の行（executionId が null、outboxId あり、channel=linear の reason）を表示する
  - 受け入れ基準: eventId と executionId が null で outboxId が入った項目が、タスクの件名（タスク詳細へのリンク）、kind、channel=linear、reason、記録した時刻（createdAt）とともに Dashboard の通知失敗の一覧に表示される; executionId が null の項目で、Execution の ID の欄に null や undefined の文字が出ない; eventId が null の項目が複数あり、execution_events の項目と混ざっても、どの項目も欠けず重複せずに表示される; reason の値に 16 の 3.4.2 の表に沿った説明が出る; 項目が応答の順（記録した時刻の新しい順、12 の 5章）のまま表示され、「さらに表示」で続きを読んでも欠けも重複も無い; コメントの本文とエラーの文字列を表示しない

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q155 | 同じ Execution で承認待ちが複数あるとき（操作ごとに owner_decisions が1件ずつ）、画面でまとめて判断させるか | 1つの画面に並べ、操作ごとに承認・却下を選ばせる。「すべて承認」のボタンは作らない（操作ごとに対象と content_id を見てから判断するため）。1つでも却下すると Execution が失敗になることを画面に示す |
| Q156 | スマートフォン向けの表示をどこまで作るか（18 で別端末から使えるようになったため、旧 7章の「スマートフォン・別端末向けの表示はスコープ外」を外した） | 本人の判断待ちの一覧と詳細だけを、幅 375px でも読めて操作できるようにする。登録や設定の画面は横幅の広い画面を前提にしたままにする |
