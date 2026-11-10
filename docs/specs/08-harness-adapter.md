---
status: draft
written: 2026-11-10
範囲: ミクロハーネス（Claude Code・Codex）の Adapter。共通インターフェース、Capability、作業の段の流れ、質問の後の同じセッションの再開、専用設定環境と認証ファイル、Tool Permissions の変換、Adapter の登録
---

# 08. ミクロハーネス Adapter 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Runner が作業の段でミクロハーネス（Claude Code・Codex）を共通のインターフェースで起動・監視・中止・再開するための層

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。Adapter が扱うのはミクロハーネスであり、スペシャルハーネスではない。commit、差分、HEAD の取得と、push の扱いは software-engineer へ移し、この文書からは消した（本人決定 1）。段の名前と作業の段の約束はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）6.2・6.3・8章に従う。

## 1. 目的

- 種類の違うミクロハーネスを、共通のインターフェースで作業の段として動かす（requirements 9.1）。
- 作業の段の入力（要求 JSON と Execution Context）と出力（終了の状態、`out/summary.md`、`out/question.json`、セッションの識別子）を、契約のとおりにやり取りする（requirements 9.5、条件9）。
- Evaloom から起動するミクロハーネスに専用の設定環境を用意し、本人のグローバル設定や個人のメモリを読み込ませない（requirements 7.5、条件11）。
- 作業中の AI が判断できずに質問を書いて終わったとき、本人の回答を渡して同じセッションを再開できるようにする（本人決定、vision-alignment.md 6章）。
- 新しい Adapter を足すとき、中央のコードは登録処理のほかを変えない（条件31）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | Profile の `stages.work`（`microHarness`、`model`、`instructions`、`skills`、`settings`、`toolPermissions`、`timeoutSec`。契約 5章）、要求 JSON（契約 6.1）、Execution Context（[`07-context.md`](./07-context.md) 5章）、Work Sandbox の実行環境（[`09-sandbox.md`](./09-sandbox.md)）、再開のときは executions.work_session_id と本人の回答 |
| 出力 | 作業の段の結果（`succeeded` / `failed` / `question`、エラー分類、セッションの識別子、使用量）、`out/summary.md` と `out/question.json` の成果物、ログのストリーム |

## 3. 処理詳細

### 3.1 Adapter インターフェース（requirements 9.3）

requirements 9.3 の共通機能と、Go のインターフェース案の対応を示す。厳密なインターフェースは requirements 21章で詳細設計事項とされている。

| 共通機能（9.3） | メソッド（案） |
|---|---|
| 実行要求の受け付け | `Start(ctx, req WorkRequest, env SandboxEnv) (SessionID, error)` |
| 同じセッションの再開 | `Resume(ctx, id SessionID, req WorkRequest, env SandboxEnv) error`（`Capabilities().Resume` が true のときだけ使う） |
| 実行セッションの識別 | `SessionID`（ミクロハーネスが返すセッションの識別子。executions.work_session_id に保存する） |
| 状態の取得 | `Status(id SessionID) (SessionStatus, error)` |
| ログの取得 | `Logs(id SessionID) (<-chan LogLine, error)` |
| キャンセル | `Cancel(id SessionID) error` |
| 結果の返却 | `Result(id SessionID) (WorkOutcome, error)` |
| エラー分類 | `WorkOutcome.ErrorClass`（3.8） |
| Capability の宣言 | `Capabilities() Capabilities` |

```go
type Capabilities struct {
    Streaming  bool // ログを逐次取得できる
    Resume     bool // 終わったセッションを、同じ識別子で続きから再開できる
    TokenUsage bool // トークン数を返せる
}

type WorkRequest struct {
    RequestPath string            // 要求 JSON のファイル（読み取り専用）
    Context     ExecutionContext  // 07 の 5章
    Model       string            // Profile の stages.work.model
    Permissions ToolPermissions   // Profile の stages.work.toolPermissions（3.9）
    Answer      *Answer           // 再開のときだけ。要求 JSON の answer と同じ値
}

type WorkOutcome struct {
    Status     string // "succeeded" | "failed"（question かどうかは 3.3 で Runner が決める）
    ErrorClass string // 3.8
    ExitCode   int
    SessionID  string
    Usage      *Usage
}
```

