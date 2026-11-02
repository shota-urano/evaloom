# HarnessResultの組み立て

`Runner.HarnessResultHandler`を工程9の`StageHarnessHead`へ渡す。
`NewGate2Executor`と`NewVerificationExecutor`はこのハンドラーを組み込む。
工程8はSandbox内の全プロセスの終了を待ち、Adapterの結果を
`Execution.HarnessResult`へ渡す。失敗結果もGit情報を集めるために渡す。

Runnerは専用のGit実行処理`safegit`を使い、worktreeのHEADを取得する。
開始点からそのHEADまでの`rev-list`と`diff`を取得し、CLIが返した
`headSha`、`commits`、`diffArtifactId`を置き換える。
HEADをSHAに固定して同じcommitの一覧と差分を作る。
未commitの変更、ステージ済みの変更、未追跡ファイルは差分に含めない。
CLIのsummary、status、errorClass、sessionId、usageは保持する。

差分はRunnerのデータディレクトリ内の`artifacts/<ULID>.diff`へ保存する。
ファイルを同期して閉じた後、既存のartifact登録APIへ`kind=diff`、
`redacted=0`で送る。ログの除外処理は差分に適用しない。
Control Planeが自身のデータディレクトリへ保存し、ハッシュとサイズを計算する。
保存確認後に`diffArtifactId`と`HarnessHeadSHA`を設定する。
送信失敗時は同じIDとファイルで再送し、取得済みのGit情報も保持する。
配送のcontextを中止してもファイルを残し、同じExecutionで再送できる。
再起動後の自動配送は既存のログ配送と同様に対象外である。

Adapterが失敗を返した場合も差分保存後に後続工程を止める。
制御付き実行器では既存のresult APIでfailedを報告し、AdapterのerrorClassを優先する。
errorClassが無ければ仕様05のexecution_failedを使う。
Git取得やローカル保存の失敗ではエラーを返し、Gateとpublishへ進めない。
Gitの生の診断文は返さない。

## 仕様の穴と接続範囲

- コミット0件時の扱いはQ31の案のままである。この処理では空の配列と空のdiffを
  保存する。新しい失敗条件は確定しない。
- Git取得・ローカル差分保存に失敗した場合のExecutionのerrorClassは仕様に無い。
  工程エラーとして呼び出し側へ返し、独自の分類は追加しない。
- 共通契約にはdiffArtifactIdとcommitsがあるが、Executionの保存テーブルとresult APIに
  対応する項目は無い。組み立てた結果はExecution内で保持し、diffは既存artifactsに保存する。
  API、テーブル、列は追加していない。
- 常駐Runnerの起動コマンドは既存どおり全工程の接続待ちである。
  Adapter本体、Sandboxの起動、worktree準備、Git Publisherは変更しない。
