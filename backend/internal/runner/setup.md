# セットアップ工程

仕様06の3.4を`SetupStage.Run`で実行する。工程3の`CopyWorktreeFiles`で
worktreeの登録が終わり、`worktree_ready`を送った後、工程6に登録する。

```go
handlers[StageSetup] = (SetupStage{
    Repository: setupRepository,
    NewLog: func(id string) (*Log, error) {
        return runner.NewLog(id, "setup", nil, knownSecrets...)
    },
    Prepare: prepareSetupSandbox,
}).Run
```

`Prepare`は`SetupRequest`の登録済みコマンドとworktreeを受け取り、
`sandbox.NewContainer`などで作ったSandbox、SandboxSpec、argvを返す。
渡されたstdout・stderrのwriterをContainerConfigへ設定する。
準備中にコマンドを実行してはならない。起動は工程側が`Sandbox.Run`で行う。
`Kind=test_setup`と同じworktreeを必須とし、Harnessの設定ディレクトリと
Harnessのネットワークポリシーは拒否する。環境変数とネットワーク宛先は
既存Sandboxの許可リストに従う。ホスト上で実行する代替処理はない。

セットアップコマンドが未設定または空白だけなら何もしない。
新規作成時は実行する。再利用時は保存済みsetup_statusがsucceededなら省略し、
pendingまたはfailedなら実行する。状態の取得に失敗した場合は起動しない。
再利用するworktreeの安全性確認は、仕様06の3.3を担当する工程の責務である。

終了を`WaitProcess`で確認し、kind=setup、target_sha=base_shaとして記録する。
通常の非ゼロ終了はfailed/config_error、Sandboxの作成・隔離・資源制限の失敗や
終了コード不明はfailed/sandbox_error、時間超過はtimed_out/timeout、
中止はcancelled/cancelledとして既存の結果APIへ報告する。
結果APIがworktreeをretainedにする。ディレクトリとブランチは削除しない。
タイムアウトや中止でも、独立したReportContextで記録とログの送信を続ける。

stdoutとstderrは既存のLog.Readで機密情報を除外し、setupのlogイベントと
setup_log成果物として既存APIへ送る。Sandboxが停止して両方の出力を読み終えてから
ログを閉じる。送信失敗の再試行はログの収集と並行して行い、コマンドを再実行しない。
保存失敗やログ失敗は呼び出し元へ返し、後続工程を開始させない。

`store.SetupStore`はControl Plane側の実装で、Runnerから直接DBを開かない。
test_runsの挿入とworktrees.setup_statusの更新を同じトランザクションで行う。
worktreeの所属、開始点SHA、登録済みコマンドを検証する。
同じExecutionでは1回の測定を保存し、同じ記録の再送は成功、差し替えは拒否する。
前のExecutionが失敗した場合、新しいExecutionで同じworktreeのsetupを再実行できる。
終了コード0でもSandboxの後処理などが失敗した場合はsetup_statusをfailedにする。

仕様の穴と仮置きは次のとおり。

- setupの測定値とsetup_statusをRunnerから送受信する要求形式が未定義である。
  `SetupRepository`で境界を分け、公開HTTP APIや要求フィールドを追加していない。
  実際の送信実装は仕様の確定後に接続する。保存の再試行は送信実装が同じ記録で行う。
- 文字列コマンドの解釈方式、イメージ、CPU・メモリの上限、環境変数の具体値、
  Projectごとの許可レジストリ一覧の取得経路は既存Sandboxと同じく外から渡す。
  `Prepare`がargvへ変換し、シェルや数値の既定値は追加していない。
  タイムアウトは既存ExecutionControlとProjectのsetupSecを使う。
- setup_statusにはtimed_outとcancelledが無いため、この2つはfailedとして保存する。
  再利用時に成功扱いで省略しないための仮置きである。
- 空白だけのコマンドは、既存のbaseline工程と同じく未設定として扱う。

`setup_test.go`はSandboxの起動境界だけを差し替え、実際のworktree作成・コピー、
結果報告とログのHTTP API、SQLiteを通して検証する。
ホストで実行するとカナリアファイルを作るコマンドを渡し、Sandboxへの引数と
ホストにファイルが無いことを確認する。失敗時のExecution、setup測定、
setup_status、機密情報除外済みの成果物とイベントをDBから読み戻す。
時間超過・中止時の強制停止、成功済みの再利用、保存の原子性と再送も検証する。