- 再開やストリーミングなど、ミクロハーネスごとに違う能力は Capability として宣言する。対応しない能力を無理に共通化しない（requirements 9.3）。
- `Resume` を使うのは、質問への回答の後に作業の段を再開するときだけである（契約 8章）。Evaloom の再起動で止まった作業の段は再開せず、`interrupted` として記録する（契約 10.3）。
- `SessionID` は、Runner が作業の段の結果の報告（`POST /api/executions/{id}/result`）に載せ、Control Plane が executions.work_session_id に保存する。`Start` が失敗してセッションの識別子が無いときは載せない。

### 3.2 Adapter の登録（条件31）

- Adapter は `internal/adapter/<name>` に1つずつ置き、登録関数で名前（`claude_code` / `codex`、[`02-data-model.md`](./02-data-model.md) 4.2）と結び付ける。
- Runner と Control Plane は、Adapter の名前で登録表を引くだけで、個別の Adapter の型を参照しない。
- Profile の登録で `stages.work.microHarness` に受け付ける名前の一覧は、登録表から得る（[`03-registry.md`](./03-registry.md)）。
- Adapter の追加で変えてよいのは、新しいパッケージと登録の1行だけとする。モデル API の宛先の一覧（3.6・3.7）と Tool Permissions の変換（3.9）も、そのパッケージの中に置く。
- requirements 20章 条件31 の「中央のコード」と「登録処理」の範囲、判定に使う拡張試験は 21章で詳細設計事項とされている。テスト用のダミー Adapter を登録表に足すだけで Execution が通るテストを用意する（9章）。

### 3.3 作業の段の流れ

1. Work Sandbox を作る（[`09-sandbox.md`](./09-sandbox.md) 3.1）。書き込めるのは、作業領域（`workspace`）、`out`、`scratch`、3.5 の作業用の設定コピーだけとする。
2. 専用設定環境を作る（3.5）。
3. 要求 JSON を読み取り専用のファイルとして置き、Execution Context と合わせて、CLI に渡せる形（引数、標準入力、ファイル）にする。外から来た文（Issue の本文など）は、タスクの内容として区切って渡す（契約 6.6）。
4. Sandbox の中で CLI を非対話モードで起動する。CLI の作業ディレクトリは作業領域（要求 JSON の `paths.workspace`）とする。
5. 標準出力と標準エラーを読み、Runner のログ収集へ渡す（[`05-runner.md`](./05-runner.md)）。
6. CLI が終わったら、終了コードとセッションから作業の段の結果を決める（契約 6.3）。作業の段が書いたファイルは、合否に使わない。
7. 終了コードが 0 のときだけ、`out` から次の2つのファイルを読む。どちらも任意である。ほかのファイルは Evaloom が成果物として保存するだけで、中身を解釈しない。
   - `out/question.json`: あれば、形を確かめて成果物（kind=`question`）として保存し、作業の段の結果を `question` にする。Execution は本人の判断待ち（cause=`question`）になり、確かめる段へ進まない（契約 8章）。
   - `out/summary.md`: あれば、機密情報の除外を通して成果物（kind=`work_summary`）として保存する。管理画面と Linear のコメントに載せる。
8. 認証ファイルを書き戻す（3.5）。
9. Sandbox と専用設定環境を片付ける。結果が `question` のときは、再開に要るセッションのファイルを保持する（3.5、Q143）。

`out/question.json` と `out/summary.md` の検査の細かい規則は Q144 で決める。

### 3.4 質問の後の同じセッションの再開（契約 8章）

本人が管理画面で質問に答えると、Execution は resume_stage=`work` で claim を待つ（[`02-data-model.md`](./02-data-model.md) 5.2・5.6）。Runner は次のとおり作業の段を再開する。

1. 同じタスクの作業領域（task_workspaces、status=`retained`）を使う。作業領域が無ければ、作業の段を始めずに本人の判断待ち（cause=`workspace_missing`）にする。
2. 新しい Work Sandbox を作る。前の作業の段の Sandbox は使い回さない（[`01-architecture.md`](./01-architecture.md) 3.2）。
3. Execution Context は、同じ Execution で最初に固定したものを使う（[`07-context.md`](./07-context.md) 5章、Q141）。
4. Adapter の `Capabilities().Resume` が true で、executions.work_session_id があり、保持したセッションのファイルを戻せたときは、`Resume` で同じセッションを再開し、本人の回答を渡す。
5. 同じセッションを再開できないときは、契約の要確認 Q124 の案のとおり、新しいセッションを `Start` で始め、元の質問と本人の回答を要求 JSON の `answer` で渡す。
6. 再開した作業の段がまた `out/question.json` を書いたら、もう一度本人の判断待ちにする。回数の上限は設けない（本人が中止を選べるため）。

