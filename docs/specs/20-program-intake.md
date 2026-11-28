---
status: draft
written: 2026-11-28
範囲: プログラムから鍵付きで仕事を受け付ける入口（プログラム用の認証区分、ループバックだけ）と、その仕事の結果（確かめる段の成果物と作業の要約）をプログラムから取り出す口。鍵の発行と無効化の CLI、受け付ける上限
---

# 20. プログラムからの入口と結果の取り出し口 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: 00-overview 1章の「ほかの入口（定期実行など）は後で足す」の具体化。定期実行そのものは Evaloom に作らない

この文書は下書きである。決まっていない点は「案」として本書 10章（Q234〜Q239）に載せている。

## 1. 目的

- Evaloom の外のプログラム（AI を使わないもの）が、本人のログインなしに、決まった振り分け先へ仕事を頼めるようにする。
- 頼んだ仕事の状態と結果（確かめる段の成果物、作業の要約）を、同じプログラムが取り出せるようにする。
- 最初の利用者は brain-harness の研究ループ（brain-harness の `docs/仕様検討.md` 15.4・15.10）。brain-harness の起動プログラムが launchd で毎朝この入口に「課題・問い」を渡し、取り込みプログラムが結果を `research/<課題>/sources/` に写す。起動の時刻を決めるのは brain-harness で、Evaloom は定期実行を持たない。
- 本人用の入口（18、ログインと CSRF）と Runner 用の入口は弱めない。プログラム用の鍵で本人用 API・Runner 用 API は呼べず、その逆もできない。

## 2. 確かめたこと（2026-11-28、この仕様を書く前の状態）

brain-harness の 15.10 の未確認点「Evaloom の成果物をプログラムから取り出せるか」の答え。

- 成果物は保存されている。作業の段の `out` の `report.md` は `out_file`（stage=`work`）、`summary.md` は `work_summary`（stage=`work`）として、確かめる段が `out` に写した `report.md` は `out_file`（stage=`check`）として残る（`backend/internal/runner/execution.go:348`、`backend/internal/runner/work_result.go:90`、researcher の `evaloom/check/run`）。
- 取り出す API は今もある。`GET /api/executions/{id}` が成果物の一覧を返し、`GET /api/executions/{id}/artifacts/{artifactId}` が中身を返す（`backend/internal/api/server.go:109`、`:112`）。
- ただし、どちらも本人用の認証区分（`/api/` 全体を `auth.Protect` が囲む。`backend/internal/api/server.go:51`）で、ログインのセッション Cookie が要る。`Authorization` ヘッダを付けると本人用 API は拒否する（`backend/internal/api/auth.go:130`）。
- したがって、今の Evaloom では、本人のパスワードを持たないプログラムは成果物を取り出せない。仕事を頼む `POST /api/tasks` も同じ理由で使えない（加えて CSRF トークンが要る）。プログラムに本人のパスワードを持たせる形は採らない（18 の本人用の認証を弱めるため）。

## 3. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | プログラム用 API の要求（`Authorization: Bearer <鍵>`、本文、`Idempotency-Key`）、CLI の引数 |
| 出力 | program_clients（本書 7章）、tasks（source=`program`、program_client_id）、executions、audit_logs、成果物の中身（読むだけ） |

## 4. 処理詳細

### 4.1 認証区分「プログラム用」

01 の 3.4 の認証区分（本人用、Runner 用）に、3つ目の「プログラム用」を足す。

- 鍵は 32 バイトの暗号学的乱数を base64url（パディングなし）にした文字列。DB には SHA-256 のハッシュだけを保存する（Runner のトークンと同じ形。`backend/internal/runnerauth/token.go`）。
- 要求ごとに DB のハッシュと照らし合わせる。メモリに覚えておかない（無効化をすぐ効かせるため）。
- 鍵1つは、登録した振り分け先1つにだけ仕事を頼める（4.4、Q235）。
- プログラム用の鍵で本人用 API・Runner 用 API を呼ぶと 403（`wrong_auth_class`）。本人のセッション Cookie や Runner のトークンでプログラム用 API を呼んでも 403。どちらでもない・知らない・無効化した鍵は 401（`invalid_client_key`）。拒否はすべて audit_logs に残す（01 の 6章）。

### 4.2 ループバックだけで受け付ける

