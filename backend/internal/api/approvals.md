# 承認と判断待ちのAPI

正本は`docs/specs/01-architecture.md`の5.1と、`02-data-model.md`の3.4・5.6。

承認の一覧・詳細・再承認は`/api/approvals`で扱う。
回答は`POST /api/decisions/{id}/answer`だけで受け付ける。
旧`POST /api/approvals/{id}/decision`は404を返す。
承認の応答は`contentId`、`decisionId`、`requestedStage`、`summary`を含む。
旧`contentVersion`、`decidedBy`、`result`は返さない。

判断待ちの一覧は`GET /api/decisions`で取得する。
`status`の既定は`open`で、`answered`と`withdrawn`も指定できる。
`kind`で種類を絞り込み、`limit`と`cursor`でページを取得する。
一覧はIDの降順で返し、cursorはstatusとkindに結び付ける。
詳細は`GET /api/decisions/{id}`で取得し、承認か提案の行を添える。
質問やfindingは行の`detail`で返す。

回答の項目は02の5.6の各行に限定する。
必須項目の欠け、型の違い、許されないaction、余分な項目は400を返す。
承認の`contentId`は1〜256文字の印字可能なASCIIであり、commit SHAには限定しない。
`ttlSec`はapproveだけで指定でき、60秒以上の整数とする。
省略した場合は`approval.ttlSec`を使い、設定が無ければ24時間とする。
有効期限がRFC 3339の4桁の年に収まらない値は保存できないため400を返す。
回答済みか取り下げ済みの判断は409、basisの不一致は422を返す。
routeのapproveでは、その時点のroutesにlabelが存在することも確認する。
routeのrejectは、5.6の回答例どおりactionだけを受け付ける。

回答後の処理は`decisionAnswerApplier`で分離している。
APIは同じトランザクション内で入力とbasisを検証して処理を呼び、成功したら
answer・answered_at・監査ログ・decision_answeredイベントを保存する。
現在は既存の承認処理だけを接続している。
route・blocked・questionの状態遷移は仕様04の別実装単位で接続する。
未接続の種類へ正しい回答を送った場合は500を返し、判断待ちも関連行も変更しない。

このタスクでは、承認後の既存の単一操作の処理を引き継ぐ。
複数操作の承認の集約、check_blockedからの承認要求の作成、提案の承認後の
ExecutionとLinear outboxの作成は仕様04・15・17の実装単位で扱う。
