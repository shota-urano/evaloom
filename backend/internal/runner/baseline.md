# 開始点のテスト基準値

`BaselineStage.Run`を`StageTestBaseline`のハンドラに渡す。
工程表に従い、setupの後、ハーネスの前に呼ぶ。
worktree準備側は`Execution.WorktreeID / WorktreePath / BaseSHA`を渡し、
Control Planeに同じworktreeと開始点SHAを記録しておく。

`BaselineRepository.FindBaseline`は、現在のExecutionから同じworktreeを使った
過去のExecutionまで検索する。保存済みならテスト、解析、ID保存を呼ばない。
元のtest_runsとartifactsを参照し、新しいExecutionへコピーしない。
freshで別のworktreeを作った場合は、同じ開始点SHAでも新しく取得する。

新規取得ではRegistryのコマンド・レポート設定と開始点SHAを
`BaselineTestSandbox.RunBaseline`へ渡す。`SandboxBaseline`は既存の
`sandbox.Sandbox`と`Execution.WaitProcess`を使い、`kind=test_run`で実行する。
開始点とHEADの一致確認は信頼する起動側が行う。continueで基準値が無いときも、
変更後のHEADを開始点として記録したり、worktreeをresetしたりしない。
レポートは今回の実行で作られたものだけを返す。

解析には先行タスクの`gate.ParseJUnitXML`を利用できる。
取得した件数を`model.TestRun`へ移し、IDが取得可能なら`RecordIDs`で
`test_ids`成果物を保存して、そのIDをtest_runsから参照する。
IDが取得できない場合も件数を保存する。欠落・不正レポートの件数はNULLにする。
保存を確認してからハーネスへ進む。保存・検索の失敗を未取得として扱わない。

Control Plane側の`store.BaselineStore`は、既存のtest_runsへ保存する。
worktreeとExecutionのTask・Project・Runner・開始点を確認し、トランザクションで
重複登録と上書きを防ぐ。同じレコードの再送は受け付ける。
Runnerに管理DBを開かせる実装は追加していない。

## 仕様の穴と接続待ちの箇所

- 仕様05は、基準値の報告本文と、過去のworktreeの基準値をRunnerが取得する
  通信契約を定めていない。`BaselineRepository`で分離した。
  公開API・テーブル・列は追加していない。HTTPへの接続は未実装。
- コマンド文字列からSandboxのargvへの変換、開始点SHAを指定する起動処理、
  古いレポートを使わずに読む方法は未定義である。
  `SandboxBaseline.Prepare / OpenReport`で分離した。信頼する呼び出し側から
  Sandboxの設定、argv、今回のレポートを読む処理を渡す。製品設定との接続は未実装。
- JUnitのID規則は先行タスクと同じ`JUnitIDResolver`で注入する。
  ID成果物の符号化も未定義なので`RecordIDs`で分離した。
  製品用の既定ID変換・成果物形式は追加していない。
- 開始点でのテスト失敗・レポート欠落時の最終状態と分類は、Q11に記載がない。
  測定済みの結果は保存し、工程からローカルエラーを返す。
  公開のstatus・error_class・Block原因への変換は追加していない。
  未設定コマンドの`failed(config_error)`判定は仕様05どおりGate 2が担当する。
- Runner起動コマンドは従来どおり工程全体の接続待ちである。
  この変更だけで常駐Runnerが基準値を取得できるとは扱わない。

`store/baselines_test.go`では、Sandbox境界だけをfixtureに置き換え、
実際の工程表、JUnit解析器、SQLite、成果物保存を通して検証する。
テスト内のname属性によるID変換とJSON形式は製品仕様には採用していない。
