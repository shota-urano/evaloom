# Execution Contextの保存API

正本は`docs/specs/07-context.md`の3.2〜3.5・5・6節と、
`02-data-model.md`のcontext_revisions・5.3・5.7節。

`POST /api/executions/{id}/context`はRunner認証を使う。本人のセッションは403、
無効なBearerは401で拒否する。別Runnerに属するExecutionと未claimのExecutionは
403で拒否し、既存の認証拒否処理でaudit_logsに記録する。
存在しないExecutionは404。保存できるのは最新Executionのpreparing中だけである。
所有権確認と保存には同じDBトランザクションを使う。

要求形式は次のとおり。空のファイル一覧も両方を明示する。
Runnerは送信直前にworktreeの現在のHEADを`gitcontext.HeadCommitSHA`で取得し、
必須のheadShaとして送る。これはContextの固定元であるbase_shaとは別に扱う。
APIでの受け取り方法は仕様に未定義のため、hb9.4の修正時にこの必須項目を追加した。
既存のSHA検証と同じく小文字の完全なcommit SHAを要求し、省略・不正値は400で拒否する。
その場合は状態・Context・イベント・worktreeを変更せず、base_shaによる補完はしない。
既存の`projectcontext.Read`と`harnesscontext.Read`が返すFileをそのままJSON化できる。
`content`は元のバイト列をbase64にした文字列とする。FileのJSON処理が変換と復号を行い、
不正なUTF-8を含む内容も変えずに転送する。ハッシュは復号した元のバイト列で照合する。

```json
{
  "headSha": "<Runnerが確認した現在のHEAD>",
  "project": [
    {"path":"AGENTS.md","content":"<base64>","contentHash":"<SHA-256>","sourceCommitSha":"<base_sha>"}
  ],
  "harness": [
    {"path":"prompts/developer.md","content":"<base64>","contentHash":"<SHA-256>","sourceCommitSha":"<profile_commit_sha>"}
  ]
}
```

APIは相対パス・重複・内容のSHA-256・Executionに保存済みのSHAとの一致を検証する。
不正なパス・ハッシュ・未選択ファイルは400で拒否し、行を保存しない。
Projectはprojectcontext.Readと同じ対象に限定する。保存済みContextの検索は当該Executionに
限定するため、別Projectや別Taskの行とrevision IDを応答に含めない。
Runnerが送るファイルの由来は、従来どおり認証済みRunnerの読み込みとSHAに基づく。
Control PlaneがProjectリポジトリを再読して提出内容を照合する処理は追加していない。

Context BuilderはExecutionに保存したProfile IDで登録済み定義を取得し、Harness ID・版・
commit SHA・定義ハッシュを検証する。現在のproject_harnesses割り当てやタグは参照しない。
登録済みProfileとExecutionの版・SHAが食い違う場合は、03-registry.mdの3.5節に従い
failed(config_error)にする。提出したProject/HarnessファイルのSHAが固定値と異なる場合は
構造的な矛盾としてTaskとExecutionをblocked、execution_blocks.causeをcontext_conflictにする。
blockedではerror_classに値を入れず、causeで分類する。HTTPは409でdetails.reasonも
context_conflictとする。ブロックのhead_shaには、02-data-model.mdの定義に従い、
認証済みRunnerが報告したheadShaを保存する。continueでは06-worktree.mdの3.3節に従って
以前のbase_shaを引き継ぐため、head_shaとbase_shaは異なる場合がある。
関連worktreeは、blockedとfailed(config_error)のどちらでもretainedにする
（06-worktree.mdの3.6節）。状態遷移・エラー分類・ブロック行・worktreeの保持状態・
blocked/status_changedイベントは同一トランザクションで保存する。
意味の矛盾の判定や、ルートAGENTS.mdを必須とする独自規則は追加していない。

4.3のスキーマにはファイルのパス指定が無いため、hb9.4の実装判断として任意のcontextFilesを
追加した。値はリポジトリ相対の個別ファイル名の配列で、ワイルドカード展開はしない。
省略・空配列は空の選択、null・重複・不正なパスは登録時に拒否する。
promptVersionやskillVersionsからパスを推測しない。例は次のとおり。

```yaml
contextFiles:
  - prompts/developer.md
  - skills/check/SKILL.md
  - .codex/config.toml
```

Runnerは固定した定義のcontextFilesをharnesscontext.Readのpathsに渡す。
APIは指定したファイルだけを受け付け、必須ファイルが提出一覧に無ければ07-context.mdの
エラー処理表に従いfailed(config_error)・422にする。登録済み定義の不正や未登録Adapterも
設定不備として同様に処理する。ハーネスのタグや作業ツリーは読み直さない。
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
instructionsはGlobal、注入が必要なProject指示、タスクデータとして区切ったTaskの順にする。
Harnessは仕様3.3の専用設定環境で読むため本文には重ねない。
ProjectのAGENTS.md・CLAUDE.mdはAdapter Registryから取得したNativeInstructionFilesに
ファイル名が無いものだけをパス順で含める。サブディレクトリの指示も同様に扱う。
.mcp.jsonと.claude/・.codex/内の設定・Skillsは作業ディレクトリから読むため重ねない。
本文から省いたファイルもContextの記録とrevisionIdsには含める。

情報量調整は仕様3.6の「実装時に確定」に従い、選択した全階層のcontentの合計を
1 MiB（1,048,576バイト）までとした。ネイティブに読むファイルとTaskの保存JSONも数え、
整形用の見出しは数えない。超過時は切り捨てずfailed(config_error)・422とし、行を保存しない。