### 3.5 専用設定環境（requirements 7.5、Q9）

Evaloom から起動するミクロハーネスには、作業の段ごとに専用の設定環境を作る。

| 要件 | 方法（案） |
|---|---|
| 本人のグローバル設定を読ませない | Sandbox 内の HOME を作業の段ごとの空のディレクトリにする。本人の HOME（`~/.claude/`、`~/.codex/`、`~/.gitconfig` など）を Sandbox に見せない |
| 個人のメモリを読ませない | 同上。本人の HOME の下を一切マウントしない |
| スペシャルハーネスの設定を与える | Profile の commit から読んだ `stages.work` の指示・スキル・設定（[`07-context.md`](./07-context.md) 3.2）を専用設定ディレクトリに置く。指示ファイルをどこに置くかは Q142 |
| CLI に専用設定ディレクトリを使わせる | Codex は `CODEX_HOME`、Claude Code は `CLAUDE_CONFIG_DIR` を専用ディレクトリに向ける。各 CLI の版で、この指定どおりに読まれることを実装時に確かめる |
| 読み込んだ構成を記録する | 置いたファイルのパスとハッシュを context_revisions（layer=`harness`、kind=`instructions` / `skill` / `settings`）に保存する（[`07-context.md`](./07-context.md) 3.2、requirements 7.1） |
| 設定ディレクトリへの書き込みが要る CLI に対応する | 専用設定ディレクトリは読み取り専用のまま残す。CLI が設定ディレクトリに書き込めないと起動しない場合は、作業の段ごとに書き込める作業用の設定コピーを作り、CLI の設定ディレクトリの環境変数をそのコピーへ向ける。CLI を起動する前に、コピーのうち Profile から置いた全ファイルのパスとハッシュが記録した値と一致することを確かめ、一致しなければ `config_error` で止める。CLI がコピーへ書き込んだ内容は、読み込んだ構成の記録に含めない。どの CLI がコピーを要するかは Adapter が決める |
| コピーの片付け | 作業の段の結果が `question` 以外なら、段の終わりにコピーを捨て、次の作業の段に持ち越さない。`question` のときは、認証ファイルを除いたコピーを、Runner のデータディレクトリの中のタスクごとの保管場所に移して、再開まで保持する（Q143） |
| 更新された認証トークンを失わない | 下の「認証ファイルの受け渡しと書き戻し」に従う。Codex と Claude Code は実行中に認証トークンを更新して認証ファイルへ書き戻すため、両方とも作業用の設定コピーを使う |

#### 認証ファイルの受け渡しと書き戻し（2026-11-05 決定）

サブスクリプションのログイン情報（[`09-sandbox.md`](./09-sandbox.md) 3.5）は、CLI が実行中にトークンを更新し、設定ディレクトリの認証ファイルへ書き込む。更新前のリフレッシュトークンが使えなくなる場合があるため、作業用の設定コピーと一緒に捨てると、次の作業の段が失効したトークンで始まって認証エラーになる。次のとおり扱う。

