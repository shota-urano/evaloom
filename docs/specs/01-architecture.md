---
status: draft
written: 2026-11-10
範囲: 整合性アンカー。構成要素・段ごとの Sandbox・鍵の保管・プロセス配置・権限境界・認証・API 一覧と共通規約
---

# 01. アーキテクチャ・インターフェース 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: 構成要素、段ごとの Sandbox、鍵の保管、プロセス配置、権限境界、認証、API 一覧と共通規約

この文書は整合性アンカーである。API のパス・認証区分・エラー形式はここだけで定義し、ほかの詳細仕様はここを参照する。テーブルと列挙値の名前は [`02-data-model.md`](./02-data-model.md)、スペシャルハーネスとの約束はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）に従う。

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。worktree、Gate 2・3、Git Publisher は software-engineer へ移し、この文書からは消した（本人決定 1）。

## 1. 目的

- Evaloom の構成要素と、それぞれが持つ権限を一か所で定める。
- 仕事の中身（準備・作業・確かめる・出す）はスペシャルハーネスが決め、それを動かす安全な場所（段ごとの隔離、鍵の保管、固定した版からの読み出し）を Evaloom が用意する、という分担を構成として固定する（vision.md 3章）。
- 「Runner は Control Plane へ pull で仕事を取りに行く」「作業の段に鍵を渡さない」「鍵を持つ段は AI を呼ばない」という境界を、構成として固定する（本人決定 4・7）。
- 本人用 API と Runner 用 API の一覧、認証区分、共通規約（エラー・ページネーション・入力検証）を定める。

## 2. 入出力

- 入力: 本人のブラウザからの HTTP リクエスト（Mac mini のループバック、または tailnet からの `tailscale serve` の中継、[`18-remote-access.md`](./18-remote-access.md)）、Runner からの HTTP リクエスト、Linear API の照会結果。
- 出力: JSON レスポンス、SSE ストリーム、Linear へのコメント・状態の変更・振り分けのラベルの追加。
- データの形は [`02-data-model.md`](./02-data-model.md) を参照する。

## 3. 処理詳細

### 3.1 構成要素と権限

| 構成要素 | プロセス | 実行内容 | 持つ認証情報 | 持たない認証情報 |
|---|---|---|---|---|
| Control Plane | `evaloomd`（launchd 常駐） | 本人用 API、Runner 用 API、Registry（Project・スペシャルハーネス・Profile・振り分けの表）、Linear Poller と振り分け、Task 管理、本人の判断待ち、承認の照合と消費、通知、Linear への書き戻し、SQLite | 本人のパスワードのハッシュとセッションの検証値、Runner トークンの検証値、Linear の API キー | スペシャルハーネスの鍵（GitHub など） |
| Management UI | `evaloomd` が静的ファイルを配信 | 画面表示。本人の判断はすべてここで受け付ける（本人決定 3） | 本人のセッション（ブラウザ） | - |
| Trusted Runner | `evaloom-runner`（launchd 常駐） | claim、作業領域の用意、Profile の固定版の読み出し、段ごとの Sandbox の作成、ミクロハーネスの Adapter の起動、段の通話口の中継、鍵の受け渡し、ログ収集 | Runner トークン、鍵の保管（3.5）、ミクロハーネスの Evaloom 専用の認証ファイル（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.5） | 本人用 API の認証情報、管理 DB への直接アクセス、Linear の API キー |
| Prepare Sandbox | Runner が段ごとに作る隔離環境 | スペシャルハーネスの固定版の準備のプログラム。AI を呼ばない | Profile が `access: read` と宣言した鍵だけ | 書き込み用の鍵、Runner トークン、上記すべて |
| Work Sandbox | 同上 | ミクロハーネス（Claude Code・Codex）による作業 | モデル API の接続手段だけ（[`09-sandbox.md`](./09-sandbox.md)） | すべての鍵、上記すべて |
| Check Sandbox | 同上 | スペシャルハーネスの固定版の確かめるプログラム。AI を呼ばない | なし | すべての鍵、上記すべて |
| Publish Sandbox | 同上 | スペシャルハーネスの固定版の出すプログラム。AI を呼ばない | Profile が宣言した鍵だけ | 宣言していない鍵、Runner トークン、上記すべて |
| Proposal Sandbox | Runner が提案ごとに作る隔離環境 | 振り分けの提案（AI が Issue を読み、振り分けの表から1つ選ぶ） | モデル API の接続手段だけ | すべての鍵、上記すべて |

