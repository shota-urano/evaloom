# 作業用クローンの準備

Runnerのworktree準備処理から、次の内部関数を呼ぶ。

```go
repositories := worktree.Repositories{WorktreeRoot: config.WorktreeRoot}
repository, err := repositories.Prepare(ctx, project.ID, project.GitRemoteURL)
```

戻り値はシンボリックリンクを解決した絶対パスである。後続のBase Branch取得や
`worktree add`は、このパスに対して`safegit.Output`で実行する。
このパッケージはHTTP APIやDBの列を追加しない。Local Repository Pathも受け取らない。

## 配置

仕様06の3.2.2はクローンのディレクトリ名を定めていないため、
`<Worktree Root>/.evaloom-repositories/<Project ID>`を選んだ。
タスクのworktreeを置く`<Worktree Root>/<task-id>[-r<n>]`と階層を分けるためである。

初回は同じ格納先の一時ディレクトリに`clone --no-checkout --no-local`で作り、
検証後にProjectのパスへ移す。2回目以降は同じクローンでfetchする。
fetch先は登録済みURL、更新先は`refs/remotes/origin/*`に固定する。
fetch前に`safegit.ValidateFetchURL`で、登録URLと同名のremote設定、
`url.*.insteadOf`、`fetch.bundleURI`を拒否する。include、条件不成立のincludeIf、
有効なconfig.worktreeも検査する。旧形式の`.git/remotes`と`.git/branches`も拒否する。
これらはハーネスが取得元を変更するために使えるため、未使用の設定も拒否する。
Base Branchの取得と開始点SHAの記録は下記の関数を使う。
タスクのworktree作成は後述の`CreateWorktree`を使う。

## Base Branchの取得と記録

新規worktreeを準備する工程3で、`Prepare`の成功後に呼ぶ。

```go
if err := execution.RecordRemoteBase(ctx, repositories); err != nil {
    return err
}
// execution.BaseSHAを開始点として、後続のworktree作成処理へ渡す。
```

`Repositories.FetchBase`はProjectごとのロックを取り、既存クローンのパスと
登録済みURLを再検証する。登録済みBase Branchだけを明示したrefspecでfetchし、
`refs/remotes/origin/<Base Branch>`をcommitに解決する。
ローカルのHEADやFETCH_HEADは開始点として使わない。
リモートでブランチが削除済みなら、古い追跡refが残っていてもエラーにする。
fetch失敗はRunnerから`failed/config_error`として報告し、Gitのエラー本文は返さない。

`Execution.RecordRemoteBase`は既存の結果報告APIへ`baseSha`を送り、
Control Planeが`executions.base_sha`へ保存した応答を受けてから`Execution.BaseSHA`を設定する。
Context固定より前に保存するため、`preparing`から`preparing`へのSHAだけの報告を
既存のbaseline測定報告と同じ方式で受け付ける。同じSHAの再送は成功し、
記録済みSHAの変更は既存の`precondition_failed`（422）で拒否する。状態遷移、イベントや通知は発生させない。
結果報告の本文と工程内の報告時点は仕様で未定義のため、既存のフィールドと
エンドポイントの範囲でこの方式にした。新しいHTTP API、テーブルや列は追加していない。

`BaseFetcher`は取得処理の内部境界で、実装には`Repositories`を渡す。
`continue`で以前のworktreeがある場合はこの関数を呼ばず、再利用処理で保存済みSHAを引き継ぐ。
worktreeの作成とブランチ命名は次節の関数が担当する。
`worktrees.base_sha`を含む行の作成は、コピー処理後の記録処理が担当する。
読み取り用認証は引き続き`ReadTransport`の境界に残し、供給方式の仮定は追加していない。

Projectごとのファイルロックで、複数の準備処理による二重cloneを防ぐ。
呼び出し元は、同じProjectのSandboxの書き込みプロセスを停止してから準備する。
ロックはSandboxの書き込みを停止する機能を持たない。
既存の不正なディレクトリや登録URLと異なるoriginを検出した場合は、変更せずにエラーを返す。

## 新規worktreeの作成

`RecordRemoteBase`で開始点SHAを保存した後に呼ぶ。

