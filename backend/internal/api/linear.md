# Linear の状況と全体設定 API の実装判断

正本は`docs/specs/15-linear-intake.md`の5章・3.8・6章。以下は仕様で未確定の部分の実装判断。

- `GET /api/settings`は`{"linear":{"triggerLabel","routingLabelPrefix"}}`だけを返す。
  仕様01・13の他の全体設定（タイムアウト・通知先など）は応答の形が未定のため含めない。
- `PATCH /api/settings`は`linear`の中の指定したキーだけを置き換え、省略したキーを残す。
  未知のキーは 400`body / invalid_json`、null・空白だけの値は`linear.<key> / required`。
  1つでも拒否したら何も変えない。
- 全体設定の保存先は仕様に無い（02 にテーブルが無い）。`linearintake.LabelSettings`として
  Control Plane のプロセス内に持ち、再起動で既定値（`ai-ready`・`repo:`）に戻る。
- Poller は照会の回の始めに設定を1回読み、その回の一覧の取得・取り外しの判定・ルーティングに同じ値を使う。
- `GET /api/linear/status`は linear_poll_teams と linear_issue_states（`ai_ready=1`）だけを読む。
  どちらにも API キーと Linear の応答本文は入らない。Team は ID 順、Issue は Team・Issue ID 順。
- タスクの API（一覧・詳細）に`source`、`intakeBlockCause`（無ければ null）、
  `linear`（`{identifier,url}`、手動作成は null）を足す。