| 項目 | 内容 |
|---|---|
| 対象の認証ファイル | ミクロハーネスごとに1つだけ。Codex は `auth.json`、Claude Code は `.credentials.json`。どちらも CLI の設定ディレクトリの直下に置く |
| 認証の保管場所 | Evaloom 専用の認証ファイルを、ミクロハーネスごとに1つ Runner のデータディレクトリ（Git 管理外）に置く。パスは Runner の設定ファイルで、ミクロハーネスの名前ごとに指定する（[`05-runner.md`](./05-runner.md) 4.1）。本人の普段の認証ファイル（`~/.codex/`、`~/.claude/`）は使わない。ファイルの権限は 0600 とする |
| 渡し方 | 作業用の設定コピーを作るとき、保管場所の認証ファイルをコピーの対象パスに置く。読み取り専用の専用設定ディレクトリ（Profile から作るもの）には置かない。保管場所に認証ファイルが無い、または下の検査に通らない場合は CLI を起動せず `config_error` で止める |
| 読み込んだ構成の記録との関係 | 認証ファイルは Profile の設定ではないので、context_revisions に内容もハッシュも保存しない。CLI を起動する前の照合では、コピーのうち認証ファイルを除く Profile から置いた全ファイルが記録と一致することと、認証ファイルが対象パスに1つだけあることを確かめる。認証ファイルの内容は照合に使わない |
| 書き戻す時機 | Sandbox が止まり、CLI の書き込みが終わったことを確かめた後、作業用の設定コピーを捨てる（または保持する）前に行う。作業の段の結果（成功・失敗・質問・中止・タイムアウト）にかかわらず行う。失敗した段の中でもトークンは更新されうるためである |
| 書き戻す前の検査 | コピーの認証ファイルがシンボリックリンクではない通常ファイルであること、大きさが 64 KiB 以下であること、内容が JSON のオブジェクトとして読めることを確かめる。通らなければ書き戻さず、保管場所の認証ファイルをそのまま残す。理由を Runner のログに残す（認証ファイルの内容はログに出さない） |
| 書き戻し方 | 内容が保管場所と同じなら何もしない。違えば、保管場所と同じディレクトリに一時ファイル（0600）を書き、ディスクへの書き出しを確かめてから名前を付け替えて置き換える。途中で失敗しても、保管場所に中身の欠けたファイルを残さない |
| ログからの除外 | Runner はログを行単位で除外してから保存・配信する（[`05-runner.md`](./05-runner.md)。CLI の実行中も逐次配信する）。除外する値は、段の開始時に読んだ認証ファイルの値と、各 CLI コマンド（認証確認と本実行）の終了ごとに作業用の設定コピーから読み直した値で、段の中で累積する。値の取り出しには JSON の文字列値を使う。読み直しに失敗したら、それ以降の CLI の出力は保存・配信しない。CLI が実行中に新しく発行し、Runner がまだ読んでいない値は、その時点までに配信した行からは除外できない。この残りの危険は、CLI が認証の値を出力しないことに頼る |
| 保持するコピーとの関係 | 結果が `question` で作業用の設定コピーを保持するときも、認証ファイルは書き戻した後にコピーから消してから保持する。再開のときは保管場所の最新の認証ファイルを置き直す |
| 同時実行 | 同じミクロハーネスの認証ファイルを使う作業の段と振り分けの提案（3.10）は、同時に1本までにする（全体の同時実行の上限2本、[`04-task-lifecycle.md`](./04-task-lifecycle.md) は変えない）。Runner は、保管場所の認証ファイルを読む前にミクロハーネスごとの排他ロック（OS のファイルロック）を取り、書き戻しを終えてから外す。ロックを待っているものは、中止の指示とタイムアウトで待つのをやめる |

書き戻さなかった場合（検査に通らない、書き込みに失敗した）でも、作業の段の結果は変えない。次の作業の段が認証エラーになったら、`config_error` として扱う（3.8）。

「HOME を見せない」ことを主な対策とし、環境変数による設定ディレクトリの指定は補助とする。環境変数だけに頼ると、CLI の版の違いで別の場所を読む可能性が残るためである。

実際に読み込んだ指示の検証は [`14-security-tests.md`](./14-security-tests.md) 3.1 で行う（requirements 7.5、14.2）。

### 3.6 Codex Adapter

| 項目 | 内容 |
|---|---|
| 起動 | 非対話モード（`codex exec` を候補とする）。引数・フラグは採用する版で確定する |
| 再開 | `codex exec resume <セッション>` を候補とする。採用する版で、作業用の設定コピーを戻した後に同じセッションを続けられるかを実装時に確かめ、できなければ `Capabilities().Resume` を false にする |
| 作業ディレクトリ | Sandbox の中の作業領域 |
| 設定 | `CODEX_HOME` を 3.5 の作業用の設定コピーに向ける。codex-cli 0.163.0 は `CODEX_HOME` に書き込めないと起動しない（`failed to initialize in-process app-server client: Read-only file system`。2026-11-05 に [`14-security-tests.md`](./14-security-tests.md) 3.1 の実モデル試験で確認） |
| 指示の渡し方 | Q142 の案のとおり、`stages.work.instructions` を `CODEX_HOME` の指示ファイルとして置き、Execution Context の指示文は起動の入力で渡す |
| ツール権限 | Profile の `stages.work.toolPermissions` を CLI の設定に反映する。変換の規則は 3.9（案 Q72） |
| 認証 | Q7（サブスクリプションか API キーか）と Q8（値をミクロハーネスから見えないようにする方式）の結論に従う（[`09-sandbox.md`](./09-sandbox.md) 3.5） |
| ネットワークの許可先（案 Q71） | ChatGPT のログインで使う `chatgpt.com:443`、`auth.openai.com:443`、`api.openai.com:443` を候補とする。採用する CLI の版で、この一覧だけで認証確認と本実行が通ることを [`14-security-tests.md`](./14-security-tests.md) 3.1 の実モデル試験で確かめてから確定する |

