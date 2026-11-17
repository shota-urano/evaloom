# 振り分けの提案

`ProposalAdapter`は、ExecutionやProfileを持たない提案の入口である。
Runnerは作業と同じ登録関数からAdapterを取得し、`StartProposal`を呼ぶ。
Claude Codeは作業と同じ監視、専用設定コピー、認証ファイルのOSロック、
認証更新の回収と書き戻しを使う。提案ではセッションを再開・保持しない。

提案にマウントするのは読み取り専用の入力、`out`、`scratch`、
専用設定コピーだけである。入力JSONはIssueのタイトル・本文と候補一覧だけを含む。
固定プロンプトはそれらを信頼しないデータとして扱うよう指示する。
Claude Codeの`--tools Read,Write`で他のツールを除き、`dontAsk`と
`permissions.allow`の`Read`、`Edit(//<out/proposal.jsonの絶対パス>)`を使う。
Writeのパス検査もEditの規則を使うことは
[Claude Codeの権限仕様](https://code.claude.com/docs/en/permissions#read-and-edit)に従う。

Runnerは標準出力から提案を組み立てない。`out/proposal.json`を通常ファイル・
64 KiB以下・labelとreasonのみ・reasonが2000文字以内として検査する。
認証の開始値と各CLI終了後の更新値を除外し、`proposal_result`として登録する。
ログも同じ提案に属する`log`成果物として登録する。

## 仕様判断が必要な箇所

- Codex 0.163.0にはツール単位の権限制限が無い（仕様08の3.9）。
  提案の「ファイルの読み取りとoutへの書き込みだけ」を表現できないため、
  `ErrProposalPermissionsUnsupported`を返し、CLIを起動しない。
- 提案のerrorClassには`config_error`と`work_failed`が無い（仕様02のroute_proposals）。
  認証・設定・CLIの失敗を既存の別分類へ読み替えず、Runnerのエラーとして返す。
  この場合は結果APIへ送らず、提案の行は終端状態にならない。
  対応する分類または写像が決まるまで、この経路は未対応である。

仕様書、公開APIの項目、DBの表・列には追加しない。
`POST /api/route-proposals/{id}/artifacts`は仕様01・05にある既定のAPIを接続したものである。

結合テストでは外部コンテナランタイムの実行ファイルだけを試験用にする。
Runner、Adapter、Sandboxの起動・検証・終了確認、認証処理、HTTP API、DBは本番の経路を使う。
実モデルによるCLIの権限検証はこのテストに含めない。
