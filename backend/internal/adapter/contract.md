# 共通契約の JSON 検証

仕様の正本は `docs/specs/02-data-model.md` の 4.2、5.7、5.8。
既存の `HarnessRequest` / `HarnessResult` を `encoding/json.Unmarshal` で読み込むと検証する。
Go で直接組み立てた値には `Validate()` を使う。

Phase 1 の JSON スキーマは次のように確定する。

- 契約版は文字列の `"1"`。各オブジェクトは仕様例にあるすべての項目を必須とし、項目名は camelCase と完全一致させる。定義外の項目は拒否する。
- `errorClass`、`prUrl`、`usage.inputTokens`、`usage.outputTokens` は null を許す。これらの項目も省略はできない。
- 配列は空でもよいが、null や null の要素は許さない。`summary` は空文字でもよい。時間とトークン件数は 0 以上とする。
- `task.acceptanceCriteria` は仕様 3.2 に従い、空文字を拒否する。
- `profile.definition` は JSON オブジェクトとして受け付け、内部の項目はこの層では検証しない。Profile 登録時の整合性検証は Registry の担当とする。
- ID、SHA、パス、Profile の版、URL の形式や実在性は、この JSON 検証では確認しない。仕様例のプレースホルダーもそのまま受け付ける。

`Validate()` は値と型を検証する。Go のスカラー型では JSON 項目の省略とゼロ値を区別できないため、入力 JSON の必須項目の検証には `json.Unmarshal` を使う。
不正な JSON を読み込んだ場合、既存の受け取り先の値は更新しない。

列挙値は `internal/model` で定義する。既存の `adapter.ErrorClass` と `adapter.ResultStatus` は型エイリアスとして同じ定義を使う。Adapter 登録表は引き続き任意の名前を登録でき、Phase 1 の Adapter 名の列挙型とは独立している。
