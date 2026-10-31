# 開始点のテスト基準値

`Runner.BaselineHandler`で作ったハンドラを`StageTestBaseline`に渡す。
工程表に従い、setupの後、ハーネスの前に呼ぶ。
worktree準備側は`Execution.WorktreeID / WorktreePath / BaseSHA`を渡し、
ハンドラは基準値と同じresult報告でControl Planeにworktreeと開始点SHAを関連付ける。

`BaselineRepository.FindBaseline`は、現在のExecutionから同じworktreeを使った
過去のExecutionまで検索する。保存済みならテスト、解析、ID保存を呼ばない。
元のtest_runsとartifactsを参照し、新しいExecutionへコピーしない。
freshで別のworktreeを作った場合は、同じ開始点SHAでも新しく取得する。

新規取得ではRegistryのコマンド・レポート設定と開始点SHAを
`BaselineTestSandbox.RunBaseline`へ渡す。`SandboxBaseline`は既存の
`sandbox.Sandbox`と`Execution.WaitProcess`を使い、`kind=test_run`で実行する。
開始点とHEADの一致確認は`SandboxBaseline`が行う。continueで基準値が無いときも、
変更後のHEADを開始点として記録したり、worktreeをresetしたりしない。
レポートは今回の実行で作られたものだけを返す。

解析には先行タスクの`gate.ParseJUnitXML`を利用する。
取得した件数を`model.TestRun`へ移し、IDが取得可能なら`RecordIDs`で
`test_ids`成果物を保存して、そのIDをtest_runsから参照する。
IDが取得できない場合も件数を保存する。欠落・不正レポートの件数はNULLにする。
保存を確認してからハーネスへ進む。保存・検索の失敗を未取得として扱わない。

Control Plane側の`store.BaselineStore`は、既存のtest_runsへ保存する。
worktreeとExecutionのTask・Project・Runner・開始点を確認し、トランザクションで
重複登録と上書きを防ぐ。同じレコードの再送は受け付ける。
Runnerに管理DBを開かせる実装は追加していない。

## 通信とSandboxの接続

- 基準値は既存の`POST /api/executions/{id}/result`に`baseline`として送る。
  `fromStatus`と`status`はともに`preparing`とする。本文には`worktreeId`と
  `baseSha`も指定する。この測定報告では状態遷移・通知・Gate報告を行わない。
  resultハンドラは`store.BaselineStore.SaveBaselineTx`を使い、関連付けと基準値を
  同じトランザクションで保存する。同じID・内容の再送は204、置換は409にする。
- claim応答の`worktree: {id, path, baseSha, baseline}`で保存済みの基準値を渡す。
  continueでは直前のExecutionと同じworktreeを検索し、元のtest_runsを返す。
  Runnerを再生成してもこの応答で再取得を防ぐ。`worktree: null`は前回までに
  worktreeが記録されていないことを示し、準備工程で新規作成したworktreeでは
  基準値を取得する。`worktree`項目自体が欠ける応答はエラーにする。
  `cleaned`のworktreeも元のパス・開始点SHA・基準値を返し、準備工程で
  ブランチから復元した後に基準値を引き継ぐ。新しいエンドポイントは追加しない。
- ID一覧はJSON配列にして既存のartifacts APIに`kind=test_ids`で送る。
  送信失敗時は同じID・内容で再送する。基準値のresult報告も再送できる。
  どちらの保存確認も済んでからハーネスへ進む。
- `BaselineConfig`には既存の`sandbox.Sandbox`、明示的なリソース上限、Sandbox内の
  shellの絶対パス、Registryで使うJUnit形式名を渡す。公開設定項目は追加しない。
  Registryのコマンドをそのshellの`-c`に1引数で渡す。ホストでは実行しない。
  Test Sandboxのネットワークを禁止し、認証情報やGit管理領域は渡さない。
- `Prepare / OpenReport`は製品コードの`baseline_prepare.go`に実装した。
  HEADの照合後、前回のレポートだけを除去し、Sandbox終了後に新しい通常ファイルを読む。
  `os.Root`でworktreeの外への参照を拒否し、Git管理領域のパスやsymlinkのレポートも拒否する。
- JUnitのID規則は先行タスクと同じ`JUnitIDResolver`で注入する。
  baselineとGate 2で同じ変換器を使う。未指定なら件数だけを保存する。

## 仕様の穴と担当範囲

- 開始点でのテスト失敗・レポート欠落時の最終状態と分類は、Q11に記載がない。
  測定済みの結果は保存し、工程からローカルエラーを返す。
  公開のstatus・error_class・Block原因への変換は追加していない。
  未設定コマンドの`failed(config_error)`判定は仕様05どおりGate 2が担当する。
- Runner起動コマンドは従来どおり工程全体の接続待ちである。
  このタスクの基準値取得・HTTP保存経路は`BaselineHandler`で接続した。
  常駐Runner全体の工程接続は、このタスクの完了には含めない。

`runner/baseline_controlplane_test.go`では、製品のPrepare・OpenReportが実際に
生成されたレポートを読み、工程表・JUnit解析・認証付きHTTP・SQLite・成果物保存まで
通す。隔離ランタイムだけをテスト用のプロセスに置き換える。
continueを2回行い、HEADとRegistryが変わっても実行回数と基準値の行数が増えないこと、
成果物とresultの応答消失後に再送しても重複しないことを確認する。
worktree作成前に失敗したExecutionからのcontinueでは基準値を初めて保存し、
claimのworktree項目が欠ける場合には取得しないことも確認する。
cleanedからのcontinueでは、実際のlinked worktreeの削除とブランチからの復元を
テストの準備工程で行い、claim成功と保存済み基準値の再利用を確認する。
