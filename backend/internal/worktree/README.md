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
// execution.WorktreePathとBranchNameを、後述のCopyWorktreeFilesへ渡す。
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

## コピー許可リストとハッシュ記録

`RecordRemoteBase`と`CreateWorktree`の成功後、同じ工程3で呼ぶ。

```go
if err := execution.CopyWorktreeFiles(ctx, repositories, copyRecorder); err != nil {
    return err
}
// 保存の応答後にExecution.WorktreeIDとCopiedFilesが設定される。
// 保存処理がworktree_readyを記録した後、後続の工程へ進む。
```

`Repositories.CopyFiles`はLocal Repository Pathを読み取り元とし、許可リストに
列挙された通常ファイルだけを同じ相対パスへ置く。ディレクトリの再帰コピーや
ワイルドカード展開はしない。許可リスト外の`.env`は読まない。
許可リストに明示された`.env`は仕様03の3.3に従ってコピー対象になるため、
テストではダミー値だけを使う。

コピー元とコピー先の各パス要素を`openat`の`O_NOFOLLOW`で開く。
コピー元のリンクは内部・外部・リンク切れを問わずスキップし、
親ディレクトリのリンクもたどらない。通常ファイル以外もスキップする。
コピー元が無ければ`WorktreeCopyRecorder.WarnMissingCopy`の成功を待って続ける。
警告を保存できなければ成功にしない。相対パスは正規化して重複を除き、
コピー先へ書いた内容のSHA-256を`model.CopiedFile`へ記録する。
SandboxとWorktree Rootを書き換えるプロセスは、保存が終わるまで停止しておく。

コピー先に通常ファイルがある場合も、追跡の有無を問わずコピー元の内容で置き換える。
同じ親ディレクトリ内の一時ファイルへ書き終えてから`renameat`で置き換え、
既存のシンボリックリンクやディレクトリはエラーにする。
親ディレクトリのリンクをたどらず、ハードリンク先のファイルも変更しない。
途中で失敗した場合、置き換え前のファイルと、それまでにコピーしたファイルは保持する。
その場合は完成したコピー一覧として保存しない。

`store.WorktreeCopyStore`はControl Plane側の保存処理である。
`worktrees`の行と`copied_files`、Executionへの関連付けを同じトランザクションで保存する。
空の一覧は`[]`として保存する。`worktree_ready`も同じトランザクションで記録する。
同じ記録の再送は成功し、内容の差し替えは拒否する。
`WarnMissingCopy`は既存の`execution_events`へ`log`警告を保存する。
RunnerからこのStoreを直接使わず、`WorktreeCopyRecorder`の送信実装を外から渡す。

仕様の穴と仮置きは次のとおり。

- worktree登録の要求本文が未定義のため、HTTPの要求形式は追加していない。
  保存と警告の送信は内部インターフェースで区切った。実際の送信実装は仕様の確定後に接続する。
- 仕様06に分類の無いコピーI/Oエラーと警告保存失敗は、仮に`failed/config_error`として報告する。
  記録の送信失敗では先のコピーを繰り返さず、成功したように後続工程へ進めない。
- 仕様03の相対パス検証案に従い、`..`、絶対パス、ワイルドカードを拒否する。
  `.git`を含むパスもGit管理領域への書き込みを防ぐため拒否する。
  新しいディレクトリは既存の準備処理と同じ`0700`、ファイルはコピー元の通常権限を使う。

`copy_test.go`は実ファイルを使って除外、リンク拒否、既存の通常ファイルの置き換えを検証する。
Runnerの`TestCopyWorktreeFilesPersistsPathsAndActualSHA256`は実際にfetch・作成・コピーを行い、
SQLiteに保存した相対パスとSHA-256を実ファイルと照合する。
同じ記録をGate 3へ渡し、コピー直後は除外され、変更後は除外されないことも確認する。
`TestCopyWorktreeFilesReplacesTrackedFileAndPersistsSHA256`ではBase Branchの追跡済みファイルを
本人のローカルの内容で置き換え、保存したSHA-256とGate 3の除外を確認する。

## continueでの再利用

claimに`worktree`がある通常のcontinueでは、工程3で新規作成の代わりに呼ぶ。

```go
if err := execution.ReuseWorktree(ctx, repositories, reuseRepository, pathAuditor); err != nil {
    return err
}
// 成功の応答後、Context固定とsetupへ進む。
```

`Repositories.Reuse`は作業用クローンとworktreeの実パス、`.git`の参照先を検証する。
チェックアウト中のブランチ、未コミットの変更、前回の既知のSHAとの一致を確認する。
コピーしたファイルの除外はGate 3と同じ`gate.CheckWorktree`を使う。
記録した内容と一致する未追跡ファイルと未ステージのコピーだけを除外し、
変更済みのコピー、ステージ済みの変更、削除、renameは除外しない。

未コミットの変更、ブランチやHEADの不一致は、既存の認証付きresult APIで
`blocked/git_state`として記録する。Gitの状態を読めない場合も安全側で停止する。
現在のHEADを読めない場合は前回の既知のSHAを参照値として送り、現在のHEADは
未確認とerrorMessageに明記する。パス拒否は既存の監査境界を呼び、
`failed/config_error`とする。Gitや認証情報を含みうる生のエラー本文は送らない。

