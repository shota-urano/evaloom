# Artifactの保存

Control Planeは`NewArtifactStore(db, dataDir)`で保存先を指定する。
ローカルではリポジトリ直下の`data/`を使える。このディレクトリは
`.gitignore`の対象で、DB・ログ・artifactの実ファイルをGitに追加しない。
リポジトリ外の専用データディレクトリも指定できる。

`Save`には呼び出し側で発行したIDと、機密情報の除外処理を済ませた内容を渡す。
`redacted`はその処理を通したかを表す。保存処理自体は機密情報を除外しない。
生成したファイル名で`dataDir/artifacts/`に保存し、そのバイト列のSHA-256と
サイズをDBに記録する。`storage_path`は`dataDir`からの相対パスとする。
ファイルの書き込み・同期・クローズに成功してからDBに行を追加する。
書き込みやDBへの追加に失敗した場合は、その処理が作成したファイルを除去する。
プロセスの強制終了による未登録ファイルの回収は、この実装の対象に含めない。

`Open`はExecution IDとArtifact IDで対象を検索し、保存先ディレクトリ内の
ファイルを開く。呼び出し側は返されたファイルと`ArtifactStore`を閉じる。

JSON列への書き込みには`RecordGateResult`、`RecordExecutionBlock`、
`RecordAuditLog`を使う。Findingは型付き配列、detailはJSONオブジェクトとして
エンコードし、不正なJSONやエンコードできない値はDBへの書き込み前に拒否する。
ExecutionやTaskの状態遷移、Gate判定、本人の続行判断は各サービス側で扱う。
