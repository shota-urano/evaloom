# Linear の状況と全体設定 API の実装判断

正本は`docs/specs/15-linear-intake.md`の5章・3.8・6章。以下は仕様で未確定の部分の実装判断。

- `GET /api/settings`は 02 3.8 の6キーをキーの接頭辞で入れ子にして返す:
  `{"linear":{"triggerLabel","routingLabelPrefix"},"routing":{"proposer","defaultRunnerId"},`
  `"limits":{"stageTimeoutMaxSec":{"prepare","work","check","publish"}},"approval":{"ttlSec"}}`。
  行が無いキーは 02 3.8 の既定値（`approval.ttlSec`は 17 の Q55 の 86400）を返す。
- `limits.stageTimeoutMaxSec`は Profile の登録（`registry.ParseStageTimeoutMaxSec`）と同じ読み方で表示する。
  正の整数が無い段は 0（その段はタイムアウトを許さない）と出す。PATCH は4段すべてを正の整数で置き換え、
  段の欠け・未知の段は 400。`approval.ttlSec`は承認の判断と同じく JSON の整数で保存し、PATCH は60以上だけを受ける
  （Q200、2026-11-20本人決定）。60未満は400`approval.ttlSec / out_of_range`で、同じPATCHの他のキーと監査記録も保存しない。
  TTLの上限は既存の整数型の範囲とし、段の上限設定には正の整数より強い検査を加えない。
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
