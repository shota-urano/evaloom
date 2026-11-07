# 起動コマンドと工程表の接続

`cmd/evaloom-runner`は、claimごとに`PipelineDependenciesForClaim`で依存を組み立て、
`Runner.ExecuteClaim`へ渡す。仕様05の13工程には既存のRegistry、Worktree、Context、
Sandbox、Adapter、Gate、Publisher、ログ・使用量の部品を使う。
管理DBの更新はControl Planeだけが行う。

## 入力と記録の経路

| 処理 | 接続先 |
| --- | --- |
| Projectの再検証、clone・fetch・リモートブランチの確認 | `publisher.Reader`が工程ごとに読み取り専用トークンを発行し、RegistryとWorktreeへ渡す |
| 前回のPR | claimの`pullRequests`を読み取り用トークンで確認する。freshではマージ済みを優先して拒否し、開いているPRがあればworktree作成前に停止する |
| freshの番号 | claimの`execution.freshNumber`を使う。Executionのseqで代用しない |
| worktreeとコピー一覧 | resultの`worktree`報告で登録し、保存の応答を待つ。再利用の照合にはclaimのstatus・expectedHeadSha・preparationCompleteを使う |
| パス拒否、push後のSHA不一致 | failedのresultに`audit`を付ける |
| Context | claimのharnessRepoPathとProfileのcommit SHAから設定を固定し、既存context APIへ送る |
| Sandbox | 設定のruntime・image・proxy・CPU・メモリを使う。setupはclaimのsetupAllowedHostsだけを許可し、基準値とテストはネットワークを遮断する |
| ハーネス | CodexとClaude Codeに、専用設定環境、harnessAuthFiles、claimのtoolPermissionsを渡す。許可宛先とCLI権限への変換は各Adapterが担当する |
| セッションと作業概要 | runningを抜けるresultにharnessSessionIdを載せる。工程9でHarnessResultをharness_output成果物として登録する |
| pushとPR | publishingのままpushedShaをresultで保存し、応答後にPRを作成する。その後、PR情報をresultで保存する |
| Blockからの続行 | gate3から再開し、claimのresumeBlock・resumeGateResults・resumeHarnessSummaryを使う。判断時のHEADを再確認し、PR本文にBlockの内容を載せる |
| 使用量と完了 | 工程13で実行時間とAdapterの使用量をsucceededのresultへ載せる |

工程の期限はProjectのtimeoutsを優先し、省略した項目には設定のdefaultTimeoutsを使う。
基準値とGate 2にはそれぞれ独立した期限を設け、Sandboxには残り時間を渡す。
キャンセルの猶予とruntime制御コマンドの上限は各5秒の実装定数とする。
Gitの作者情報はcommitAuthorから実行プロセス専用のGit設定環境変数で渡す。

失敗した工程の後は実行せず、作成済みworktreeを保持する。
工程9のdiffとharness_outputは、登録に失敗しても保存済みのファイルとartifactIdを使って再送する。
登録成功の応答を受けてから次の工程へ進み、再送待ちもキャンセルで停止する。
パス拒否にはconfig_errorと監査を記録し、push直前の中止確認に失敗した場合は
publish_failedを報告する。認証設定やclaimの必須情報が無ければ成功扱いにしない。

## 結合テスト

`pipeline_controlplane_test.go`は、起動コマンドと同じ`PipelineDependenciesForClaim`を使い、
実際のControl Planeハンドラ・認証・SQLite・Gitでclaimからsucceededまで確認する。
置き換えるのはコンテナruntime、adapter/dummy、GitHubのHTTP接続先とトークン発行だけである。
Gitのテスト用起動スクリプトはGitHubの接続先とTLS証明書をテストサーバーへ向け、
実際のGitでローカルbareリポジトリへのclone・fetch・push・SHAの読み戻しを行う。
永続化の記録実装やSandbox自体の代替実装は渡さない。

| 確認する動作 | テスト |
| --- | --- |
| 13工程、worktreeとコピーのハッシュ、各イベント、Executionの全記録、usage、PR作成前のpushedSha保存、初回freshの-r1、読み取りと書き込みのトークン権限 | `TestProductionPipelineClaimPushAndSQLite` |
| harness_outputの初回503と保存後の応答喪失、同じID・内容での再送、重複行を作らずpush・succeededまで進むこと | `TestProductionPipelineRetriesHarnessOutput` |
| 工程途中の失敗、worktree保持、認証設定不足、パス逸脱の監査、ProjectとdefaultTimeoutsの期限 | `TestProductionPipelineStopsAndRetainsWorktree` |
| 開いている前回PRがあるfreshの停止 | `TestProductionPipelineFreshOpenPRBlocksBeforeWorktree` |
| Gate 3からの再開、ハーネス・テストを再実行しないこと、PR本文のBlock原因 | `TestProductionPipelineResumeGate3UsesSavedBlock` |
| 再開時のパス逸脱・Git領域の逸脱と監査、判断後のHEAD変更による再停止 | `TestProductionPipelineResumeRejectsChangedWorktree` |
| push直前の中止確認と通信失敗の終端報告 | `TestControlledExecutorChecksCancellationBeforePublishAndFailsClosed` |
| setup・ハーネス・基準値・Gate 2の期限とProject優先 | `TestStageTimeoutsProjectOverridesAndIndependentDeadlines` |
| 起動表示、claimから工程表への受け渡し、認証設定不足の報告 | `cmd/evaloom-runner`の`TestRunWaitsUntilShutdown`・`TestRunExecutesClaimAndReportsMissingHarnessAuthentication` |

`pipeline_test.go`には工程単体の差し替えによる追加テストも残している。
本番依存の接続とDB保存の証拠は、上記のProductionPipelineテストで確認する。
実コンテナ、実モデル、実GitHubへの接続は、この結合テストでは実行しない。
