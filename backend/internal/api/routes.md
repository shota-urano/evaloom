# 振り分け表のAPI

正本は`docs/specs/01-architecture.md`の5.1、`02-data-model.md`の3.2。

`GET /api/routes`はlabelの昇順で一覧を返す。limitとcursorでページを取得する。
`POST /api/routes`はlabelと、projectIdかharnessIdの一方だけを受け付ける。
`PATCH /api/routes/{label}`は振り分け先だけを置き換える。
`DELETE /api/routes/{label}`は登録を消し、204を返す。

登録時にsettingsの接頭辞と起動ラベルを読み、labelと振り分け先を検証する。
重複は409、未登録の振り分け先は422を返す。
変更・削除の対象が無ければ404を返す。
保存と変更前後の監査ログは同じトランザクションで書く。
表を変更しても、queued以降のtasksの振り分け先は変更しない。

Linear側のラベルの存在確認と警告は仕様03の振り分け表の詳細実装で扱う。
このAPIの登録・更新は外部のLinear APIを呼ばない。
