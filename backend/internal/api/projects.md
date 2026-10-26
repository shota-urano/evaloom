# Project APIの実装判断

正本は`docs/specs/03-registry.md`の3.1〜3.3・3.7・5〜6節、
`02-data-model.md`のprojects、`01-architecture.md`の5.3節。
以下は仕様で未確定の部分の実装判断。

- POSTは201、GET・PATCHは200でProjectの全列をcamelCaseで返す。
  POST・PATCHの応答には`warnings`の文字列配列も付ける。警告がなければ空配列。
  一覧は既存の`items`・`nextCursor`形式とID昇順のページネーションを使う。
- 登録には`id`、`name`、`localRepoPath`、`gitRemoteUrl`、`runnerId`、
  `baseBranch`、`defaultRole`が必要。IDは既存のslug検証を使う。
  `contextLocation`の省略値は`AGENTS.md`で、Phase 1ではこの値だけを受け付ける。
  `reviewRole`はPhase 5まで使わないためnullだけを受け付ける。
- nullable列の省略値はnull、配列は`[]`、設定オブジェクトは`{}`。
  `allowedOperations`と`approvalPolicy`はオブジェクトとして保存・表示する。
  内部キーやポリシーの実行効果は追加しない。
- `timeouts`は`setupSec`・`harnessSec`・`testSec`だけを受け付ける。
  指定値は正の整数とし、キーを省略した工程は仕様05の3.6に従って全体設定を継承する。
  今回は全体設定やタイムアウト実行処理を追加しない。
- PATCHは省略した項目を保持し、指定した項目を丸ごと置き換える。
  配列とオブジェクトの内部キーはマージしない。nullable列はnullで解除できる。
  ID・作成日時・更新日時は要求から変更できない。空のPATCHも再検証・監査記録を行う。
- 未登録Runnerは422で`runnerId / not_found`を返す。
  リポジトリ検証のfield・reasonは既存の`registry.ValidateRepository`をそのまま使う。
  必須値や型の不備は既存の400とdetails形式で返す。
- コピー許可リストには相対ファイルパスだけを受け付ける。
  空文字、空の要素、`.`・`..`要素、絶対パス、ワイルドカード、
  バックスラッシュ、NULを拒否する。
  仕様03の3.3にある秘密情報らしいファイル名は拒否せず警告する。
  ファイルの内容は読まず、コピーも実行しない。
- 登録・更新とaudit_logsへの書き込みを同じトランザクションで行う。
  登録のactionは`project.register`、更新は仕様の`project.update`。
  actorは`owner`、target_typeは`project`、target_idはProject ID。
  detailの`before`・`after`に設定を記録する。登録時のbeforeはnull。
  リモートURLに含まれる認証情報・クエリ・フラグメントは監査記録から除外する。
  トークンやファイル内容を監査記録に渡さない。監査書き込みの失敗時はProjectもロールバックする。
- パスは既存検証で実パスへ正規化する。同一ディレクトリかもトランザクション内で比較し、
  大文字小文字を区別しないファイルシステムでの別表記による重複も409にする。
  DBのUNIQUE制約でもProject IDと正規化したパスの重複を拒否する。
