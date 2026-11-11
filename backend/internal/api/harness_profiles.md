# Harness Profile APIの実装判断

正本は`03-registry.md`の3.4・5〜6節、ハーネス契約（`docs/contract/harness-contract-v1.md`）の
4章・5章、`02-data-model.md`の3.2・3.8節。以下は仕様で未確定の部分の実装判断。

- POST `/api/harness-profiles`は`harnessId`と`version`だけを受け付ける。
  新規登録は201、同じcommit SHAの再登録は200で既存行を返す。
  Harnessが無い場合は404。ID・JSONの不正は既存の400形式を使う。
- SemVerでない版は422 `precondition_failed`、`version / invalid_format`。
  タグ・定義の取得失敗は`tagName / not_found`・`definition / not_found`、
  YAMLとして読めない（1つのマッピングでない、複数の文書、重複したキー）ときは
  `definition / invalid_format`。`evaloom/profile.yaml`がシンボリックリンクなら読まない。
- 契約の検証（3.4の表）は`internal/contract`の`DecodeProfile`と`EnvironmentIssues`、
  リポジトリの中身の確認は`internal/registry`の`ReadProfile`が行う。失敗した規則は
  すべて`details`に入れる。`field`は定義の中のパス（例 `stages.prepare.secrets[0].access`、
  `stages.publish.network.allow[1]`）、`reason`は短い英語の識別子
  （`unknown_field`、`required`、`not_allowed_in_stage`、`differs_from_tag`、`ip_literal`、
  `wildcard`、`local_name`、`invalid_port`、`duplicate`、`model_api`、`exceeds_limit`、
  `outside_repository`、`not_found`、`symlink`、`submodule`、`contains_symlink`、
  `not_a_file`、`inside_work_files`、`unconvertible`、`invalid_schema`など）。
  形の誤りがある項目の下の項目は、重ねて報告しない。
- 参照するパスは、commitの`ls-tree`のモードで確かめる。`command`の先頭と`params.schema`は
  通常のファイル、`stages.work`の宣言はファイルかディレクトリとする。ディレクトリの中に
  シンボリックリンク・サブモジュールがあれば拒否する（作業の段に置くため）。
- `limits.stageTimeoutMaxSec`は設定の行が無ければ02の3.8の既定値を使う。値が読めない段は
  上限0として扱い、その段の`timeoutSec`を拒否する（Fail Closed）。
- Tool Permissionsの変換とモデルAPIの宛先はAdapterの登録表から得る。Adapterが
  `registry.ToolPermissionsChecker`・`registry.ModelAPIDeclarer`を実装していればそれを使う。
  変換を持たないAdapterでは、`allow`と`deny`が空のときだけ受け付ける。
  `runtime.kind: macos_vm`はmacOS VMの実装まで拒否する（09のQ147）。
- 定義はYAMLのコメント・表記を含めず、キー順の安定したJSONに変換して全文を保存する。
  definition_hashはその保存JSONのSHA-256。model_idは`stages.work.model`を写す。
- Git操作は既存の10秒の期限を共用し、全てDBトランザクションの外で行う。
  保存時にはHarness全体と同じ版の既存行を再確認する。
- 保存と監査記録は同一トランザクションで行う。actionは`profile.register`、
  target_typeは`harness_profile`。監査記録には定義の全文を複製しない。
- 同じ版・別SHAは定義の不正より409を優先する。監査記録のactionは
  `profile.register_conflict`とし、登録済みSHAと要求SHAを記録する。
  同じSHAなら行・監査記録を追加しない。登録済みの行は変更しない。
