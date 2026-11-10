# Project APIの実装判断

正本は`docs/specs/03-registry.md`、`02-data-model.md`のprojects、`01-architecture.md`。
以下は仕様で未確定の部分の実装判断。

開発の列（ローカルリポジトリ、リモートURL、Base Branch、Context の場所、テストコマンド、
setup とその許可宛先、コピー許可リスト、承認の方針）は software-engineer へ移したため持たない
（`docs/specs/19-routing-and-stages.md` 3.11、マイグレーション0025）。送ると未知の項目として400になる。

- POSTは201、GET・PATCHは200でProjectの全列をcamelCaseで返す。
  一覧は既存の`items`・`nextCursor`形式とID昇順のページネーションを使う。
- 登録には`id`、`name`、`runnerId`、`defaultRole`が必要。IDは既存のslug検証を使う。
  `reviewRole`はPhase 5まで使わないためnullだけを受け付ける。
- nullable列の省略値はnull、`allowedOperations`と`timeouts`は`{}`。
  `allowedOperations`はオブジェクトとして保存・表示し、内部キーの実行効果は追加しない。
- `timeouts`は`harnessSec`だけを受け付ける。指定値は正の整数とする。
  今回は全体設定やタイムアウト実行処理を追加しない。
- PATCHは省略した項目を保持し、指定した項目を丸ごと置き換える。
  オブジェクトの内部キーはマージしない。nullable列はnullで解除できる。
  ID・作成日時・更新日時は要求から変更できない。空のPATCHも再検証・監査記録を行う。
- 未登録Runnerは422で`runnerId / not_found`を返す。Runnerの存在は保存のトランザクション内で確かめる。
  必須値や型の不備は既存の400とdetails形式で返す。
- 同じIDの登録は409を返す。
- 更新では保存のトランザクション内でProjectの全値を再確認し、読み取り後に変更されていたら409を返す。
- 登録・更新とaudit_logsへの書き込みを同じトランザクションで行う。
  登録のactionは`project.register`、更新は仕様の`project.update`。
  actorは`owner`、target_typeは`project`、target_idはProject ID。
  detailの`before`・`after`に設定を記録する。登録時のbeforeはnull。
  監査書き込みの失敗時はProjectもロールバックする。

## Linear 項目（仕様15の3.8・5章）

- `linearTeamId`・`linearRoutingLabel`は nullable で、省略値は null。null で解除する。
  空白だけの値は 400 と`<field> / required`で拒否する（Linear の Team やラベルに一致しないため）。
- 別の Project と同じ Team とラベルの組は、保存のトランザクション内で確かめて 409 にし、
  details に`linearRoutingLabel / duplicate`を返す。DB の UNIQUE 制約も残る。
- 保存のトランザクションで、どの Project も登録しなくなった Team の linear_poll_teams の行を消す
  （仕様15の3.8、Q64）。次の照会を待たずに消すので、すぐ登録し直しても最初の照会からやり直す。
