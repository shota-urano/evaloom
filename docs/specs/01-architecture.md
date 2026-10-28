---
status: confirmed
範囲: 整合性アンカー。構成要素・プロセス・権限境界・認証・API 一覧と共通規約
---

# 01. アーキテクチャ・インターフェース 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: 構成要素、プロセス配置、権限境界、認証、API 一覧と共通規約

この文書は整合性アンカーである。API のパス・認証区分・エラー形式はここだけで定義し、他の詳細仕様はここを参照する。

## 1. 目的

- Phase 1 の構成要素と、それぞれが持つ権限を一か所で定める。
- 本人用 API と Runner 用 API の一覧、認証区分、共通規約（エラー・ページネーション・入力検証）を定める。
- 「Runner は Control Plane へ pull で仕事を取りに行く」「ハーネスには認証情報を渡さない」という境界を、構成として固定する。

## 2. 入出力

- 入力: 本人のブラウザからの HTTP リクエスト（127.0.0.1）、Runner からの HTTP リクエスト。
- 出力: JSON レスポンス、SSE ストリーム。
- データの形は [`02-data-model.md`](./02-data-model.md) を参照。

## 3. 処理詳細

### 3.1 構成要素と権限

requirements 15.7 の表を Phase 1 の構成に落としたもの。

| 構成要素 | プロセス | 実行内容 | 持つ認証情報 | 持たない認証情報 |
|---|---|---|---|---|
| Control Plane | `evaloomd`（launchd 常駐） | 本人用 API、Runner 用 API、Registry、Task 管理、通知、SQLite | 本人用 API の検証鍵、Runner トークンの検証鍵 | GitHub の書き込み権限 |
| Management UI | `evaloomd` が静的ファイルを配信 | 画面表示 | 本人のセッション（ブラウザ） | - |
| Trusted Runner | `evaloom-runner`（launchd 常駐） | claim、worktree 準備、Sandbox の作成、Gate 1〜3、ログ収集 | Runner トークン、Base Branch の fetch に使う読み取り権限 | 本人用 API の認証情報、管理 DB への直接アクセス |
| Git Publisher | Runner から起動する独立した処理（プロセス分離の形態は実装時に確定） | 検証済み commit の push と PR 作成 | 対象リポジトリに限った GitHub の書き込み権限 | Runner トークン、本人用 API の認証情報 |
| Harness Sandbox | Runner が作る隔離環境 | AI によるコード変更と commit | モデル API の接続手段のみ（[`09-sandbox.md`](./09-sandbox.md)） | 上記すべて、プロジェクトの秘密情報 |
| Test Sandbox | Runner が作る隔離環境 | setup・テストの実行 | なし | 上記すべて、プロジェクトの秘密情報 |

- 管理 DB（SQLite）に書き込むのは Control Plane だけとする。Runner は Runner 用 API を通じて状態を報告する。Runner を別マシンに置く将来構成（requirements 2.3）でもこの経路を変えないためである。
- Control Plane から Runner へは接続しない（requirements 2.3）。中止の指示も Runner が heartbeat で受け取る。
- 127.0.0.1 への限定だけでは権限境界にならない（requirements 15.7）。同じ Mac mini 上のハーネスから HTTP を送れるため、Sandbox からループバックへの通信そのものを止める（[`09-sandbox.md`](./09-sandbox.md)）。

### 3.2 プロセス配置

```
launchd (ユーザーエージェント)
├── evaloomd          … 127.0.0.1:<port> で待ち受け。SQLite (WAL) を所有
└── evaloom-runner    … evaloomd の Runner 用 API に pull で接続
        ├── harness sandbox (Execution ごと)
        ├── test sandbox (setup / Gate 2 ごと)
        └── git publisher (push / PR 作成ごと)
```

- 待ち受けポートは実装時に確定する。`0.0.0.0` や LAN 側アドレスでは待ち受けない。
- `evaloomd` と `evaloom-runner` は管理画面とは独立して常駐する（requirements 2.3）。

### 3.3 Go バックエンドのモジュール境界（提案、Q21）

requirements 21章で詳細設計事項とされている。次は提案であり、実装時に確定する。

