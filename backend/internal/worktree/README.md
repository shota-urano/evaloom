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
Base Branchの選択や開始点SHAの記録、タスクのworktree作成は後続タスクが担当する。

Projectごとのファイルロックで、複数の準備処理による二重cloneを防ぐ。
呼び出し元は、同じProjectのSandboxの書き込みプロセスを停止してから準備する。
ロックはSandboxの書き込みを停止する機能を持たない。
既存の不正なディレクトリや登録URLと異なるoriginを検出した場合は、変更せずにエラーを返す。

## 読み取り用認証の境界

仕様06の3.2はTrusted Runnerの読み取り用認証を要求するが、渡し方は定めていない。
cloneとfetchだけを`ReadTransport`に分け、後続の認証実装を差し込めるようにした。
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