- 段の名前と、段ごとに見える範囲は契約 6.2 の表を正本とする。Evaloom 側の実現方法（隔離の方式、ネットワークの許可）は [`09-sandbox.md`](./09-sandbox.md) で定める。
- 鍵を受け取る段（Prepare Sandbox と Publish Sandbox）では、AI（ミクロハーネス、モデルの API）を動かさない。ネットワークはスペシャルハーネスが宣言した宛先だけを通し、モデルの API の宛先は通さない（本人決定 4）。
- 作業の段（Work Sandbox）と振り分けの提案（Proposal Sandbox）には、モデル API のほかの外への接続と鍵を渡さない（本人決定 7）。Issue の本文は外から来た文であり、AI がそれに引きずられても鍵と外への接続が無いようにするためである（vision.md 3章）。
- 管理 DB（SQLite）に書き込むのは Control Plane だけとする。Runner は Runner 用 API を通じて報告する。
- Control Plane から Runner へは接続しない（requirements 2.3）。中止の指示も Runner が heartbeat で受け取る。
- 127.0.0.1 への限定だけでは権限境界にならない（requirements 15.7）。同じ Mac mini 上の Sandbox から HTTP を送れるため、どの Sandbox からもホストのループバックへの通信を止める（[`09-sandbox.md`](./09-sandbox.md)）。
- Control Plane はスペシャルハーネスの鍵を持たない。出す段の直前の問い合わせ（契約 7.4）で Control Plane が行うのは、中止の確認と、承認の照合と消費だけである。外への操作はスペシャルハーネスの出すプログラムが行う。
- 確かめたものと出すものが同じであることは、スペシャルハーネスが見張る（本人決定 5、契約 12章）。Evaloom は、承認したときの中身の識別子と出す直前の識別子だけを比べる。

### 3.2 プロセス配置

```
launchd (ユーザーエージェント)
├── evaloomd          … 127.0.0.1:<port> で待ち受け。SQLite (WAL) を所有。Linear Poller と Linear 送信 Worker を含む
└── evaloom-runner    … evaloomd の Runner 用 API に pull で接続。鍵の保管を読む
        ├── prepare sandbox  (Execution の準備の段ごと。Profile に宣言があるときだけ)
        ├── work sandbox     (Execution の作業の段ごと)
        ├── check sandbox    (Execution の確かめる段ごと)
        ├── publish sandbox  (Execution の出す段ごと)
        └── proposal sandbox (振り分けの提案ごと)
```

- 待ち受けは `127.0.0.1:<port>` だけとする。`0.0.0.0` や LAN 側アドレスでは待ち受けない。別端末からは `tailscale serve` の中継で届く（[`18-remote-access.md`](./18-remote-access.md) 3.1）。
- `evaloomd` と `evaloom-runner` は管理画面とは独立して常駐する（requirements 2.3）。
- 段ごとに Sandbox を作り直す。前の段の Sandbox を次の段に使い回さない。段の間で受け渡すのは、作業領域と、Runner が成果物として保存した `out`（契約 6.2）だけである。
- Runner は段の Sandbox の中に通話口（契約 7.4）を置き、段からの要求を Runner 用 API に中継する。通話口は段ごとに作り、段が終わったら閉じる。中継してよい要求は契約 6.2 の表の範囲だけとし、それ以外は Runner が拒否する。

### 3.3 段の流れと構成要素

