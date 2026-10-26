# API 共通基盤

仕様の正本は `docs/specs/01-architecture.md` 5.3 と
`docs/specs/02-data-model.md` 3.1。以下は仕様に記載がない部分の実装判断。

- 一覧の応答は `{"items": [], "nextCursor": ""}`。終端の cursor は空文字列。
- limit は 1〜200 の十進整数。省略時は50。空文字、重複指定、不正値、範囲外は400。
  上限を超えた値を200へ丸めない。
- cursor は `resource + "\n" + lastID` の padding なし base64url。
  別の一覧の cursor、不正な形式、cursor の重複指定は400。空文字は先頭ページ。
- 並び順は ID の昇順（SQLite の BINARY 順）。cursor の ID より後を取得する。
  その ID の行が削除されても継続できる。limit の変更は許可する。
  一覧全体のスナップショットは保持しない。
- エラーの `details` は常に配列。詳細がない場合は空配列。
  共通の入力検証では `required`、`invalid_format`、`invalid_json`、
  `invalid`、`out_of_range` を reason に使う。
- JSON 入力はオブジェクト1個だけを受け付け、未知のフィールドを拒否する。
  必須項目などの業務上の検証はハンドラ側で追加する。
- ULID は大文字の26桁。slug は小文字英数字をハイフンで連結した形式。
  task ID の連番は数字列とし、桁数を固定しない。ID の長さ上限は追加しない。
- 時刻は UTC を受け付け、`+00:00` を `Z` に統一して返す。
  秒未満の精度がある場合は保持する。

実エンドポイントは `GET /api/runners`（01 の5.1、05 の5節、13 の3章）。
既存の runners テーブルだけで読み取りを実現でき、登録処理を追加する必要がないため選んだ。
返却項目は `id`、`name`、`status`、`lastHeartbeatAt`、`createdAt`。
トークンのハッシュとローカルパスは返さない。Runner ID は文字列として保持し、
02 の runners の例 `mac-mini` にない制約をこの一覧で加えない。

本人用 API の認証、CSRF、Origin / Host の検証は後続タスク evaloom-vpq.3 が追加する。