ディレクトリが無い場合は、残っているローカルブランチのSHAを確認してから
同じパスにworktreeを作り直す。手動削除でGitの登録だけが残った場合は、
その存在しないパスの登録だけを`worktree remove`で解除する。
全体のpruneや`--force`は使わず、ブランチが別のworktreeで使われていれば失敗する。
再利用ではfetch、rebase、mergeを行わず、ブランチのSHAと開始点SHAを変えない。
Gitがディレクトリを作り直す前に、`BeginWorktreeRestore`が復元開始の`log`イベントを
記録し、以前のコピー一覧を空にしてsetupをpendingに戻す。復元開始は既存の
`sourceId=worktree-restore/<worktree-id>`、`stream=runner`、`text`で表す。
再作成した場合だけ既存のコピー処理を呼び、コピーがすべて終わったときに限り、
コピー一覧と`worktree_ready`を同じトランザクションで保存する。途中でキャンセル・
失敗した場合は、完了した一覧として保存しない。
普通の再利用ではコピー元を読み直さず、成功済みのsetupを引き継ぐ。

再利用前に、最後の復元開始より後の`worktree_ready`とsetupの完了を確かめる。
イベントのパス・ブランチ・開始点SHAと所属が保存済みworktreeに一致し、
`setup_status=succeeded`である場合だけ準備完了とする。setupが未設定で
`setup_status=pending`なら、その準備時点の実行不要を示す`log`イベントも確認する。
この記録は`worktree_ready`と同じトランザクションで保存し、sourceIdを
`worktree-setup-not-required/<worktree-id>`とする。現在のコマンドが空になったことだけでは、
前回のセットアップを完了扱いにしない。
古いExecutionの遅延イベントでは、後のExecutionで始まった復元を完了扱いにしない。
準備完了の証拠が無い場合、またはcleanedの行に実ディレクトリが残っている場合は、
コピー・setup・復元を自動で繰り返さず`blocked/git_state`で停止する。

`continue`のHTTP retryは、新しいpending Executionの作成時に、前回のworktree_id・
base_sha・branch_nameと最後の既知SHAを引き継ぐ。最後の既知SHAは、pushed_sha、
harness_head_sha、base_shaの順に選び、新しいExecutionのharness_head_shaへ保存する。
verified_shaとpushed_shaは引き継がず、新しい実行の測定結果と混同しない。
Runnerも再利用の照合前に同じ関連付けとSHAを保持し、blocked・cancelled・failedの
どの結果にも付ける。想定外のHEADはBlockのhead_shaへ記録し、既知SHAは変更しない。
このため、準備前の中止でも、次のretryがworktreeを新規作成しようとすることはない。

成功時は既存のresult APIへ、preparingのまま`worktreeId`、`branchName`、`baseSha`を送る。
Control Planeは直前のExecutionの所属、ブランチ、開始点との一致を確認し、
コピー完了のイベントも検証し、関連付けとworktrees.status=active、
cleaned_atの解除を同じトランザクションで保存する。
同じ報告の再送は成功し、別のworktreeや開始点への差し替えは拒否する。
新しいHTTPエンドポイント、要求フィールド、テーブル、列は追加していない。

仕様の穴と仮置きは次のとおり。

- claimには前回のpushed_sha、harness_head_sha、worktreeのstatusが含まれていない。
  `WorktreeReuseRepository.LoadWorktreeReuse`を内部の取得境界にした。
  Control Plane側の`store.WorktreeReuseStore`が、直前のExecutionから
  pushed_sha、harness_head_sha、base_shaの順で参照値を選ぶ。
  Runnerからの取得形式が確定するまでは送信実装を外から渡す必要があり、
  取得できない場合は準備を進めない。Runnerは管理DBを開かない。
- 再作成時のコピー一覧の送信形式も未定義なので、既存の新規コピー登録と同じく
  内部境界`RecordRestoredCopies`で区切った。Control Plane側の実装は保存済みの
  worktreeにコピー一覧とsetup_status=pending、worktree_readyを保存する。
  復元開始の送信形式も未定義であり、`BeginWorktreeRestore`の内部境界に残す。
  準備記録は既存のexecution_eventsとsetup_statusで表し、列は追加しない。
- 再利用の工程内報告は、既存の開始点報告と同じpreparingからpreparingへの
  測定報告として扱う。仕様はresult本文の詳細を定めていない。
- ブランチの不一致・消失、Git状態の読み取り失敗の分類は表に明記されていないため、
  仮にgit_stateとして停止する。再作成やコピーのI/O失敗は既存作成処理に合わせて
  failed/config_errorとする。監査の送信形式は引き続き既存の内部境界に残す。

`runner/worktree_reuse_test.go`は実Gitでworktreeを作り、認証付きHTTPで報告して
SQLiteから結果を読み戻す。未コミット変更の停止、コピーの除外、削除後の復元、
前回の追加commitと開始点SHAの保持、SHAの優先順、ブランチの不一致、
不正な結果報告の拒否、パス拒否、取得・保存失敗時の停止を確認する。
復元開始直後、復元後、コピー後、部分コピー中のキャンセルからHTTP retryとclaimを行い、
関連付けと追加commitのSHAを保持したままblockedになることも確認する。
ファイル内容、保存したSHA-256、setup_status、準備記録の欠落と古い遅延イベントを検証する。

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
