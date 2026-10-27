# Harness Profile APIの実装判断

正本は`03-registry.md`の3.5・5〜6節、`02-data-model.md`の3.2・4.3節、
`01-architecture.md`の5.1・5.3節。以下は仕様で未確定の部分の実装判断。

- 定義ファイルとタグにはQ5の提案を採用し、`evaloom/profile.yaml`と
  `profile/v<SemVer>`を使う。SemVerは3要素を必須とし、prereleaseとbuildを許可する。
  数値の先頭ゼロなど不正な表記は拒否する。annotated tagもcommitへ解決する。
- POST `/api/harness-profiles`は`harnessId`と`version`だけを受け付ける。
  新規登録は201、同じcommit SHAの再登録は200で既存行を返す。
  Harnessが無い場合は404。ID・JSONの不正は既存の400形式を使う。
- SemVerでない版は422 `precondition_failed`、`version / invalid_format`。
  定義内の版の不一致は`profileVersion / invalid`、Adapterの不一致は
  `executionConfiguration.adapter / invalid`とする。仕様にreasonの指定がないため、
  既存のreasonを使う。タグ・定義の取得失敗は`tagName / not_found`・
  `definition / not_found`、定義のスキーマ不一致は`definition / invalid_format`。
- 4.3の全フィールドを必須とし、文字列・配列・オブジェクトの型と未知のキーを検証する。
  空のskillVersions・allowedTools・argsは許可する。版・Model ID・Skillの名前などは
  空白以外を含む文字列とし、Phase 1のcollaborationPolicyは`solo`、
  evaluationPolicyVersionは明示的なnullだけを受け付ける。
  Model Registryや追加引数の実行時の許可判定はこの登録処理に追加しない。
- hb9.4では、ファイル選択のパス欄が仕様に無いため、任意の`contextFiles`配列を追加した。
  個別のリポジトリ相対パスだけを指定し、省略・空配列は空の選択とする。
  null・重複・絶対パス・正規化されていないパスは登録時に拒否する。
  選択も定義全文とハッシュに含める。実行時の扱いは`context.md`を参照する。
- YAMLのコメント・表記は保存対象に含めず、全文をキー順の安定したJSONへ変換して
  definitionに保存する。definition_hashはその保存JSONバイト列のSHA-256とする。
  YAMLパーサーにはyaml.v3、ULID生成にはoklog/ulidを使う。
- GETは既存のID昇順・items/nextCursor形式を使い、harnessIdで絞り込める。
  cursorは絞り込み条件にも結び付ける。存在しないHarnessでの検索結果は空の一覧。
- Git操作は既存の実装と10秒の期限を共用し、全てDBトランザクションの外で行う。
  タグ名は検証済みSemVerから組み立て、rev-parseには--end-of-optionsを指定する。
  定義は解決済み40桁SHAから読み、HEADや未コミットのファイルを参照しない。
  保存時にはHarness全体と同じ版の既存行を再確認する。
- 保存と監査記録は同一トランザクションで行う。actionは仕様例の`profile.register`、
  target_typeは`harness_profile`。監査記録にはID・版・SHA・ハッシュ・状態だけを含め、
  定義内にあり得る秘密情報を複製しない。監査記録の失敗時には登録も戻す。
- 同じ版・別SHAは定義の不正より409を優先する。監査記録のactionは
  `profile.register_conflict`とし、登録済みSHAと要求SHAを記録する。
  同じSHAなら行・監査記録を追加しない。登録済みの行は変更しない。

Projectへの割り当てとExecution時の検証は別の実装単位に委ねる。