```go
// 通常作成ではfreshNumber=0。freshではタスク内のfresh通し番号を渡す。
if err := execution.CreateWorktree(ctx, repositories, freshNumber, pathAuditor); err != nil {
    return err
}
// execution.WorktreePathとBranchNameを、コピーと記録の工程へ渡す。
```

`Repositories.Create`は作業用クローンを再検証し、Projectごとのロックを取る。
通常は`evaloom/<task-id>-<shortName>`と`<Worktree Root>/<task-id>`を使う。
freshでは両方に`-r<n>`を付ける。保存済みBaseSHAのcommitから、
`safegit.Output`を通した`worktree add --no-track -b`で作成する。
本人のローカルリポジトリは入力にも変更対象にも含めない。

同名のローカルブランチと、登録URLへの`ls-remote`で見つかるリモートブランチを拒否する。
追跡refがまだ無いリモートブランチも対象にする。既存のrefを更新・削除しない。
取得先や作成に失敗した場合は、Runnerが既存の結果APIへ`failed/config_error`を報告する。
選んだ`branchName`は、既存の結果APIの同名フィールドで次の状態遷移時に保存する。

Worktree Rootを実パスへ解決してから、タスク用ディレクトリを排他的に作る。
既存の空ディレクトリも再利用しない。作成前後で実パスの包含を確認し、
シンボリックリンクなどによるパスの安全性違反は`ErrWorktreePath`で返す。
Runnerは`WorktreePathAuditor`へ拒否の記録を要求してから失敗を報告する。
失敗時に作成済みのディレクトリやブランチを自動削除しない。
呼び出し元はWorktree Rootを書き換えるプロセスも停止しておく。

仕様の未確定部分と接続境界は次のとおり。

- freshの命名はQ22の案を採用した仮置きである。`n`は1からのfresh通し番号として外から渡す。
  `executions.seq`にはcontinueも含まれるため、代用しない。claimに番号や実行履歴が無く、
  正確な採番の取得経路は未定義なので、この実装では採番しない。
- パス拒否をRunnerから`audit_logs`へ送る要求形式は未定義である。
  必須の`WorktreePathAuditor`を内部境界とし、HTTP APIや列を追加していない。
  実際の監査送信は呼び出し元の実装が必要である。監査失敗も成功として扱わない。
- このタスクは仕様06の3.2の手順3〜4を担当する。コピー、worktrees行の作成、
  worktree_ready、setup、continueでの再利用は後続の工程で行う。
  worktrees行をRunnerから登録する要求形式も未定義であり、ここではAPIを追加しない。

## 読み取り用認証の境界

仕様06の3.2はTrusted Runnerの読み取り用認証を要求するが、渡し方は定めていない。
clone、fetch、ls-remoteを`ReadTransport`に分け、後続の認証実装を差し込めるようにした。
既定の実装は`safegit.Output`を使い、追加の認証情報を渡さない。
認証が必要なHTTPSリポジトリへの独自の認証供給は、このタスクでは実装していない。
SSHではsafegitが許可する信頼済みSSH設定やagentを引き続き利用する。

認証の実装はsafegitの防御を維持し、credential helperやaskpassを有効にしてはならない。
秘密情報をクローンのconfig、URL、worktreeに保存しない。
HTTPのuserinfoやSSH URLのパスワードを含むURLは拒否する。
ローカルパス、file URL、外部transport helperもクローン元として拒否する。
Gitのエラー本文やstderrは認証情報を含む可能性があるため返さない。

## 検証

テストは使い捨てのbareリモートをHTTPで公開し、実際のGitでcloneとfetchを行う。
新commitの取得、Gitディレクトリの再利用、Projectごとの分離、本人用リポジトリの
作業ツリー・index・ref・config・オブジェクトの不変を確認する。
パスのシンボリックリンク、共有オブジェクト、originの変更、fetch先の変更設定、失敗後の再試行、
並行準備とロック待ちのキャンセルも確認する。
`create_test.go`は通常とfreshのブランチ名、実パス、同名のloose/packedブランチと
fetch後に作られたリモートブランチの拒否、既存SHAの不変、並行作成、
既存パスとシンボリックリンクの拒否、checkout時のフック無効化を実gitで確認する。
Runnerの`worktree_test.go`は、既存HTTP APIを通した`failed/config_error`の保存と
`branchName`の保存も確認する。未定義の監査送信だけはテスト用の実装を渡す。