| 段・処理 | 動かす構成要素 | Control Plane とのやり取り（Runner 経由） |
|---|---|---|
| 振り分けの提案 | Proposal Sandbox | claim で提案の仕事を受け取り、結果を `POST /api/route-proposals/{id}/result` で返す |
| 版の固定 | Control Plane（claim） | claim のトランザクションで Profile の版、渡す値を Execution に固定する |
| 準備 | Prepare Sandbox | 段の開始と結果を `POST /api/executions/{id}/result` で報告する |
| 作業 | Work Sandbox | 同上。質問（契約 8章）があれば本人の判断待ちになる |
| 確かめる | Check Sandbox | 同上。承認の要求と中身の識別子を報告する |
| 承認 | Control Plane と Management UI | 本人が管理画面で判断する |
| 出す | Publish Sandbox | 外への操作の直前に毎回 `POST /api/executions/{id}/authorize` を呼ぶ |
| 返す | Control Plane | 通知、Linear への書き戻し、記録の保存 |

### 3.4 Go バックエンドのモジュール境界（提案、Q21）

| パッケージ | 責務 | 対応仕様 |
|---|---|---|
| `cmd/evaloomd` | Control Plane の起動 | 01 |
| `cmd/evaloom-runner` | Runner の起動 | 05 |
| `internal/api` | HTTP ハンドラ、認証、CSRF、共通エラー | 01、18 |
| `internal/store` | SQLite アクセス、マイグレーション | 02 |
| `internal/contract` | 契約の型と検証（Profile 定義、要求 JSON、結果 JSON、finding） | 契約、02 |
| `internal/registry` | Project・スペシャルハーネス・Profile・振り分けの表 | 03 |
| `internal/routing` | Linear Poller、振り分けの判定、振り分けの提案の管理 | 15、19 |
| `internal/task` | Task / Execution の状態遷移、claim、同時実行制御、回収 | 04 |
| `internal/decision` | 本人の判断待ち（振り分け・承認・続行・質問）と回答の後の処理 | 04、17 |
| `internal/approval` | 承認の作成、照合、消費 | 17 |
| `internal/contextbuilder` | Execution Context の生成 | 07 |
| `internal/notify` | 通知の送信口（macOS、Linear） | 12、16 |
| `internal/runner` | pull ループ、段の実行手順、通話口の中継 | 05 |
| `internal/stage` | 段ごとの Sandbox の組み立て（見える範囲、ネットワーク、鍵） | 09、19 |
| `internal/adapter` | ミクロハーネスの Adapter（Claude Code・Codex） | 08 |
| `internal/sandbox` | 隔離方式の実装（コンテナ、macOS VM） | 09 |
| `internal/secretstore` | 鍵の保管と、段への受け渡し | 01 3.5、09 |

`internal/worktree`、`internal/gate`、`internal/publisher` は持たない（本人決定 1）。

### 3.5 鍵の保管

スペシャルハーネスが外とやり取りするときに使う鍵は Evaloom が保管し、準備の段と出す段を動かすときだけ渡す（vision.md 6章、本人決定 4・7）。

- 保管するのは Runner である。鍵の値は Control Plane に送らず、管理 DB にも入れない。Control Plane が持つのは、Profile が宣言した鍵の名前（slot）と使い方（`read` / `write`）だけである。
- 置き場所は Runner のデータディレクトリ（Git 管理外）の鍵ファイル（1つの鍵に1ファイル、権限 0600）とする案（Q116）。名前は `<スペシャルハーネス ID>/<slot>` とする（契約 11章、Q110）。
- 鍵の登録と入れ替えは、Mac mini の CLI だけで行う（案 `evaloom-runner secret set <harness-id>/<slot>`。値は標準入力から読み、画面に出さない）。管理画面と API からは登録しない。
- 鍵ごとに使い方（`read` / `write`）を登録時に記録する。準備の段には `read` の鍵だけを渡す。Profile が `access: read` と宣言しても、`write` で登録した鍵は準備の段に渡さない。
- 渡し方は契約 11章のとおり、宣言した段の Sandbox に環境変数 `EVALOOM_SECRET_<SLOT>` で渡す。Runner は、渡した値を段のログと成果物から除外してから保存・配信する（[`05-runner.md`](./05-runner.md)）。
- 宣言した鍵が保管に無いときは、段を始めずに `config_error` とする。
- Linear の API キーは Control Plane だけが使う鍵であり、この保管には入れない（[`15-linear-intake.md`](./15-linear-intake.md) 3.1）。