### 3.7 Claude Code Adapter

| 項目 | 内容 |
|---|---|
| 起動 | 非対話モード（`claude -p` と構造化出力を候補とする）。引数・フラグは採用する版で確定する |
| 再開 | `claude -p --resume <セッション>` を候補とする（vision-alignment.md 6章）。Codex と同じく実装時に確かめる |
| 作業ディレクトリ | Sandbox の中の作業領域 |
| 設定 | `CLAUDE_CONFIG_DIR` を 3.5 の作業用の設定コピーに向ける。認証トークンの書き戻し（3.5）に、`.credentials.json` を書き込めるコピーが要るため |
| 指示の渡し方 | Q142 の案のとおり、`stages.work.instructions` を `CLAUDE_CONFIG_DIR` の指示ファイルとして置き、Execution Context の指示文は起動の入力で渡す |
| ツール権限 | Profile の `stages.work.toolPermissions` を CLI の許可設定に反映する。変換の規則は 3.9（案 Q72） |
| 認証 | Codex と同じく Q7・Q8 に従う |
| ネットワークの許可先（案 Q71） | サブスクリプションのログインで使う `api.anthropic.com:443`、`console.anthropic.com:443`、`claude.ai:443` を候補とする。Codex と同じく実モデル試験で確かめてから確定する |

### 3.8 エラー分類の対応（requirements 9.5）

作業の段の結果のエラー分類は Evaloom が付ける（契約 13章）。

| 状況 | errorClass |
|---|---|
| CLI が見つからない、設定ファイルが不正、認証情報が無い、Tool Permissions を変換できない、作業用の設定コピーのハッシュが記録と合わない | `config_error` |
| CLI が 0 以外で終了した、出力を解釈できない、`out/question.json` の形が合わない（Q144） | `work_failed` |
| Sandbox を作れない、隔離の設定を確かめられない | `sandbox_error` |
| Runner が上限時間で止めた | `timeout` |
| Runner が中止の指示で止めた | `cancelled` |

確かめた結果が基準を満たさない（`check_failed`）は、確かめる段のスペシャルハーネスのプログラムが付ける。Adapter は付けない。

### 3.9 Tool Permissions から CLI の設定への変換（案 Q72）

Tool Permissions は Profile の `stages.work.toolPermissions` だけから取る（契約 5.2、[`02-data-model.md`](./02-data-model.md) の Q118）。本人は、Project に固定する Profile の版を選ぶことで許可を選ぶ。Runner は claim の時点の Profile の値を Adapter に渡し、実行中に読み直さない。変換は Adapter のパッケージの中に置く（3.2 の条件31の範囲）。

| 項目 | 内容（案） |
|---|---|
| 形 | `{ "allow": ["<CLI のツール名・規則>", ...], "deny": [...] }`。どちらのキーも任意。項目が無い、または `{}` は「Adapter の既定の許可だけ」を表す。`allow` と `deny` のほかのキーがあれば受け付けない |
| ツール名の書き方 | Adapter が使う CLI の表記のまま書く（例 Claude Code の `Bash(git commit:*)`）。Evaloom 独自のツール名は作らない |
| Claude Code | 設定 JSON を `{ "permissions": { "allow": <allow>, "deny": <deny> } }` とする。`--permission-mode dontAsk` なので、`allow` に無い操作は確認を求めずに拒否され、`deny` は `allow` より優先される |
| Codex | 採用した版（codex-cli 0.163.0）には、ツールごとの許可・禁止を表す設定が無い。`allow` と `deny` がどちらも空のときだけ受け付け、どちらかに値があれば表せない権限として受け付けない。実際の制限は Work Sandbox（[`09-sandbox.md`](./09-sandbox.md)）で行う |
| 変換できない値 | 未知のキー、配列でない値、空文字の要素は受け付けない |
| 検査の時機 | Profile の登録のときに Adapter の変換で確かめ、受け付けない値なら登録を拒否する（Q145）。claim の後にも同じ検査を行い、通らなければ CLI を起動せず `config_error` とする |

