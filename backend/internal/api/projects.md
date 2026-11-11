# Project APIの実装

正本は`docs/specs/02-data-model.md`の3.2節と`03-registry.md`。

登録では`id`、`name`、`harnessId`、`profileId`、`runnerId`を受け取る。
`params`の既定は`{}`、`approvalWaivers`の既定は`[]`。
`defaultRole`、`linearTeamId`、`linearRoutingLabel`や開発用の設定は受け付けない。
振り分けのラベルは`routes`に持つ。

PATCHは省略した項目を保持する。RunnerとProfileの存在、ProfileとHarnessの対応、
承認を省く操作の宣言を保存トランザクション内で確かめる。
Profileを変更した場合も、変更後の宣言に対して承認を省く設定を検証する。
保存と変更前後の監査記録は同じトランザクションで行う。

一覧と詳細には、既存の管理画面との接続用に、直接固定した1つのProfileを
`harnesses`配列にも返す。役割ごとの割り当ては保存しない。
Profile定義とparamsの契約全体の検証は、契約型・検証の実装単位で扱う。

承認を省く設定（`approvalWaivers`）は03の3.5の表で確かめる。Harnessの設定も同じ関数を使う。
文字列の配列でない、または同じ名前を2回含むときは400（field は`approvalWaivers`）。
Profileの`approvals`に無い操作は422（reason は`unknown_operation`）、
`waivable: true`でない操作は422（reason は`not_waivable`）。
