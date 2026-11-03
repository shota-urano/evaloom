# セットアップ工程

仕様06の3.4を`SetupStage.Run`で実行する。工程3の`CopyWorktreeFiles`で
worktreeの登録が終わり、`worktree_ready`を送った後、工程6に登録する。

```go
handlers[StageSetup] = (SetupStage{
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

`Repository`を省略すると、RunnerはExecutionControlの認証付き結果送信を使う。
既存の`POST /api/executions/{id}/result`へ、次の内容を送る。

```json
{
  "fromStatus": "preparing",
  "status": "preparing",
  "worktreeId": "<記録済みのworktree ID>",
  "setupStatus": "succeeded または failed",
  "testRuns": [{"kind": "setup", "...": "既存のTestRunの各項目"}]
}
```

Control Planeは`store.SetupStore.SaveSetupTx`を呼び、test_runsの挿入と
worktrees.setup_statusの更新を同じトランザクションで行う。
測定値を保存した後、失敗時は既存の状態遷移報告でExecutionを終了する。
Runnerから直接DBを開かない。再利用時の成否は、Control Planeがclaimの
`worktree.setupStatus`で渡す。値が欠けている場合やworktreeが一致しない場合は
実行せずエラーにする。
worktreeの所属、開始点SHA、登録済みコマンドを検証する。
同じExecutionでは1回の測定を保存し、同じ記録の再送は成功、差し替えは拒否する。
前のExecutionが失敗した場合、新しいExecutionで同じworktreeのsetupを再実行できる。
終了コード0でもSandboxの後処理などが失敗した場合はsetup_statusをfailedにする。

仕様の穴と仮置きは次のとおり。

- 仕様06の工程結果報告と仕様02のsetup_statusに基づき、結果本文へ
  `setupStatus`、claimのworktreeへ`setupStatus`を追加した。
  仕様にはJSONの詳細が無いため、これを最小限の送受信形式とした。
  終了コード0でもSandboxの後処理が失敗する場合があるので、成否は別に送る。
  測定値は既存の`testRuns`を使い、新しいエンドポイント・テーブル・列は作らない。
  同じ本文で通信障害を再試行し、最新Executionでは終端状態になった後も
  保存済みの同一測定値だけ再送を受け付ける。
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
保存後の応答だけ失う場合も、測定値の再送は同じ本文となり、Sandboxを再実行しない。
APIのテストでは、Runner認証、所属・SHA・登録コマンド・成否の検証と、
setup_statusの更新失敗時にtest_runsの挿入も取り消すことを確認する。
