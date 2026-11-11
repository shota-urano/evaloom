# Artifactの保存

Control Planeは`NewArtifactStore(db, dataDir)`で保存先を指定する。
ローカルではリポジトリ直下の`data/`を使える。このディレクトリは
`.gitignore`の対象で、DB・ログ・artifactの実ファイルをGitに追加しない。
リポジトリ外の専用データディレクトリも指定できる。

`Save`には`Artifact`と、機密情報の除外処理を済ませた内容を渡す。
`Artifact`には呼び出し側で発行したID、Execution IDか振り分け提案IDの一方、
段、種別、表示名を指定する。表示名はファイルの保存パスとして使わない。
`redacted`はその処理を通したかを表す。保存処理自体は機密情報を除外しない。
生成したファイル名で`dataDir/artifacts/`に保存し、そのバイト列のSHA-256と
サイズをDBに記録する。`storage_path`は`dataDir`からの相対パスとする。
ファイルの書き込み・同期・クローズに成功してからDBに行を追加する。
書き込みやDBへの追加に失敗した場合は、その処理が作成したファイルを除去する。
プロセスの強制終了による未登録ファイルの回収は、この実装の対象に含めない。

`Open`はExecution IDとArtifact IDで対象を検索し、保存先ディレクトリ内の
ファイルを開く。呼び出し側は返されたファイルと`ArtifactStore`を閉じる。

Runnerの登録APIは所有権を確認したトランザクションと`Artifact`を`RegisterTx`へ渡す。
クエリの`stage`・`kind`・`name`は仕様05の3.8.1に従う。
同じExecution・ID・SHA-256の再送は既存行を返し、受信時に作ったファイルを除去する。
別のExecutionや異なるSHA-256へのIDの再利用は`ErrArtifactConflict`を返す。
振り分け提案IDへのIDの再利用にも同じ規則を適用する。
呼び出し側はcommitが失敗した場合、返されたcleanupを呼んで新規ファイルを除去する。

監査記録のJSON列への書き込みには`RecordAuditLog`を使う。detailはJSONオブジェクトとして
エンコードし、不正なJSONやエンコードできない値はDBへの書き込み前に拒否する。
ExecutionやTaskの状態遷移は各サービス側で扱う。
`RecordUsage`には測定した段を指定する。トークン数と費用が不明ならNULLを保存する。
旧形式のExecution全体の時間を、段の実行時間として保存しない。

`HarnessRecordsForNextWork`は同じハーネスの`deleted_at IS NULL`の行を新しい順に返す。
上限にはProfileの`records.pass.recent`を渡す。削除済みの行を除いてから上限を適用する。
呼び出し側は文脈を固定するトランザクションを渡せる。既に固定した記録は変更しない。
Gate結果とBlockの記録（`gate_results`・`execution_blocks`）は software-engineer への切り替えで消した
（`docs/specs/19-routing-and-stages.md` 3.11、マイグレーション0025）。