### 3.6 認証

| 区分 | 使う主体 | 方式 | 状態 |
|---|---|---|---|
| 本人用 | 管理画面 | ID とパスワードでログインし、端末ごとのセッション（有効期限3日）と CSRF トークンを使う（[`18-remote-access.md`](./18-remote-access.md) 3.3〜3.6） | 決定（18） |
| Runner 用 | Trusted Runner | Runner ごとのトークン（`Authorization: Bearer`）。Control Plane にはハッシュだけを保存する（Q3） | 案 |
| なし | すべての Sandbox | 本人用と Runner 用のどちらの認証情報も渡さない。段から Control Plane へは、Runner の通話口を通してだけ届く | 確定 |

本人用の認証は次のとおりとする。

- 起動時の一回限りのログイン URL は使わない（[`18-remote-access.md`](./18-remote-access.md) 3.6）。
- セッションは `HttpOnly; SameSite=Strict; Path=/` の Cookie で持ち、tailnet からの HTTPS では `Secure` を付ける（18 の 3.5）。
- 状態を変える要求（POST / PUT / PATCH / DELETE）は、Cookie とは別に `X-CSRF-Token` ヘッダを必須にする。
- `Host` / `Origin` は次の値だけを許可し、完全一致で照合する（DNS リバインディング対策）。`127.0.0.1:<port>` と `http://127.0.0.1:<port>`、起動時に指定した Tailscale の名前（`orionmac-mini.tailb76d84.ts.net`）と `https://<その名前>`（18 の 3.2）。

選択の判断基準: 同じマシンの不正なプログラムが Cookie を持たずに状態変更 API を呼べないこと、ブラウザ経由の CSRF が通らないこと。この2点を [`14-security-tests.md`](./14-security-tests.md) のテストで確認する。

開発時の例外は次のとおりとする（2026-10-26 本人決定）。

- 管理画面の開発中は、Vite の開発サーバ（`localhost:5173` / `127.0.0.1:5173`）が `/api` と `/auth` を `evaloomd` へ中継する。開発サーバの入口として、この2つの Host と、それぞれに対応する Origin（`http://localhost:5173` / `http://127.0.0.1:5173`）を許可する。
- 開発サーバは、`evaloomd` へ転送する前に次を照合する。Host が上の許可値のどちらかに一致し、かつ1つだけであること。Origin は無いか、1つだけで `http://<受信した Host>` に一致すること。
- 照合に通らない要求は、開発サーバが 403 を返して終える。`evaloomd` へは転送しない。
- 照合に通った要求だけ、Host と Origin を `evaloomd` の許可値へ書き換えて転送する。`evaloomd` 側の許可 Host / Origin は緩めない。
- 開発サーバで拒否した要求は `data/security/` 配下のファイルに1行1件の JSON で記録する（時刻、メソッド、パス、受信した Host と Origin、拒否理由）。Cookie・トークンの値は記録しない。`data/` は git の管理外。

Runner 用の認証は次のとおりとする（Q3 の案）。

- Runner 登録時に Runner ごとのトークンを発行し、Control Plane にはハッシュだけを保存する。登録は API にせず、Mac mini 上の `evaloomd` のサブコマンド（例 `evaloomd runner register <id>`）で行う。トークンは発行時に一度だけ表示する。
- Runner トークンでは Runner 用 API だけを呼べる。本人用 API は呼べない。逆も同じ。
- Runner は、自分が claim した Execution と振り分けの提案についてだけ報告できる。

### 3.7 Fail Closed

