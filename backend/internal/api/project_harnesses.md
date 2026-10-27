# ProjectへのProfile割り当てAPIの実装判断

正本は`03-registry.md`の3.6・5〜6節と`02-data-model.md`の3.2節。
以下は仕様で応答形式などが指定されていない部分の実装判断。

- POST `/api/projects/{id}/harnesses`は`role`、`harnessId`、`version`を受け付ける。
  `version`はProfile登録APIと同じSemVer。役割名は空白だけの値を拒否し、
  それ以外に仕様で指定されていない文字種や長さの制限を追加しない。
- 新規割り当ては201、既存の役割への割り当ては200。
  応答には保存した`projectId`、`role`、`harnessId`、`profileId`、`assignedAt`を返す。
  同じ版への再割り当ても割り当て日時と監査記録を更新する。
- Projectが無ければ404。指定したHarnessと版の組が未登録なら、Harness自体が
  未登録の場合も含め422 `precondition_failed`、`version / not_found`。
  SemVerでない版はProfile登録APIと同じ422、`version / invalid_format`。
  ID・JSONの不正と役割名の未指定には既存の400形式を使う。
- 本人用の認証・CSRF保護を使う。登録済みProfileのIDを保存し、Gitタグや
  最新版を参照しない。新しい版の登録によって割り当てを更新しない。
- ProjectとProfileの存在確認、変更前の割り当ての取得、保存、監査記録を
  同一トランザクションで行う。監査記録のactionは`project.assign_harness`、
  target_typeは`project`、target_idはProject ID。detailの`before`と`after`に
  割り当て行を残す。新規割り当ての`before`はnull。ProfileのIDから固定した版を
  参照できる。監査記録に失敗したら割り当ても取り消す。

default_roleの存在検証とExecutionへの版の固定はタスクのライフサイクル側で扱う。