- プログラム用 API は、Host が `127.0.0.1:<port>` と完全に一致する要求だけを受け付ける。`-remote-host` の名前（18 の 3.2）では受け付けない（403 `host_mismatch`）。
- `Origin` ヘッダが1つでもあれば拒否する（403 `origin_mismatch`）。ブラウザからは呼ばせない（CSRF と DNS リバインディングの対策）。
- `tailscale serve` が中継した要求も `127.0.0.1` から届く（18 の 3.2）。中継の目印になるヘッダ（`X-Forwarded-For`、`X-Forwarded-Host`、`X-Forwarded-Proto`、`Forwarded`、`Tailscale-` で始まるヘッダ）が1つでもあれば拒否する（403 `forwarded_request`）。中継が実際にどのヘッダを付けるかは実装時に実測する（Q238）。この拒否は守りの重ねであり、鍵が無ければどのみち受け付けない。

### 4.3 鍵の発行と無効化（CLI）

- 発行: `evaloomd -db <DB> client register -harness <スペシャルハーネス ID> [-name <表示名>] <client ID>`、または `-project <Project ID>`。どちらか一方が必須。鍵は標準出力に1回だけ出す。ログ・DB・audit_logs には値を残さない。
- 無効化: `evaloomd -db <DB> client revoke <client ID>`。revoked_at を書き、以後その鍵は 401。
- 一覧: `evaloomd -db <DB> client list`（ID、表示名、振り分け先、作成・無効化の時刻。鍵もハッシュも出さない）。
- 発行と無効化は audit_logs（`program_client.register`、`program_client.revoke`）に残す。
- 鍵の値を受け取ったファイルへ置くのは本人（Q234）。Evaloom は鍵のファイルを読まない・書かない。管理画面から鍵は発行しない。

### 4.4 仕事を頼む（`POST /api/programs/tasks`）

入力:

| 項目 | 必須 | 検証 |
|---|---|---|
| `title` | 必須 | 空白だけでない。500 文字まで |
| `body` | 必須 | 空白だけでない。20,000 文字まで |
| `acceptanceCriteria` | 振り分け先の Profile が `acceptanceCriteria: required` なら必須 | 04 の 3.1 と同じ |
| `Idempotency-Key` ヘッダ | 必須 | 1〜200 文字の表示可能な ASCII。同じ鍵・同じキーの再送には最初に作ったタスクを返す（200）。別の鍵で同じキーを使っても別のタスクとして扱う |

- 振り分け先は鍵に登録したものに決まる。要求で `projectId`・`harnessId` を送ると 400（`not_allowed`）。
- 振り分け先が今は無い（消された、スペシャルハーネスの既定の版が無い）なら 422（`route_unavailable`）。
- 上限（4.6）を超えるなら 429（`limit_exceeded`）。`details` にどの上限かを入れる。
- 処理は 04 の 3.1 と同じ1つのトランザクションで行う。違いは次のとおり。
  - tasks.source=`program`、program_client_id に鍵の client ID を書く。idempotency_key は `program:<client ID>:<Idempotency-Key>` とする（本人の手動作成のキーと混ざらないため）。
  - audit_logs は `task.create`（actor=`program:<client ID>`）。
- 応答（201）: `{ "taskId", "status", "createdAt" }`。
- 振り分けの提案（`routing`）は通らない。振り分け先は鍵で決まっているため。
- 仕事の後の流れ（順番待ち、段、承認、本人の判断待ち、通知）は手動のタスクと同じ。作業中の AI の質問や承認待ちは、今までどおり管理画面で本人が答える。プログラムからは答えられない（00-overview 1章「本人の判断は管理画面だけ」）。

### 4.5 状態と結果を取り出す

#### `GET /api/programs/tasks/{id}`

- その鍵で作ったタスクだけを返す。ほかの鍵や本人が作ったタスクは 404（あるかないかも教えない）。
- 応答:

```json
{
  "taskId": "01J...",
  "status": "completed",
  "createdAt": "2026-11-29T05:30:00Z",
  "updatedAt": "2026-11-29T05:52:10Z",
  "execution": {
    "id": "01J...",
    "seq": 1,
    "status": "completed",
    "errorClass": null,
    "summary": "check の段の結果 JSON の summary"
  },
  "files": [
    { "name": "report.md", "sizeBytes": 18234, "sha256": "..." },
    { "name": "summary.md", "sizeBytes": 812, "sha256": "..." }
  ]
}
```

- `status` はタスクの状態（02 の 5.1）。`waiting_approval` と `blocked` は「管理画面で本人の判断待ち」を表す。プログラムは待つか、次の回に回す。
- `execution` は最新の Execution。まだ無ければ null。
- `files` は、最新の Execution が `completed` のときだけ中身を入れる。それ以外は空の配列。中身は次の2種類だけ。
  - 確かめる段（`check`）の `out_file` の成果物。確かめた中身そのものであるため、作業の段の `out` ではなくこちらを渡す。
  - 作業の段の要約（`work_summary`）。名前は `summary.md`。
  - 同じ名前があれば確かめる段のものを使う。名前は成果物の name のまま（`/` を含むことがある）。