| パッケージ | 責務 | 対応仕様 |
|---|---|---|
| `cmd/evaloomd` | Control Plane の起動 | 01 |
| `cmd/evaloom-runner` | Runner の起動 | 05 |
| `internal/api` | HTTP ハンドラ、認証、CSRF、共通エラー | 01 |
| `internal/store` | SQLite アクセス、マイグレーション | 02 |
| `internal/registry` | Project / Harness / Profile | 03 |
| `internal/task` | Task / Execution の状態遷移、claim、同時実行制御、回収 | 04 |
| `internal/contextbuilder` | Execution Context の生成 | 07 |
| `internal/notify` | 通知の送信口（macOS 実装を差し替え可能に） | 12 |
| `internal/runner` | pull ループ、Execution の実行手順 | 05 |
| `internal/worktree` | worktree の作成・再利用・コピー | 06 |
| `internal/adapter` | Adapter インターフェースと Codex / Claude Code 実装 | 08 |
| `internal/sandbox` | Sandbox の作成・実行 | 09 |
| `internal/gate` | Gate 1〜3 | 10 |
| `internal/publisher` | push・PR 作成 | 11 |

### 3.4 認証

| 区分 | 使う主体 | 方式 | 状態 |
|---|---|---|---|
| 本人用 | 管理画面 | 認証 + CSRF 対策（requirements 15.5）。方式は Q2 | 実装時に確定 |
| Runner 用 | Trusted Runner | 本人用とは別の限定された認証方式（requirements 15.5）。Runner ごとのトークンを提案 | 実装時に確定 |
| なし | ハーネス、Test Sandbox | どちらの認証情報も渡さない | 確定 |

Q2 の提案（本人用）:

- `evaloomd` の起動時にセッション用の秘密値を生成し、本人がログインするための一回限りの URL を macOS 通知またはターミナルに出す。
- セッションは `HttpOnly; SameSite=Strict; Path=/` の Cookie で持つ。
- 状態を変える要求（POST / PUT / PATCH / DELETE）は、Cookie とは別に `X-CSRF-Token` ヘッダを必須にする。
- `Origin` / `Host` ヘッダが `127.0.0.1:<port>` 以外なら拒否する（DNS リバインディング対策）。

選択の判断基準: 同じマシンの不正なプログラムが Cookie を持たずに状態変更 API を呼べないこと、ブラウザ経由の CSRF が通らないこと。この2点を 14 のテストで確認する。

開発時の例外（2026-10-26 本人決定）:

- 管理画面の開発中は、Vite の開発サーバ（`localhost:5173` / `127.0.0.1:5173`）が `/api` と `/auth` を `evaloomd` へ中継する。開発サーバの入口として、この2つの Host と、それぞれに対応する Origin（`http://localhost:5173` / `http://127.0.0.1:5173`）を許可する。
- 開発サーバは、`evaloomd` へ転送する前に次を照合する。Host が上の許可値のどちらかに一致し、かつ1つだけであること。Origin は無いか、1つだけで `http://<受信した Host>` に一致すること。
- 照合に通らない要求は、開発サーバが 403 を返して終える。`evaloomd` へは転送しない（受信した値が `evaloomd` の許可値と一致する場合も転送しない）。
- 照合に通った要求だけ、Host と Origin を `evaloomd` の許可値へ書き換えて転送する。
- `evaloomd` 側の許可 Host / Origin は緩めない（`127.0.0.1:<port>` のまま）。
- 開発サーバで拒否した要求は `evaloomd` に届かず `audit_logs` に残らないため、`data/security/` 配下のファイルに1行1件の JSON で記録する（時刻、メソッド、パス、受信した Host と Origin、拒否理由）。Cookie・トークンの値は記録しない。`data/` は git の管理外。

Q3 の提案（Runner 用）:

- Runner 登録時に Runner ごとのトークンを発行し、Control Plane にはハッシュだけを保存する。
- Runner はトークンを `Authorization: Bearer` で送る。
- Runner トークンでは Runner 用 API だけを呼べる。本人用 API は呼べない。逆も同じ。

### 3.5 Fail Closed

認証・入力検証・状態遷移の前提確認のどれかに失敗したら、副作用のある処理を行わない（requirements 15.1）。権限と状態は、要求を受けた時点と、副作用のある処理を実際に行う直前の両方で確認する（requirements 15.3）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 接続元 | 127.0.0.1 のみ | requirements 2.2, 15.5 |
| リアルタイム通信 | SSE | requirements 2.2 |
| HTTP | Go 標準 `net/http` | requirements 2.2 |
| DB | SQLite / WAL モード | requirements 2.2 |
| 常駐方式 | launchd | requirements 2.2 |
| Runner の接続方向 | Runner → Control Plane の pull のみ | requirements 2.3 |
| ライブラリのバージョン | 詳細設計時に検証して固定 | requirements 2.2 |

