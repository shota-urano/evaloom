# 振り分けの提案

`ProposalAdapter`は、ExecutionやProfileを持たない提案の入口である。
Runnerは作業と同じ登録関数からAdapterを取得し、`StartProposal`を呼ぶ。
Claude CodeとCodexは作業と同じ監視、専用設定コピー、認証ファイルのOSロック、
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

Codexにはツール単位の権限制限が無いため、作業の段と同じくProposal Sandboxの
隔離で制限する（仕様08の3.10、2026-11-17本人決定）。作業領域は渡さず、
専用の`CODEX_HOME`で認証確認と`codex exec`を実行する。

認証・設定の不足は`config_error`、CLIの異常終了は`work_failed`として結果APIへ
報告し、提案を`failed`にして実行枠を空ける。ログ・提案結果の成果物と結果APIの
再送には中止可能なcontextを渡し、中止やRunner終了時には再送を止める。
保存済みのログと成果物はRunner側に残す。

仕様書、公開APIの項目、DBの表・列には追加しない。
`POST /api/route-proposals/{id}/artifacts`は仕様01・05にある既定のAPIを接続したものである。

結合テストでは外部コンテナランタイムの実行ファイルだけを試験用にする。
Runner、Adapter、Sandboxの起動・検証・終了確認、認証処理、HTTP API、DBは本番の経路を使う。
実モデルによるCLIの権限検証はこのテストに含めない。