- ログ、要求 JSON、結果 JSON、質問の成果物は渡さない。

#### `GET /api/programs/tasks/{id}/files/{name...}`

- `files` に載る名前の中身を返す。載らない名前、`completed` でないタスクは 404。
- `Content-Type: application/octet-stream`、`Content-Length` を付ける。中身は Runner が鍵の値を除いた後に保存したもの（01 の 3.5）。
- 本文の大きさの上限は今の成果物の上限に従う（新しく作らない）。

### 4.6 上限（Q236）

- 鍵ごとに、終わっていない（`completed` / `failed` / `timed_out` / `cancelled` 以外の）タスクは2件まで。
- 鍵ごとに、作れるタスクは1日（JST の 0 時から）4件まで。冪等キーの再送は数えない。
- 上限は Evaloom の定数にする。変えるときはこの仕様を直す。
- 同時実行の上限（全体2本、振り分け先ごとに1本。04 の 3.3）は変えない。上限の内側で順番を待つ。

## 5. インターフェース

| メソッド | パス | 用途 | 認証 |
|---|---|---|---|
| POST | `/api/programs/tasks` | 仕事を頼む | プログラム用 |
| GET | `/api/programs/tasks/{id}` | 状態と取り出せるファイルの一覧 | プログラム用 |
| GET | `/api/programs/tasks/{id}/files/{name...}` | ファイルの中身 | プログラム用 |

CLI: `evaloomd -db <DB> client register|revoke|list`。

呼び方の例（鍵はファイルから読み、コマンドラインの引数に書かない）:

```sh
KEY_FILE="$HOME/Library/Application Support/brain-harness/evaloom-intake-key"
curl -sS -X POST http://127.0.0.1:<port>/api/programs/tasks \
  -H @<(printf 'Authorization: Bearer %s\n' "$(cat "$KEY_FILE")") \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: second-brain-2026-11-29' \
  -d '{"title":"second-brain: つながりの良し悪しを見分ける方法","body":"課題: second-brain\n問い: ..."}'
curl -sS http://127.0.0.1:<port>/api/programs/tasks/<taskId> -H @<(...)
curl -sS -o report.md http://127.0.0.1:<port>/api/programs/tasks/<taskId>/files/report.md -H @<(...)
```

エラーの形は 01 の 5.3 に従う。

## 6. エラー処理

| 事象 | 扱い |
|---|---|
| 鍵が無い・形が違う・知らない・無効化済み | 401 `unauthenticated`。audit_logs に残す |
| 本人のセッション・Runner のトークンでプログラム用 API を呼んだ、プログラム用の鍵でほかの API を呼んだ | 403 `forbidden`（reason `wrong_auth_class`）。audit_logs に残す |
| Host が `127.0.0.1:<port>` でない、Origin がある、中継のヘッダがある | 403。audit_logs に残す |
| 要求で振り分け先を送った・入力が不正 | 400 |
| 振り分け先が使えない | 422 |
| 上限を超えた | 429 |
| ほかの鍵のタスク・無いタスク・取り出せないファイル | 404 |
| DB の読み書きに失敗 | 500。受け付けない（Fail Closed、01 の 3.7） |

## 7. データモデル

**program_clients**（プログラム用の鍵。02 から参照する）

| 列 | 型 | 説明 |
|---|---|---|
| id | TEXT PK | client ID（URL のパスの1区切りに入る文字だけ） |
| name | TEXT | 表示名 |
| token_hash | TEXT UNIQUE | 鍵の SHA-256 |
| project_id | TEXT NULL FK | 振り分け先の Project |
| harness_id | TEXT NULL FK | 振り分け先のスペシャルハーネス（直接の振り分け） |
| created_at | TEXT | 発行した時刻 |
| revoked_at | TEXT NULL | 無効化した時刻 |

- project_id と harness_id はちょうど一方だけ入る（CHECK）。
- tasks に program_client_id（TEXT NULL FK）を足し、tasks.source の列挙に `program` を足す（02 の 5章）。source=`program` のときだけ program_client_id が入る。
- 列の追加・変更はマイグレーションで行う（02 の 3.1）。

## 8. 既存の仕様への影響

確定するときに、次の仕様を合わせて直す。直した仕様は、本人が確かめるまで status を draft に戻す。

