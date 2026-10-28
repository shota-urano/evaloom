# 保存済みログの取得

仕様の正本は `docs/specs/01-architecture.md` 5.1・5.3、
`02-data-model.md` の artifacts と execution_events、`05-runner.md` 3.4・3.5、
`13-management-ui.md` 3.3。

以下は仕様に応答形式の記載がない部分の実装判断。

- 本人用の `GET /api/executions/{id}/logs` は
  `{"harness":"...","setup":"...","test":"...","runner":"..."}` を返す。
  各値はログ本文で、未保存のstreamは空文字列。
- harness・setup・testは、それぞれ `harness_log`・`setup_log`・`test_log` の
  成果物ファイルを読む。同じstreamのファイルが複数ある場合は
  `created_at`、`id` の昇順で連結する。区切り文字の追加や改行変換はしない。
  対応するlogイベントは重ねて返さない。
- runnerに対応する成果物kindは仕様にない。新しいkindを作らず、既存の
  `execution_events` の `type=log`、`payload.stream=runner` をID昇順で読み、
  各 `text` にLFを付けて返す。Runnerの行単位ログと同じ表現とする。
- 終了状態だけに限定する規定はないため、実行中も保存済みの内容を返す。
  成果物の未登録分をイベントから補完する処理は追加しない。
  不正なIDは共通検証の400、存在しないExecutionは404。
  ファイルやDBの読み出し失敗は共通形式の500とし、部分的な成功応答は返さない。

runnerのファイル保存方法・成果物kindは仕様の穴として残る。