ツール権限は作業中の AI の手を絞る補助であり、鍵と外への接続を持たせない境界は Work Sandbox が受け持つ（[`09-sandbox.md`](./09-sandbox.md)）。

### 3.10 振り分けの提案での使い方

振り分けの提案（[`15-linear-intake.md`](./15-linear-intake.md)）も、全体設定 `routing.proposer` のミクロハーネスの Adapter で動かす（[`02-data-model.md`](./02-data-model.md) の Q111）。

- 動かす場所は Proposal Sandbox とし、作業領域は持たない（[`09-sandbox.md`](./09-sandbox.md) 3.1）。
- 指示は Evaloom の決まった提案の指示とし、スペシャルハーネスの `stages.work` は使わない。Issue の本文は外から来た文として区切って渡す。
- 専用設定環境、認証ファイルの受け渡しと書き戻し、ミクロハーネスごとの排他ロック（3.5）は作業の段と同じに扱う。
- Tool Permissions は、ファイルの読み取りと `out` への書き込みのほかを許さない Adapter の固定の値とする。
- 結果は `out/proposal.json` から Runner が読み、成果物（kind=`proposal_result`）として登録して、`POST /api/route-proposals/{id}/result` で返す。ファイルの形は [`15-linear-intake.md`](./15-linear-intake.md) の Q202。`Resume` は使わない。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| Adapter が扱うもの | ミクロハーネス（Claude Code、Codex） | 本人決定 8 |
| 初期対象 | Codex、Claude Code | requirements 9.2 |
| 実行場所 | Work Sandbox の中の作業領域（振り分けの提案は Proposal Sandbox） | requirements 9.4、契約 6.3 |
| 作業の段の出力として Evaloom が読むもの | `out/summary.md` と `out/question.json` だけ | 契約 6.3 |
| 作業の段の合否 | 終了コードとセッションで決める。作業の段が書いたファイルは合否に使わない | 契約 6.3 |
| セッションの再開 | 質問への回答の後だけ。executions.work_session_id を使う | 契約 8章 |
| Tool Permissions | Profile の `stages.work.toolPermissions` | 契約 5.2、Q118 |
| 本人のグローバル設定・個人のメモリ | 読み込ませない | requirements 7.5 |
| Capability | ミクロハーネスごとに宣言し、無理に共通化しない | requirements 9.3 |
| Adapter の追加 | 中央のコードは登録処理のほかを変えない | requirements 20章 条件31 |

## 5. インターフェース

- 段の入力と出力: 契約 6.1（要求 JSON）、6.3（作業の段）、8章（質問と再開）
- Execution Context: [`07-context.md`](./07-context.md) 5章
- エラー分類: [`02-data-model.md`](./02-data-model.md) 5.5
- Adapter の名前: [`02-data-model.md`](./02-data-model.md) 4.2
- 成果物の種類（`work_summary`、`question`）: [`02-data-model.md`](./02-data-model.md) 3.5 artifacts
- Sandbox の呼び出し: [`09-sandbox.md`](./09-sandbox.md) 5章

## 6. エラー処理

- CLI の異常終了は 3.8 の表で分類し、作業の段の結果を `failed` として返す。Adapter 自身は panic しない。
- `Cancel` を呼んだ後、一定時間でプロセスが終わらなければ Sandbox ごと強制終了する（[`05-runner.md`](./05-runner.md)）。
- 出力に秘密情報が混ざる可能性に備え、ログと `out/summary.md` は Runner の除外処理を通してから保存する（[`05-runner.md`](./05-runner.md)）。
- 再開で保持したセッションのファイルを戻せないときは、3.4 の5のとおり新しいセッションで続ける。失敗にはしない（Profile が別の扱いを宣言する場合は契約の Q124）。

## 7. スコープ外