## 5. インターフェース

### 5.1 本人用 API（Phase 1）

認証区分はすべて「本人用」。

| Method | Endpoint | 内容 | 出典 | 詳細 |
|---|---|---|---|---|
| GET | /api/projects | プロジェクト一覧 | 18章 | 03 |
| POST | /api/projects | プロジェクト登録 | 18章 | 03 |
| GET | /api/projects/{id} | プロジェクト詳細 | 追加案 | 03 |
| PATCH | /api/projects/{id} | プロジェクト更新（パス、テスト設定、許可リスト、権限設定） | 追加案 | 03 |
| GET | /api/harnesses | ハーネス一覧 | 18章 | 03 |
| POST | /api/harnesses | ハーネス登録 | 追加案 | 03 |
| PATCH | /api/harnesses/{id} | ハーネス更新 | 追加案 | 03 |
| GET | /api/harness-profiles | Profile 一覧（`?harnessId=` で絞り込み） | 18章 | 03 |
| POST | /api/harness-profiles | Profile の版を登録（SemVer と commit SHA を記録） | 追加案 | 03 |
| POST | /api/projects/{id}/harnesses | Harness Profile 割り当て（役割・Harness・版） | 18章 | 03 |
| GET | /api/tasks | タスク一覧 | 18章 | 04 |
| POST | /api/tasks | 手動タスク作成 | 18章 | 04 |
| GET | /api/tasks/{id} | タスク詳細（Execution 一覧を含む） | 18章 | 04 |
| POST | /api/tasks/{id}/cancel | タスク中止 | 18章 | 04 |
| POST | /api/tasks/{id}/retry | 再実行（新しい Execution を作成）。`mode` で継続 / 最初から を選ぶ | 18章 | 04 |
| POST | /api/tasks/{id}/resume | Blocked からの続行・中止の判断 | 追加案 | 04, 10 |
| GET | /api/executions/{id} | Execution 詳細（固定した版、Gate 結果、PR URL） | 追加案 | 04, 05 |
| GET | /api/executions/{id}/events | SSE 実行イベント | 18章 | 05 |
| GET | /api/executions/{id}/logs | 保存済みログの取得 | 追加案 | 05 |
| GET | /api/executions/{id}/artifacts/{artifactId} | 成果物（差分、テストレポート等）の取得 | 追加案 | 05, 13 |
| GET | /api/runners | Runner 一覧 | 18章 | 05 |
| GET | /api/settings | 全体設定（同時実行上限の表示、タイムアウト等） | 追加案 | 13 |
| PATCH | /api/settings | 全体設定の更新 | 追加案 | 13 |

「追加案」は requirements 18章に無いエンドポイントで、Phase 1 の機能を満たすために必要と判断したもの（Q4）。

Phase 1 で実装しない 18章のエンドポイント: `/api/webhooks/linear`、`/api/evaluations*`、`/api/improvements`、`/api/approvals*`、`/api/projects/{id}/harnesses/{role}/rollback`、`/api/contexts`、`/api/models`。

### 5.2 Runner 用 API（Phase 1）

認証区分はすべて「Runner 用」。

| Method | Endpoint | 内容 | 出典 | 詳細 |
|---|---|---|---|---|
| POST | /api/runners/{id}/claim | 次の仕事を取得。同時実行制御はここで行う | 18章 | 04, 05 |
| POST | /api/runners/{id}/heartbeat | 生存報告。応答で実行中 Execution の中止指示を返す | 追加案（Q3） | 05 |
| POST | /api/executions/{id}/events | 実行イベント・ログ断片の報告 | 追加案（Q3） | 05 |
| POST | /api/executions/{id}/artifacts | 成果物ファイル（ログファイル等）の登録。本文はファイルの中身そのもの | 追加案（evaloom-aor） | 05 |
| POST | /api/executions/{id}/result | 工程の結果（Gate 結果、検証 SHA、PR URL、エラー分類）の報告 | 追加案（Q3） | 05 |
| POST | /api/executions/{id}/context | 固定した Project / Harness Context を送り、Execution Context を受け取る | 追加案（Q3） | 07 |

Runner の登録とトークンの発行は API にせず、Mac mini 上で `evaloomd` のサブコマンド（例 `evaloomd runner register <id>`）で行う案とする。トークンは発行時に一度だけ表示する。

### 5.3 共通規約