認証・入力検証・状態遷移の前提確認のどれかに失敗したら、副作用のある処理を行わない（requirements 15.1）。権限と状態は、要求を受けた時点と、副作用のある処理を実際に行う直前の両方で確認する（requirements 15.3）。出す段の直前の問い合わせに Control Plane が答えられないとき、Runner は段に `allowed: false`（`unavailable`）を返す（契約 7.4）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 待ち受け | `127.0.0.1:<port>` のみ | requirements 2.2、18 の 3.1 |
| 別端末からの経路 | tailnet 内の `tailscale serve` だけ。Funnel は使わない | 18 の 3.1（決定） |
| リアルタイム通信 | SSE | requirements 2.2 |
| HTTP | Go 標準 `net/http` | requirements 2.2 |
| DB | SQLite / WAL モード | requirements 2.2 |
| 常駐方式 | launchd | requirements 2.2 |
| Runner の接続方向 | Runner → Control Plane の pull のみ | requirements 2.3 |
| 段 | `prepare`（任意）→ `work` → `check` → `publish` | 本人決定 7、02 の 4.1 |
| 鍵を受け取る段 | `prepare`（読み取り用だけ）と `publish`。どちらも AI を呼ばない | 本人決定 4・7 |
| 作業の段の外への接続 | モデルの API だけ | 本人決定 7 |
| 本人の判断の場所 | 管理画面だけ | 本人決定 3 |
| 鍵の保管 | Runner。Control Plane と管理 DB に値を置かない | vision.md 6章、案（Q116） |
| ライブラリのバージョン | 詳細設計時に検証して固定 | requirements 2.2 |

## 5. インターフェース

### 5.1 本人用 API

認証区分はすべて「本人用」（`/auth/login` を除く）。

| Method | Endpoint | 内容 | 詳細 |
|---|---|---|---|
| POST | /auth/login | ID とパスワードでログインする（認証なし。Host / Origin の照合はする） | 18 |
| POST | /auth/logout | 自分のセッションを消す | 18 |
| GET | /api/session | ログイン中かどうかと CSRF トークン | 18 |
| GET | /api/projects | Project 一覧（振り分けのラベルを含む） | 03 |
| POST | /api/projects | Project 登録。入力は `id`、`name`、`harnessId`、`profileId`、`params`、`approvalWaivers`、`runnerId` | 03 |
| GET | /api/projects/{id} | Project 詳細 | 03 |
| PATCH | /api/projects/{id} | Project 更新（`name`、`harnessId`、`profileId`、`params`、`approvalWaivers`、`runnerId`） | 03 |
| GET | /api/routes | 振り分けの表 | 03、15 |
| POST | /api/routes | 振り分けのラベルを登録。入力は `label` と、`projectId` か `harnessId` のどちらか一方 | 03、15 |
| PATCH | /api/routes/{label} | 振り分け先を変更 | 03、15 |
| DELETE | /api/routes/{label} | 振り分けのラベルを消す | 03、15 |
| GET | /api/harnesses | スペシャルハーネス一覧 | 03 |
| POST | /api/harnesses | スペシャルハーネス登録（`id`、`name`、`repoUrl`、`repoPath`） | 03 |
| GET | /api/harnesses/{id} | スペシャルハーネス詳細 | 03 |
| PATCH | /api/harnesses/{id} | スペシャルハーネス更新（`name`、`repoUrl`、`repoPath`、`defaultProfileId`、`approvalWaivers`） | 03 |
| GET | /api/harness-profiles | Profile 一覧（`?harnessId=` で絞り込み） | 03 |
| POST | /api/harness-profiles | Profile の版を登録（タグを commit SHA に解決し、契約 5章で検証） | 03 |
| GET | /api/harness-profiles/{id} | Profile 詳細（定義の全文、宣言した段・承認・鍵の名前） | 03 |
| GET | /api/harnesses/{id}/records | スペシャルハーネスの記録の一覧 | 19 |
| DELETE | /api/harness-records/{id} | 記録を消す（次の仕事に渡さない） | 19 |
| GET | /api/tasks | タスク一覧 | 04 |
| POST | /api/tasks | 手動タスク作成。振り分け先は `projectId` か `harnessId` のどちらか一方 | 04 |
| GET | /api/tasks/{id} | タスク詳細（Execution 一覧を含む） | 04 |
| POST | /api/tasks/{id}/cancel | タスク中止 | 04 |
| POST | /api/tasks/{id}/retry | 再実行（新しい Execution を作成）。`mode` で `continue` / `fresh` を選ぶ | 04 |
| GET | /api/decisions | 本人の判断待ちの一覧（`?status=open` が既定。`?kind=` で絞り込み） | 04、13 |
| GET | /api/decisions/{id} | 判断待ちの詳細（振り分けの提案、承認の対象、finding、質問など） | 04、13 |
| POST | /api/decisions/{id}/answer | 本人の回答。形は 02 の 5.6 | 04、15、17 |
| GET | /api/approvals | 承認の履歴（`?status=` で絞り込み） | 17 |
| GET | /api/approvals/{id} | 承認の詳細 | 17 |
| POST | /api/approvals/{id}/renew | 期限切れの承認から新しい承認待ちを作る | 17 |
| GET | /api/executions/{id} | Execution 詳細（固定した版、渡した値、段ごとの結果、中身の識別子、外に出したもの） | 04、05 |
| GET | /api/executions/{id}/events | SSE 実行イベント | 05 |
| GET | /api/executions/{id}/logs | 保存済みログの取得 | 05 |
| GET | /api/executions/{id}/artifacts/{artifactId} | 成果物の取得 | 05、13 |
| GET | /api/runners | Runner 一覧 | 05 |
| GET | /api/notification-failures | 通知失敗の一覧（全 Execution と Execution の無い契機をまたいで新しい順） | 12、16 |
| GET | /api/linear/status | Linear の照会の状況 | 15 |
| GET | /api/settings | 全体設定（02 の 3.8 のキー） | 13 |
| PATCH | /api/settings | 全体設定の更新 | 13 |

