# Execution Contextの保存API

正本は`docs/specs/07-context.md`の3.2〜3.5・5・6節と、
`02-data-model.md`のcontext_revisions・5.3・5.7節。

`POST /api/executions/{id}/context`はRunner認証を使う。本人のセッションは403、
無効なBearerは401で拒否する。別Runnerに属するExecutionと未claimのExecutionは
403で拒否し、既存の認証拒否処理でaudit_logsに記録する。
存在しないExecutionは404。保存できるのは最新Executionのpreparing中だけである。
所有権確認と保存には同じDBトランザクションを使う。

要求形式は次のとおり。空のファイル一覧も両方を明示する。
既存の`projectcontext.Read`と`harnesscontext.Read`が返すFileをそのままJSON化できる。
`content`は元のバイト列をbase64にした文字列とする。FileのJSON処理が変換と復号を行い、
不正なUTF-8を含む内容も変えずに転送する。ハッシュは復号した元のバイト列で照合する。

```json
{
  "project": [
    {"path":"AGENTS.md","content":"<base64>","contentHash":"<SHA-256>","sourceCommitSha":"<base_sha>"}
  ],
  "harness": [
    {"path":"prompts/developer.md","content":"<base64>","contentHash":"<SHA-256>","sourceCommitSha":"<profile_commit_sha>"}
  ]
}
```

APIは相対パス・重複・内容のSHA-256・Executionに保存済みのSHAとの一致を検証する。
不正な要求は400で拒否し、行を保存しない。ハーネスのタグや作業ツリーは読み直さない。
Harnessのファイル選択は既存の読み込み処理と同様に呼び出し側が明示する。
Profileスキーマにファイル指定欄が無いため、版からパスを推測しない。
Profile定義による選択と構造的矛盾のblocked化はContext Builderの別実装単位で扱う。
RunnerでのHarness読み込み失敗は既存のConfigErrorによってfailed(config_error)として報告する。

GlobalはControl Plane起動時の`-context-root <evaloomルート>`と、繰り返し指定できる
`-global-context context/<相対パス>`で選ぶ。指定が無ければGlobalファイルを読まない。
ディレクトリ全体を暗黙に収集しない。選択はcontext配下の相対パスに制限する。
読み取り時点のevaloomリポジトリのHEADを完全なcommit SHAに一度だけ解決し、
既存のgitcontext.Readで、そのcommitの通常ファイルだけを取得する。作業ツリーは読まない。
全Global行のsource_commit_shaには、この解決済みSHAを保存する。
選択したファイルが読めない場合は、TaskとExecutionをfailed、error_classをconfig_errorにして
status_changedイベントを同時に保存し、422を返す。context_revisionsは保存しない。

Taskは保存済みのtitle・body・acceptanceCriteriaの3項目を、この順のJSONオブジェクトとして
1行に保存する。kindはtask_input、source_pathは空文字とする。
仕様にTaskのsource_commit_shaの値の定義が無いため、そのExecutionの開始点commitである
base_shaを採用する。Taskの内容自体はDBの入力から取得する。
Projectにはbase_sha、Harnessにはprofile_commit_shaを保存する。
全行のcontent_hashは、保存したcontentのバイト列に対する小文字のSHA-256である。

4階層の行とcontext_pinnedイベントを同じトランザクションで保存する。
イベントのrevisionsはlayer・path・hashのみを含み、内容は含めない。
同じProject/Harness一覧の再送には、GlobalやTaskを読み直さず、既存のrevision IDと内容を返す。
差し替え・追加・削除を伴う再送は409で拒否する。DBエラーでは全行とイベントをロールバックする。

応答は200と`{"instructions":"...","revisionIds":["..."]}`である。
revisionIdsはGlobal・Harness・Project・Taskの順、各階層はパス順に並べる。
現時点のinstructionsにはGlobalと、タスクデータとして区切ったTaskを含める。
CLI/Adapterがファイルとして読むProjectとHarnessを重ねて挿入しない。
Adapter別の選択・整形・競合判定はContext Builderの別実装単位で扱う。