| 仕様 | 直す箇所 |
|---|---|
| [`00-overview.md`](./00-overview.md) | 1章の入口に本書を足す。2.1 の図に「プログラム（鍵付き、ループバックだけ）」を足す。3章の索引に 20 を足す。5章のスコープ外の「Linear 以外の入口（定期実行など）」を「定期実行（起動は外のプログラムが行う）」に |
| [`01-architecture.md`](./01-architecture.md) | 3.4 の認証区分にプログラム用を足す。5章にプログラム用 API の表を足す |
| [`02-data-model.md`](./02-data-model.md) | program_clients、tasks.program_client_id、tasks.source の `program` |
| [`04-task-lifecycle.md`](./04-task-lifecycle.md) | 3.1 の隣にプログラムからのタスク作成（本書 4.4）への参照 |
| [`13-management-ui.md`](./13-management-ui.md) | タスクの一覧・詳細で source=`program` を「プログラム（client ID）」と出す |
| [`14-security-tests.md`](./14-security-tests.md) | 権限境界テストに本書 9章のテストを足す |
| `backend/launchd/README.md` | 鍵の発行と置き方の手順 |

## 9. テスト

- 鍵の値が DB・ログ・audit_logs に残らない。発行の出力にだけ出る。
- 無効化した鍵、知らない鍵、形の違う鍵は 401。
- プログラム用の鍵で本人用 API（例 `GET /api/tasks`）と Runner 用 API（例 claim）を呼ぶと 403。本人のセッションと Runner のトークンでプログラム用 API を呼ぶと 403。
- Host が `-remote-host` の名前、`Origin` あり、`X-Forwarded-For` などの中継のヘッダありの要求は、正しい鍵でも 403。
- 要求で振り分け先を送ると 400。作ったタスクの振り分け先は鍵に登録したもの。
- 同じ `Idempotency-Key` の再送は同じタスクを返し、上限に数えない。
- 終わっていないタスク2件で3件目が 429。1日5件目が 429。
- ほかの鍵が作ったタスク、本人が作ったタスクは 404。
- `completed` のタスクで `files` に確かめる段の `out_file` と `summary.md` だけが載り、中身を取り出せる。ログ・要求 JSON・結果 JSON・質問は載らず、名前を指定しても 404。`completed` でないタスクの `files` は空で、ファイルは 404。

## 10. 要確認

| # | 事項 | 案 |
|---|---|---|
| Q234 | 鍵の発行と置き場 | 本人が Mac mini で `evaloomd client register -harness researcher brain-research` を打ち、出た鍵を `~/Library/Application Support/brain-harness/evaloom-intake-key`（0600、本人だけが読める）に置く。AI は鍵を扱わない。置き場は brain-harness の起動・取り込みプログラムだけが読む |
| Q235 | 受け付ける仕事の範囲 | 鍵1つに振り分け先を1つだけ登録し、要求では振り分け先を選べない。brain-harness の鍵はスペシャルハーネス researcher への直接の振り分け（既定の版を使う）に固定する |
| Q236 | 上限 | 鍵ごとに、終わっていないタスク2件まで、1日（JST）4件まで。研究ループの量（1日に2課題まで、15.5）の倍 |
| Q237 | 取り出せるファイル | 最新の Execution が `completed` のときだけ、確かめる段の `out_file` と作業の要約 `summary.md`。失敗や判断待ちのときは状態だけ返す |
| Q238 | 中継の目印のヘッダ | 実装時に `tailscale serve` の中継が付けるヘッダを実測し、4.2 の一覧を確定する。実測するまでは 4.2 の一覧で拒否する |
| Q239 | パスの名前 | `/api/programs/...`。本人用の `/api/...` と区別できるよう、接頭辞を分ける |

## 11. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) 3.4 認証、3.6 Host / Origin、5章 API
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.1 タスク作成
- [`18-remote-access.md`](./18-remote-access.md) 3.2 許可する Host
- brain-harness の `docs/仕様検討.md` 15.4・15.10（最初の利用者）

## 12. 実装単位

- [ ] [Backend] program_clients のマイグレーション、tasks.source の `program` と program_client_id、鍵を発行・無効化・一覧する CLI（4.3、7章）
  - 受け入れ基準: 発行した鍵の値が標準出力にだけ出て、DB にはハッシュだけが残る; 振り分け先がちょうど一方でないと発行しない; 無効化した鍵は以後認証されない; 発行と無効化が audit_logs に残る
- [ ] [Backend] プログラム用の認証区分とループバックだけの境界、`POST /api/programs/tasks`（4.1、4.2、4.4、4.6）
  - 受け入れ基準: 9章の認証・境界・冪等・上限のテストが通る; 作ったタスクが手動のタスクと同じに claim される
- [ ] [Backend] `GET /api/programs/tasks/{id}` と `GET /api/programs/tasks/{id}/files/{name...}`（4.5）
  - 受け入れ基準: 9章の取り出しのテストが通る