使わなくなったエンドポイント: `POST /api/projects/{id}/harnesses`（役割ごとの割り当て。projects.harness_id と profile_id に置き換えた）、`POST /api/tasks/{id}/resume`（`POST /api/decisions/{id}/answer` に置き換えた）、`POST /api/approvals/{id}/decision`（同上）。

実装しない requirements 18章のエンドポイント: `/api/webhooks/linear`（定期照会にしたため）、`/api/evaluations*`、`/api/improvements`、`/api/projects/{id}/harnesses/{role}/rollback`、`/api/contexts`、`/api/models`（評価・改善の Phase で扱う）。

### 5.2 Runner 用 API

認証区分はすべて「Runner 用」。

| Method | Endpoint | 内容 | 詳細 |
|---|---|---|---|
| POST | /api/runners/{id}/claim | 次の仕事を取得する。要求に Runner の作業領域のルート `workspaceRoot`（絶対パス）を入れる（Q134）。応答の `job.kind` は `execution` / `route_proposal`。同時実行制御はここで行う | 04、05 |
| POST | /api/runners/{id}/heartbeat | 生存報告。要求は `bootId`、`runningExecutionIds`、`runningRouteProposalIds`、`cleanedWorkspaceIds`（削除した作業領域）。応答は `cancelExecutionIds`、`cancelRouteProposalIds`（Q133）、`cleanWorkspaceIds`（削除を指示する作業領域、Q137） | 05 |
| POST | /api/executions/{id}/events | ログ断片（`log`）の報告 | 05 |
| POST | /api/executions/{id}/artifacts | 成果物ファイル（ログ、段の `out` のファイル、要求 JSON、結果 JSON）の登録 | 05 |
| POST | /api/executions/{id}/result | 段の開始・終了と、段の結果 JSON（契約 6.5）の報告。Control Plane が状態を遷移させ、承認待ち・判断待ちを作る | 04、05 |
| POST | /api/executions/{id}/context | 固定した文脈を送り、Execution Context を受け取る | 07 |
| POST | /api/executions/{id}/authorize | 出す段の操作の直前の問い合わせ（契約 7.4）。中止の確認、承認を省く設定の読み直し、承認の照合と消費を1つの書き込みで行い、`allowed` と `reason` を返す | 17 |
| POST | /api/route-proposals/{id}/artifacts | 振り分けの提案のログの登録 | 15 |
| POST | /api/route-proposals/{id}/result | 振り分けの提案の結果（選んだラベル、理由、エラー分類） | 15 |

使わなくなったエンドポイント: `POST /api/approvals/{id}/consume` と `POST /api/executions/{id}/publish-check`（`POST /api/executions/{id}/authorize` に置き換えた、[`17-approval.md`](./17-approval.md) 5章）。

