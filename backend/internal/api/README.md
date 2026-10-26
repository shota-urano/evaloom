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
登録直後でheartbeatを受信していない場合、`lastHeartbeatAt` は `null`。
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
- 本人用の検証順はHost、Origin、Authorizationの区分、Cookie、CSRF。
  CookieとCSRFトークンの重複は拒否する。
  GET・HEAD・OPTIONS以外は未知のメソッドもCSRF検証の対象とする。
- 拒否記録は `actor=system`、`action=auth.reject`、`target_type=request`、
  `target_id=要求パス`。detailはmethod・reason・code・statusを保持する。
  reasonは `host_mismatch`、`origin_mismatch`、`invalid_session`、
  `invalid_csrf`、`invalid_login`、`invalid_runner_token`、`wrong_auth_class`、
  `runner_mismatch`。未認証の主体をownerとは記録しない。
  クエリ、Cookie、各トークン、要求本文、ヘッダの生値は保存しない。
  記録に失敗した場合は共通形式の500を返し、後続処理を実行しない。

Runner用の認証は `01-architecture.md` 3.4のQ3案と5.2、6節、
保存先は `02-data-model.md` 3.2の `runners` に基づく。
以下は仕様にない部分の実装判断。

- 登録は `evaloomd -db <DBパス> runner register -worktree-root <絶対パス>
  [-name <表示名>] <Runner ID>`。Goのflag規約に合わせ、フラグはIDより前に置く。
  表示名を省略するとIDを使う。サーバーを起動せずDBを初期化して登録し、
  トークンだけを1行で出力する。通常の `evaloomd` 起動時には再表示しない。
- Runner IDは空文字、`.`、`..`、スラッシュ、バックスラッシュ、空白、制御文字、
  不正UTF-8を拒否する。APIの単一パス要素として扱える最小限の制約とし、
  ULIDやslugには限定しない。表示名は非空のUTF-8で制御文字を含まないこと。
  worktree rootは絶対パスを必須とし、正規化して保存する。
  ディレクトリの作成、存在や権限の検証はこのコマンドでは行わない。
- トークンは32バイトの暗号学的乱数をpaddingなしbase64urlで表す。
  DBにはトークン文字列のSHA-256を小文字hexで保存する。
  十分な乱数を持つトークンなので、パスワード用の低速ハッシュは使わない。
  登録の書き込みが成功した後に一度だけ表示し、重複IDはエラーとする。
  既存行のトークンを上書きせず、失敗時はトークンを表示しない。
  表示先への書き込みに失敗した場合はエラーを返すが、登録済みの行は残る。
  トークンの復元・ローテーション・削除用コマンドは追加しない。
- 登録時は `status=offline`。既存のNOT NULL制約に合わせ、未受信の
  `last_heartbeat_at` は空文字を保存し、一覧では `null` を返す。
  未受信以外の時刻は従来どおりUTCのRFC 3339で扱う。
- Runnerは単一の `Authorization: Bearer <token>` で認証する。
  Bearerの大文字小文字は区別しない。複数ヘッダや余分な要素、非正規の
  base64url表記、未知のトークンは401。要求ごとにDBのハッシュでIDを解決し、
  キャッシュしない。同じハッシュが複数のRunnerを指す場合は500で拒否する。
- Runnerからの要求はOriginとCSRFを必須にしない。Hostの一致と、指定された
  Originの一致は検証する。本人用のOrigin・CSRF検証は従来のまま維持する。
- 本人用APIで有効なRunnerトークンを検出した場合は403。
  Runner用APIで有効な本人セッションを検出した場合も403。
  両方の認証情報を送った場合も403とし、一方の認証を他方へ流用しない。
  本人用APIへの不正なAuthorizationは、本人のCookieがあっても401。
  拒否は従来と同じ `auth.reject` の監査記録を残す。
- 最小のRunner用エンドポイントに `POST /api/runners/{id}/claim` を選んだ。
  Runner IDの一致まで確認でき、heartbeatやExecutionの状態を変更する必要が
  ないためである。認証成功後も割り当ては行わず、共通形式の422
  `precondition_failed`（`Runner claim is not available`）を返す。
  仕事の割り当て自体はTaskの後続実装に委ねる。
- 01の5.2にある残りのPOSTパスもRunner用認証に予約する。
  認証成功後は共通形式の404を返し、業務処理は追加しない。
  同じパスのGETは本人用認証を通るため、SSEのGETとRunner報告のPOSTを
  区別できる。Execution報告の業務処理を追加するときは、認証したRunnerの
  IDを使ってExecutionの所有権を副作用の直前にも検証すること。