- Gemini CLI、Factory Droid、OpenClaw、独自のミクロハーネス（requirements 9.2 の将来候補）
- Evaloom の再起動で止まった作業の段の再開（`interrupted` として記録する。契約 10.3）
- 作業領域の中身の解釈（commit、差分、HEAD の取得など）。スペシャルハーネスの確かめる段と出す段が行う

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) 5章 Profile 定義、6.3 作業の段、8章 質問と再開
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) executions.work_session_id、artifacts、エラー分類
- [`03-registry.md`](./03-registry.md) Profile の登録と `microHarness` の検査
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) 質問の後の再開と同時実行
- [`05-runner.md`](./05-runner.md) 段の実行手順、ログ収集、タイムアウト
- [`07-context.md`](./07-context.md) Execution Context と Harness の層
- [`09-sandbox.md`](./09-sandbox.md) Work Sandbox、Proposal Sandbox、モデル API の認証情報
- [`14-security-tests.md`](./14-security-tests.md) 設定環境の分離テスト
- [`15-linear-intake.md`](./15-linear-intake.md) 振り分けの提案

## 9. 実装単位

- [ ] [Backend] Adapter インターフェース、Capabilities、登録表
  - 受け入れ基準: Runner が Adapter の名前だけで実装を引ける; 未登録の名前でエラーになる; Profile の登録が登録表に無い `microHarness` を拒否する
- [ ] [Backend] ダミー Adapter による拡張試験（条件31）
  - 受け入れ基準: テスト用のダミー Adapter を新しいパッケージと登録の1行だけで足し、その Adapter で Execution が succeeded まで通るテストがある
- [ ] [Backend] 専用設定環境の作成と記録（専用 HOME、設定ディレクトリの環境変数、`stages.work` のファイルの配置、ハッシュの記録）
  - 受け入れ基準: Sandbox 内から本人の HOME の下が見えない; 置いたファイルの一覧とハッシュが context_revisions（layer=harness）に保存される; `stages.work` で宣言していないファイルが置かれない
- [ ] [Backend] 作業の段の結果の組み立て（3.3）
  - 受け入れ基準: 終了コード 0 で `out/question.json` が無ければ succeeded になる; 終了コード 0 で `out/question.json` があれば question になり、確かめる段へ進まない; 終了コードが 0 以外なら `out/question.json` があっても work_failed になる; `out` に確かめた結果に見えるファイルを置いても合否が変わらない; `out/summary.md` が機密情報の除外を通して work_summary として保存される; セッションの識別子が executions.work_session_id に保存される
- [ ] [Backend] 質問の後の同じセッションの再開（3.4）
  - 受け入れ基準: Resume に対応する Adapter で、回答の後に同じ work_session_id のセッションが再開され、本人の回答が渡る; 保持したセッションのファイルが無いとき、新しいセッションが要求 JSON の answer 付きで始まる; 作業領域が無いと blocked(workspace_missing) になる; 再開の Work Sandbox が前の Sandbox と別に作られる
- [ ] [Backend] Codex Adapter
  - 受け入れ基準: テスト用の作業領域で Codex を非対話で起動し、作業領域にファイルの変更が残る; 中止で止まる; 異常終了が 3.8 の分類で返る
- [ ] [Backend] Claude Code Adapter
  - 受け入れ基準: テスト用の作業領域で Claude Code を非対話で起動し、作業領域にファイルの変更が残る; 中止で止まる; 異常終了が 3.8 の分類で返る
- [ ] [Backend] 作業用の設定コピー（3.5。設定ディレクトリへの書き込みが要る CLI 向け）
  - 受け入れ基準: Adapter が設定ディレクトリの環境変数を作業の段ごとの書き込めるコピーに向け、元の専用設定ディレクトリは読み取り専用のまま変わらない; コピーのハッシュが記録と一致しないと CLI を起動せず config_error になる; 結果が question 以外ならコピーは段の終わり（成功・失敗・中止）に消える; 結果が question なら、認証ファイルを除いたコピーがタスクごとの保管場所に残る