準備の段と確かめる段の通話口の `cancel_check`（契約 7.4）は、Runner が最新の heartbeat の応答で知っている中止指示から答える。Control Plane には問い合わせない。

### 5.3 共通規約

**エラー形式**（全 API 共通）

```json
{
  "error": {
    "code": "validation_failed",
    "message": "人間が読める説明",
    "details": [{ "field": "approvalWaivers", "reason": "not_waivable" }]
  }
}
```

| HTTP | code | 使う場面 |
|---|---|---|
| 400 | `validation_failed` | 入力検証の失敗 |
| 401 | `unauthenticated` | 認証情報が無い・無効 |
| 403 | `forbidden` | 認証区分が違う、CSRF 検証失敗、Host / Origin 不一致、Runner が持っていない Execution への報告 |
| 404 | `not_found` | 対象が無い |
| 409 | `conflict` | 状態遷移として許されない操作、判断済みの判断待ちへの回答、版の重複登録、ラベルの重複 |
| 422 | `precondition_failed` | 前提条件の不成立（回答の basis が行と違う、渡す値が Profile のスキーマに合わない、省けない承認を省く設定に入れた、Profile の段の宣言が全体設定の上限を超える） |
| 429 | `too_many_requests` | ログインの続けての失敗の抑止中（18 の 3.4） |
| 500 | `internal` | 想定外のエラー。詳細はログにだけ残す |

**ページネーション**: 一覧 API は `?limit=`（既定 50、上限 200）と `?cursor=` を受け、応答に `nextCursor` を返す。

**JSON の命名**: リクエスト・レスポンスの JSON は camelCase、DB の列名は snake_case（[`02-data-model.md`](./02-data-model.md)）。

**ID**: 文字列。形式は [`02-data-model.md`](./02-data-model.md) 3.1。

**時刻**: RFC 3339（UTC）。

**入力検証**: すべての API でサーバー側で検証する。管理画面の検証は補助であり、正本はサーバー側とする。

**不透明な値**: `params`、`contentId`、`outputs`、`target` は、契約の形（型と長さ）だけを検証し、中身を解釈しない（[`02-data-model.md`](./02-data-model.md) 3.1）。

### 5.4 SSE イベント

`GET /api/executions/{id}/events` は `execution_events` の行を時系列で配信する。再接続時は `Last-Event-ID` から再開する。イベント種別は [`02-data-model.md`](./02-data-model.md) 5.3 を参照する。

## 6. エラー処理

- 認証失敗・CSRF 失敗・Host / Origin 不一致は 401 / 403 を返し、`audit_logs` に記録する。
- 開発サーバが中継の前に拒否した要求は、`audit_logs` ではなく `data/security/` 配下のファイルに記録する（3.6 の開発時の例外）。
- Runner 用 API で、Runner が持っていない Execution・振り分けの提案への報告を受けたら 403 を返し、記録する。
- 段の結果 JSON が契約に合わなければ、Execution を失敗（`contract_error`）にする。
- `POST /api/executions/{id}/authorize` で DB の書き込みに失敗したら、`allowed: false` を返す（Fail Closed）。
- DB の書き込みに失敗したら、副作用のある後続処理を行わない（Fail Closed）。

## 7. スコープ外

- インターネットへの公開（Tailscale Funnel、公開 HTTPS エンドポイント、トンネル）（18）
- 複数 Runner の運用（Phase 6）。ただし API とデータは Runner ID を持ち、複数を前提とした形にしておく
- 利用者管理
- 仕事の中身（worktree、テスト、push、PR、マージなど）。スペシャルハーネスが持つ（vision.md 4章）

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) データモデル（もう一つのアンカー）
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) claim、同時実行制御、本人の判断の後の再開
- [`05-runner.md`](./05-runner.md) Runner 用 API の使い方と段の実行手順
- [`08-harness-adapter.md`](./08-harness-adapter.md) ミクロハーネスの Adapter
- [`09-sandbox.md`](./09-sandbox.md) 段ごとの Sandbox とループバックへの通信の遮断
- [`14-security-tests.md`](./14-security-tests.md) 認証と権限境界のテスト
- [`18-remote-access.md`](./18-remote-access.md) 別端末からのアクセスと本人のログイン
- [`19-routing-and-stages.md`](./19-routing-and-stages.md) 振り分け先・段・安全な場所

