# ProjectへのProfileの固定

役割ごとの`POST /api/projects/{id}/harnesses`は廃止した。
`PATCH /api/projects/{id}`の`harnessId`と`profileId`で、Projectに1つの版を固定する。
保存先は`projects`であり、`project_harnesses`テーブルはマイグレーション0026で削除する。

正本は`docs/specs/02-data-model.md`の3.2節と`03-registry.md`の3.1節。
