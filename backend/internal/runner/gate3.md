# Gate 3

`Runner.NewVerificationExecutor`は基準値取得、HEAD取得、Gate 2とGate 3を
既存の工程表へ接続する。単独で接続するときは`Runner.Gate3Handler`を
`StageGate3`に渡す。worktree準備、Harness、Publisherは各工程の実装を渡す。

Gate 3はSandboxの全プロセスが終了した後に`safegit.Output`を使い、
現在のHEADと`git status --porcelain=v1 -z --untracked-files=all`を読む。
HEADとverified_shaが一致し、同じExecutionのkind=verifyのtest_runsが
そのSHAを測定し、totalからskippedを引いた実行数が正のときだけ通す。
測定結果の欠落や全件skipもgate3_mismatchで止める。

コピー例外にはProjectの許可リストだけでなく、実際に置いたファイルの
`worktrees.copied_files`を使う。worktree準備工程は`Execution.CopiedFiles`へ
相対パスとSHA-256を渡す。既存worktreeではclaimのスナップショットも使う。
未追跡、またはindexに変更のない通常ファイルについて、元のハッシュと照合する。
ステージ済み変更、削除、rename、シンボリックリンクは除外しない。
NUL区切りを使うため、空白・改行を含むパスも個別に照合する。

仕様10の3.4.2で実装時の判断とされているテスト出力の特別な除外は設けない。
レポートなどもGitの変更として現れればBlockする。通常のgit statusと同じく、
Gitがignoreする未追跡ファイルは表示対象にならない。

Gate 3のtarget_shaには確認時点のHEADを保存する。HEADが変わった場合も、
harness_head_shaとverified_shaを置換せず、Blockのhead_shaと同じSHAを報告する。
既存result APIはこの対応を検証する。Gate結果、Block、イベント、通知要求、
状態変更は既存トランザクションで保存する。Blockでは後続工程を実行しない。

resume_point=gate3のclaimには、そのExecutionのharness_head_sha、verified_sha、
元のverify測定を載せる。worktreeには保存済みコピー記録とブランチ名を載せる。
既存claim APIの入力スナップショットを拡張し、新しいAPIやDB列は追加しない。
再開時はGate 3がこれらを読み戻し、再確認する。テストは再実行しない。
新しいmode=continueのExecutionには前のverify測定を引き継がず、
元のbaselineだけを再利用してGate 2を実行する。

## 未確定の境界

- Findingのrule一覧は仕様に無いため、`Gate3Config.MismatchRule`を必須とした。
  既定値は追加していない。
- cleaned worktreeの復元は仕様06の準備工程に属する。工程3を通らない再開では
  `Gate3Config.PrepareResume`に同じ準備処理を渡す。未接続でworktreeが無ければ
  検査エラーで止まり、公開しない。復元時のDB更新経路は先行工程と同じ境界に残る。
- `cmd/evaloom-runner`は先行タスクの時点で全工程の接続待ちである。
  本変更は製品の工程実行器までを接続する。常駐プロセス全体の接続は含めない。
- Gate 2でBlockedになった測定のverified_shaを続行時にどう扱うかは、
  後続のevaloom-pdy.6で扱う。本変更は検証SHAが無い場合の自動公開を許可しない。

結合テストでは隔離ランタイムと周辺工程を置き換え、実際のGit、工程実行器、
HTTP報告、SQLiteの保存を確認する。continueとGate 3の再開はRunnerを作り直し、
cleanedでは実際のlinked worktreeを削除・復元して確認する。