## 9. 実装単位

- [ ] [Backend] `evaloomd` と `evaloom-runner` の起動骨格、127.0.0.1 限定の待ち受け、launchd の plist
  - 受け入れ基準: 両プロセスが launchd から起動する; `evaloomd` が 127.0.0.1 以外のアドレスで待ち受けていないことを `lsof` で確認できる
- [ ] [Backend] API 共通基盤（エラー形式、ページネーション、入力検証、時刻・ID の形式）
  - 受け入れ基準: 5.3 のエラー形式と HTTP ステータスの対応がテストで確認できる; 一覧 API が limit と cursor で動く
- [ ] [Backend] 本人用 API の認証、CSRF 対策、Host / Origin 検証（18 の形）
  - 受け入れ基準: Cookie の無い状態変更要求が 401 になる; CSRF トークンの無い POST が 403 になる; 許可した2つの経路以外の Host / Origin の要求が 403 になる; 拒否が audit_logs に残る
- [ ] [Backend] Runner 登録のサブコマンド、Runner 用トークンの発行・ハッシュ保存・検証と、本人用 / Runner 用の区分の分離
  - 受け入れ基準: Runner トークンで本人用 API を呼ぶと 403; 本人のセッションで Runner 用 API を呼ぶと 403; DB にトークンの平文が残らない
- [ ] [Backend] 5.1 の Registry・振り分けの表・判断待ちの API を 02 の名前で作る（使わなくなった3つのエンドポイントを消す）
  - 受け入れ基準: Project の登録・更新が6項目以外の入力を 400 にする; `POST /api/decisions/{id}/answer` が 02 の 5.6 の形以外を 400 にし、basis の不一致を 422 にする; `/api/tasks/{id}/resume` と `/api/approvals/{id}/decision` が 404
- [ ] [Backend] 鍵の保管（3.5）と CLI
  - 受け入れ基準: CLI で登録した鍵の値が Runner のデータディレクトリの 0600 のファイルにだけあり、管理 DB とログに無い; `write` で登録した鍵が準備の段に渡らない; 宣言した鍵が無いと段を始めず config_error になる
- [ ] [Backend] 出す段の直前の問い合わせ（`POST /api/executions/{id}/authorize`）
  - 受け入れ基準: 中止要求があると allowed=false（cancelled）; 承認の contentId と違うと allowed=false（content_changed）で承認が invalidated になる; 同じ承認に並行に2回問い合わせると allowed=true が1回だけ; Control Plane に届かないと Runner が段に allowed=false（unavailable）を返す
- [ ] [Backend] SSE 配信基盤（Last-Event-ID による再開）
  - 受け入れ基準: 接続を切って Last-Event-ID 付きで再接続すると、欠けたイベントから再開する

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q114 | 段の通話口の方式（3.2、契約 7.4） | 決定（2026-11-10 本人、案を採用）: Runner が段ごとに作る Unix ドメインソケットを Sandbox の中に置く。コンテナと macOS VM の両方で使えるかを実装時に確かめ、使えなければ別のファイル記述子で同じ形の要求と応答をやり取りする |
| Q116 | 鍵の保管場所（3.5） | 決定（2026-11-10 本人、案を採用）: Runner のデータディレクトリの鍵ファイル（1つの鍵に1ファイル、0600）。macOS のキーチェーンは、launchd の常駐から確実に読めることを実測するまで使わない |
| Q122 | Linear の「管理画面で判断待ち」のコメントに載せる管理画面の URL | 決定（2026-11-10 本人、案を採用）: 18 で決めた Tailscale の名前の URL（`https://orionmac-mini.tailb76d84.ts.net/decisions/<id>`）を載せる。`-remote-host` の指定が無いときは判断待ちの ID だけを載せる |