**エラー形式**（全 API 共通）:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "人間が読める説明",
    "details": [{ "field": "acceptanceCriteria", "reason": "required" }]
  }
}
```

| HTTP | code | 使う場面 |
|---|---|---|
| 400 | `validation_failed` | 入力検証の失敗 |
| 401 | `unauthenticated` | 認証情報が無い・無効 |
| 403 | `forbidden` | 認証区分が違う（Runner トークンで本人用 API を呼んだ等）、CSRF 検証失敗、Origin 不一致 |
| 404 | `not_found` | 対象が無い |
| 409 | `conflict` | 状態遷移として許されない操作（Completed のタスクの中止等）、版の重複登録 |
| 422 | `precondition_failed` | パス検証の失敗、Blocked 続行時の SHA 不一致など、前提条件の不成立 |
| 500 | `internal` | 想定外のエラー。詳細はログにだけ残す |

**ページネーション**: 一覧 API は `?limit=`（既定 50、上限 200）と `?cursor=` を受け、応答に `nextCursor` を返す。値は実装時に確定。

**JSON の命名**: リクエスト・レスポンスの JSON は camelCase、DB の列名は snake_case（[`02-data-model.md`](./02-data-model.md)）。

**ID**: 文字列。形式は [`02-data-model.md`](./02-data-model.md) 3.1。

**時刻**: RFC 3339（UTC）。

**入力検証**: すべての API でサーバー側で検証する。管理画面の検証は補助であり、正本はサーバー側とする。

### 5.4 SSE イベント

`GET /api/executions/{id}/events` は `execution_events` の行を時系列で配信する。再接続時は `Last-Event-ID` から再開する。イベント種別は [`02-data-model.md`](./02-data-model.md) 5.3 を参照。

## 6. エラー処理

- 認証失敗・CSRF 失敗・Origin 不一致は 401 / 403 を返し、`audit_logs` に記録する。
- 開発サーバが中継の前に拒否した要求は、`audit_logs` ではなく `data/security/` 配下のファイルに記録する（3.4 の開発時の例外）。
- Runner 用 API で、Runner が持っていない Execution への報告を受けたら 403 を返し、記録する。
- DB の書き込みに失敗したら、副作用のある後続処理を行わない（Fail Closed）。

## 7. スコープ外

- 別端末・遠隔からのアクセス（Tailscale 等）、TLS 終端（requirements 15.5）
- Linear Webhook 用の公開 HTTPS エンドポイント（Phase 2）
- 複数 Runner の運用（Phase 6）。ただし API とデータは Runner ID を持ち、複数を前提とした形にしておく
- 利用者管理

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) データモデル（もう一つのアンカー）
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) claim と同時実行制御
- [`05-runner.md`](./05-runner.md) Runner 用 API の使い方
- [`09-sandbox.md`](./09-sandbox.md) Sandbox からループバックへの通信の遮断
- [`14-security-tests.md`](./14-security-tests.md) 認証と権限境界のテスト

## 9. 実装単位

- [ ] [Backend] `evaloomd` と `evaloom-runner` の起動骨格、127.0.0.1 限定の待ち受け、launchd の plist
  - 受け入れ基準: 両プロセスが launchd から起動する; `evaloomd` が 127.0.0.1 以外のアドレスで待ち受けていないことを `lsof` で確認できる
- [ ] [Backend] API 共通基盤（エラー形式、ページネーション、入力検証、時刻・ID の形式）
  - 受け入れ基準: 5.3 のエラー形式と HTTP ステータスの対応がテストで確認できる; 一覧 API が limit と cursor で動く
- [ ] [Backend] 本人用 API の認証、CSRF 対策、Origin / Host 検証
  - 受け入れ基準: Cookie の無い状態変更要求が 401 になる; CSRF トークンの無い POST が 403 になる; Origin が 127.0.0.1:<port> 以外の要求が 403 になる; 拒否が audit_logs に残る
- [ ] [Backend] Runner 登録のサブコマンド、Runner 用トークンの発行・ハッシュ保存・検証と、本人用 / Runner 用の区分の分離
  - 受け入れ基準: Runner トークンで本人用 API を呼ぶと 403; 本人のセッションで Runner 用 API を呼ぶと 403; DB にトークンの平文が残らない
- [ ] [Backend] SSE 配信基盤（Last-Event-ID による再開）
  - 受け入れ基準: 接続を切って Last-Event-ID 付きで再接続すると、欠けたイベントから再開する