- [ ] [Backend] 認証ファイルの受け渡しと書き戻し（3.5。Codex と Claude Code。同じミクロハーネスの認証を使う作業の段と振り分けの提案を同時に1本までにする）
  - 受け入れ基準: Codex Adapter と Claude Code Adapter が、保管場所の認証ファイルを作業用の設定コピーの対象パス（auth.json / .credentials.json）に置いてから CLI を起動する; 保管場所に認証ファイルが無いと CLI を起動せず config_error になる; CLI がコピーの認証ファイルを書き換えた場合、段の終わり（成功・失敗・質問・中止・タイムアウト）に保管場所の内容が書き換え後と一致する; JSON のオブジェクトとして読めない・64 KiB を超える・シンボリックリンクの認証ファイルは書き戻さず、保管場所の内容が変わらない; 書き込みが途中で失敗しても保管場所の認証ファイルは書き戻し前の内容のまま読める; context_revisions に認証ファイルのパス・内容・ハッシュが保存されない; 同じミクロハーネスの作業の段を2本同時に始めると、2本目は1本目の書き戻しが終わるまで保管場所の認証ファイルを読まない; ロックを待っているものが中止の指示で止まる; 開始時に読んだ認証の値と、認証確認の終了後に読み直した値が Runner のログファイル・events に出ない; 通常のログ行は CLI の実行中にも逐次保存・配信される
- [ ] （人間） Claude Code・Codex の認証方式と、各社規約での自動実行の可否を確認して決める（Q7）
- [ ] [Backend] Tool Permissions の変換とモデル API の宛先（3.9、3.6・3.7 の許可先。Q71・Q72 の決定後）
  - 受け入れ基準: Claude Code Adapter が `allow` と `deny` から `permissions.allow` / `permissions.deny` の設定 JSON を作り、CLI の起動引数に渡す; Codex Adapter が空の Tool Permissions で起動し、`allow` か `deny` に値がある Profile の登録が拒否される; 未知のキーを持つ Tool Permissions の Profile の登録が両 Adapter で拒否される; 各 Adapter が宣言した許可先だけが Work Sandbox のネットワークの許可に渡る; 許可先の一覧にモデル API と認証の更新先のほかの宛先が入っていない
- [ ] [Backend] 振り分けの提案での Adapter の利用（3.10）
  - 受け入れ基準: 提案が Proposal Sandbox で動き、作業領域が無い; 作業の段と同じ認証ファイルの排他ロックに従う; `out` の結果のファイルから提案の結果が組み立てられる

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q142 | `stages.work.instructions` の指示ファイルを、ミクロハーネスにどう読ませるか | 決定（2026-11-10 本人、案を採用）: Claude Code は `CLAUDE_CONFIG_DIR` の直下の `CLAUDE.md`、Codex は `CODEX_HOME` の直下の `AGENTS.md` に、宣言の順に連結して置く（どちらの CLI もユーザー単位の指示として読む場所）。Execution Context の指示文は起動の入力で渡す。実際に読まれることを [`14-security-tests.md`](./14-security-tests.md) 3.1 のカナリア B で確かめ、読まれなければ起動の入力に連結する |
| Q143 | 質問の後に同じセッションを再開するため、セッションのファイルをどこに保持するか | 決定（2026-11-10 本人、案を採用）: 作業の段の結果が `question` のとき、認証ファイルを除いた作業用の設定コピーを Runner のデータディレクトリの中のタスクごとの保管場所（0700）に移す。再開のときに新しい作業用の設定コピーとして戻し、Profile から置いたファイルのハッシュを確かめ直してから `Resume` を呼ぶ。判断待ちが回答・取り下げ・中止で閉じたら消す。保持期間は作業領域と同じ（02 の Q121） |
| Q144 | `out/question.json` と `out/summary.md` の検査 | 決定（2026-11-10 本人、案を採用）: `out/question.json` は 64 KiB 以下の通常ファイルで、契約 8章の形（`question` は 1〜4000 文字、`options` は任意で 10 個以内の文字列、ほかの項目は無い）でなければ `work_failed` にし、ファイルは成果物として残す。`out/summary.md` は 64 KiB 以下の通常ファイルだけを読み、超えた分は切り捨てて切り捨てたことを表示する。シンボリックリンクはどちらも読まない |
| Q145 | Tool Permissions の検査をいつ行うか | 決定（2026-11-10 本人、案を採用）: Profile の登録のときに、`stages.work.microHarness` の Adapter の変換で確かめ、変換できなければ 422（`precondition_failed`）で登録を拒否する。claim の後にも同じ検査を行う（Adapter の版が変わった場合に備える） |
