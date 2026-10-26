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

本人用APIの認証は `01-architecture.md` 3.4 のQ2案に基づく。
`NewHandler` は起動時に生成した `OwnerAuth` を必須で受け取る。
以下は仕様にない部分の実装判断。

- ログイン先は `GET /auth/login?token=...`。URLを起動時のターミナルに表示する。
  1回だけ利用でき、成功時にCookieと `{"csrfToken":"..."}` を返す。
  通常のアクセスログにはURLを出力しない。
- Cookie名は `evaloom_session`。秘密値はログイン用、セッション用、CSRF用を
  それぞれ32バイトの暗号学的乱数から生成し、paddingなしbase64urlにする。
  Cookieは `HttpOnly; SameSite=Strict; Path=/`、DomainなしのセッションCookie。
  TLSはスコープ外のためSecure属性は付けない。値はメモリにだけ保持し、
  有効期限はプロセス終了まで。再起動するとすべて再発行する。
- 認証済みの `GET /api/session` からCSRFトークンを再取得できる。
  ログイン応答を含め、応答に `Cache-Control: no-store` と
  `Referrer-Policy: no-referrer` を付ける。
- Hostは実際の待ち受け先、Originは `http://127.0.0.1:<実ポート>` と完全一致させる。
  Forwarded系ヘッダは参照しない。Origin省略はGET・HEAD・OPTIONSだけ許可する。
  同一オリジンの読み取りとログインURLへの直接アクセスで省略されうるためである。
  空・null・複数Origin、別スキーム、別ポートは拒否する。
- 検証順はHost、Origin、Cookie、CSRF。CookieとCSRFトークンの重複は拒否する。
  GET・HEAD・OPTIONS以外は未知のメソッドもCSRF検証の対象とする。
- 拒否記録は `actor=system`、`action=auth.reject`、`target_type=request`、
  `target_id=要求パス`。detailはmethod・reason・code・statusを保持する。
  reasonは `host_mismatch`、`origin_mismatch`、`invalid_session`、
  `invalid_csrf`、`invalid_login`。未認証の主体をownerとは記録しない。
  クエリ、Cookie、各トークン、要求本文、ヘッダの生値は保存しない。
  記録に失敗した場合は共通形式の500を返し、後続処理を実行しない。

Runner用の認証と認証区分の分離は後続タスクの範囲であり、この実装では
Runner用エンドポイントを登録しない。追加時は本人用の `/api/` ハンドラとは
別の認証経路へ登録する必要がある。
