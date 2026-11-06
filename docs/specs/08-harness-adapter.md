---
status: confirmed
範囲: 共通契約に従う Adapter インターフェース、Capability、Codex / Claude Code Adapter、ハーネスの専用設定環境、Adapter の登録
---

# 08. Harness Adapter 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Runner からハーネス（Codex / Claude Code）を共通インターフェースで起動・監視・中止するための層

## 1. 目的

- 異なる AI ハーネスを共通インターフェースで実行する（requirements 9.1）。
- 各ハーネスが共通契約に従って入出力をやりとりする（requirements 9.5、条件9）。
- Evaloom から起動するハーネスに専用の設定環境を用意し、本人のグローバル設定や個人メモリを読み込ませない（requirements 7.5、条件11）。
- 新しい Adapter を追加するとき、中央のコードは登録処理以外を変更しない（条件31）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | HarnessRequest（[`02-data-model.md`](./02-data-model.md) 5.7）、Harness Sandbox の実行環境（[`09-sandbox.md`](./09-sandbox.md)） |
| 出力 | HarnessResult（[`02-data-model.md`](./02-data-model.md) 5.8）、ログのストリーム |

## 3. 処理詳細

### 3.1 Adapter インターフェース（requirements 9.3）

requirements 9.3 の共通機能と、Go のインターフェース案の対応。厳密なインターフェースは 21章で詳細設計事項とされている。

| 共通機能（9.3） | メソッド（案） |
|---|---|
| 実行要求受付 | `Start(ctx, req HarnessRequest, env SandboxEnv) (SessionID, error)` |
| 実行セッション識別 | `SessionID`（Adapter が発行し、executions.harness_session_id に保存） |
| ステータス取得 | `Status(id SessionID) (SessionStatus, error)` |
| ログ取得 | `Logs(id SessionID) (<-chan LogLine, error)` |
| キャンセル | `Cancel(id SessionID) error` |
| 成果物返却 | `Result(id SessionID) (HarnessResult, error)` |
| エラー分類 | `Result` の `errorClass`（[`02-data-model.md`](./02-data-model.md) 5.4） |
| Capability の宣言 | `Capabilities() Capabilities` |

```go
type Capabilities struct {
    Streaming              bool     // ログを逐次取得できる
    Resume                 bool     // 中断したセッションを再開できる
    TokenUsage             bool     // トークン数を返せる
    NativeInstructionFiles []string // CLI が作業ディレクトリから自分で読む指示ファイル名
}
```

- 再開やストリーミングなどハーネスごとに異なる能力は Capability として宣言する。サポートしない能力を無理に共通化しない（requirements 9.3）。Phase 1 の Runner は Resume を使わない（中断した Execution は失敗として扱うため、requirements 8.3）。
- `NativeInstructionFiles` は Context Builder が二重に渡さないために使う（[`07-context.md`](./07-context.md) 3.6）。

### 3.2 Adapter の登録（条件31）

- Adapter は `internal/adapter/<name>` に1つずつ置き、登録関数で名前（`codex` / `claude_code`）と結び付ける。
- Runner と Control Plane は、Adapter 名で登録表を引くだけで、個別の Adapter の型を参照しない。
- Adapter の追加で変えてよいのは、新しいパッケージと登録の1行、Harness 登録時に受け付ける Adapter 名の一覧（登録表から自動で得る）だけとする。
- requirements 20章 条件31 の「中央のコード」と「登録処理」の範囲、判定に使う拡張試験は 21章で詳細設計事項とされている。Phase 1 では、テスト用のダミー Adapter を登録表に足すだけで Execution が通るテストを用意する（9章）。

### 3.3 Adapter の共通の流れ

1. Harness Sandbox を作る（[`09-sandbox.md`](./09-sandbox.md)）。書き込み可能にするのは worktree（と commit に必要な Git 領域）と、3.4 の作業用の設定コピーだけとする。
2. 専用設定環境を作る（3.4）。
3. Execution Context と HarnessRequest を、CLI に渡せる形（引数、標準入力、ファイル）にする。
4. Sandbox の中で CLI を非対話モードで起動する。
5. 標準出力・標準エラーを読み、Runner のログ収集へ渡す（[`05-runner.md`](./05-runner.md) 3.4）。
6. 終了したら、終了コードと出力から HarnessResult を組み立てる。
   - `headSha`、`commits` は Runner が worktree から取る（`git rev-parse HEAD`、`git rev-list <base>..HEAD`）。CLI の自己申告は使わない。
   - 差分（`diffArtifactId`）も Runner が `git diff <base_sha>..HEAD` で作る。
   - `summary` は CLI の最終出力から取る。取れなければ空にする。
7. Sandbox と専用設定環境を片付ける。

ハーネスには commit までをさせる。push と PR 作成は Runner の Git Publisher が行う（requirements 8.5）。ハーネスに push の手段を与えない方法は [`09-sandbox.md`](./09-sandbox.md) で定める。

commit の作者名・メールアドレスは、Runner が専用設定環境の Git 設定で与える（本人のグローバル Git 設定は読ませない）。値は Runner の設定ファイルの `commitAuthor` で指定する（[`05-runner.md`](./05-runner.md) 4.1、2026-11-06 決定）。

### 3.4 専用設定環境（requirements 7.5、Q9）

Evaloom から起動するハーネスには、Execution ごとに専用の設定環境を作る。

| 要件 | 方法（案） |
|---|---|
| 本人のグローバル設定を読ませない | Sandbox 内の HOME を Execution 専用の空のディレクトリにする。本人の HOME（`~/.claude/`、`~/.codex/`、`~/.gitconfig` 等）を Sandbox に見せない |
| 個人メモリを読ませない | 同上。本人の HOME 配下を一切マウントしない |
| ハーネスの設定を与える | Profile の commit SHA から読んだ設定・プロンプト・Skills（[`07-context.md`](./07-context.md) 3.3）を専用設定ディレクトリに置く |
| CLI に専用設定ディレクトリを使わせる | Codex は `CODEX_HOME`、Claude Code は `CLAUDE_CONFIG_DIR` を専用ディレクトリに向ける案。各 CLI の版で効くことを実装時に確かめる |
| 読み込んだ構成を記録する | 専用設定ディレクトリの全ファイルのパスとハッシュを context_revisions（layer=`harness`、kind=`harness_config`）に保存する（requirements 7.1） |
| 設定ディレクトリへの書き込みが要る CLI に対応する | 専用設定ディレクトリは読み取り専用のまま残す。CLI が設定ディレクトリに書き込めないと起動しない場合は、Execution ごとに書き込み可能な作業用の設定コピーを作り、CLI の設定ディレクトリの環境変数をそのコピーへ向ける。CLI を起動する前に、コピーの全ファイルのパスとハッシュが記録した値と一致することを確かめ、一致しなければ `config_error` で止める。コピーは Execution の終了時に捨て、次の Execution に持ち越さない。CLI がコピーへ書き込んだ内容は、読み込んだ構成の記録に含めない。どの CLI がコピーを要するかは Adapter が決める |
| 更新された認証トークンを失わない | 下の「認証ファイルの受け渡しと書き戻し」に従う。Codex と Claude Code は実行中に認証トークンを更新して認証ファイルへ書き戻すため、両方とも作業用の設定コピーを使う |

#### 認証ファイルの受け渡しと書き戻し（2026-11-05 決定）

サブスクリプションのログイン情報（[`09-sandbox.md`](./09-sandbox.md) 3.5）は、CLI が実行中にトークンを更新し、設定ディレクトリの認証ファイルへ書き込む。更新前のリフレッシュトークンが使えなくなる場合があるため、作業用の設定コピーと一緒に捨てると、次の Execution が失効したトークンで始まって認証エラーになる。次のとおり扱う。

| 項目 | 内容 |
|---|---|
| 対象の認証ファイル | ハーネスごとに1つだけ。Codex は `auth.json`、Claude Code は `.credentials.json`。どちらも CLI の設定ディレクトリ直下に置く |
| 認証の保管場所 | Evaloom 専用の認証ファイルを、ハーネスごとに1つ Runner のデータディレクトリ（Git 管理外）に置く。パスは Runner の設定ファイルの `harnessAuthFiles` で指定する（[`05-runner.md`](./05-runner.md) 4.1）。本人の普段の認証ファイル（`~/.codex/`、`~/.claude/`）は使わない。ファイルの権限は 0600 とする |
| 渡し方 | 作業用の設定コピーを作るとき、保管場所の認証ファイルをコピーの対象パスに置く。読み取り専用の専用設定ディレクトリ（Profile から作るもの）には置かない。保管場所に認証ファイルが無い、または下の検査に通らない場合は CLI を起動せず `config_error` で止める |
| 読み込んだ構成の記録との関係 | 認証ファイルは Profile の設定ではないので、context_revisions に内容もハッシュも保存しない。CLI を起動する前の照合では、コピーのうち認証ファイル以外の全ファイルが記録と一致することと、認証ファイルが対象パスに1つだけあることを確かめる。認証ファイルの内容は照合に使わない |
| 書き戻す時機 | Sandbox が止まり、CLI の書き込みが終わったことを確かめた後、作業用の設定コピーを消す前に行う。Execution の結果（成功・失敗・中止・タイムアウト）に関係なく行う。失敗した Execution の中でもトークンは更新されうるためである |
| 書き戻す前の検査 | コピーの認証ファイルが symlink ではない通常ファイルであること、大きさが 64 KiB 以下であること、内容が JSON のオブジェクトとして読めることを確かめる。通らなければ書き戻さず、保管場所の認証ファイルをそのまま残す。理由を Runner のログに残す（認証ファイルの内容はログに出さない） |
| 書き戻し方 | 内容が保管場所と同じなら何もしない。違えば、保管場所と同じディレクトリに一時ファイル（0600）を書き、ディスクへの書き出しを確かめてから名前を付け替えて置き換える。途中で失敗しても、保管場所に壊れたファイルを残さない |
| ログからの除外 | Runner はログを行単位で除外してから保存・配信する（[`05-runner.md`](./05-runner.md) 3.4。CLI の実行中も逐次配信する）。除外する値は、Execution の開始時に読んだ認証ファイルの値と、各 CLI コマンド（認証確認と本実行）の終了ごとに作業用の設定コピーから読み直した値で、Execution の中で累積する。値の取り出しには JSON の文字列値を使う。読み直しに失敗したら、それ以降の CLI の出力は保存・配信しない。CLI が実行中に新しく発行し、Runner がまだ読んでいない値は、その時点までに配信した行からは除外できない。この残りの危険は、CLI が認証の値を出力しないことに頼る |
| 同時実行 | 同じハーネスの認証ファイルを使う Execution は、同時に1本までにする（全体の同時実行上限2本、[`05-runner.md`](./05-runner.md) は変えない）。Runner は、保管場所の認証ファイルを読む前にハーネスごとの排他ロック（OS のファイルロック）を取り、書き戻しを終えてから外す。ロックを待っている Execution は、中止指示とタイムアウトで待つのをやめる |

書き戻さなかった場合（検査に通らない、書き込みに失敗した）でも、Execution の結果は変えない。次の Execution が認証エラーになったら、`config_error` として扱う（3.7）。

「HOME を見せない」ことを主な対策とし、環境変数による設定ディレクトリの指定は補助とする。環境変数だけに頼ると、CLI の版の違いで別の場所を読む可能性が残るためである。

実際に読み込んだ指示の検証は [`14-security-tests.md`](./14-security-tests.md) 3.1 で行う（requirements 7.5, 14.2）。

### 3.5 Codex Adapter

| 項目 | 内容 |
|---|---|
| 起動 | 非対話モード（`codex exec` を候補とする）。引数・フラグは採用する版で確定する |
| 作業ディレクトリ | Sandbox 内の worktree |
| 設定 | `CODEX_HOME` を 3.4 の作業用の設定コピーに向ける。codex-cli 0.163.0 は `CODEX_HOME` に書き込めないと起動しない（`failed to initialize in-process app-server client: Read-only file system`。2026-11-05 に [`14-security-tests.md`](./14-security-tests.md) 3.1 の実モデル試験で確認） |
| Project Context | Codex は作業ディレクトリの `AGENTS.md` を読む。`NativeInstructionFiles` に `AGENTS.md` を宣言する（実装時に確認） |
| ツール権限 | Harness の Tool Permissions と Profile の toolConfiguration を、CLI の設定に反映する |
| 認証 | Q7（サブスクリプションか API キーか）と Q8（値をハーネスから見えないようにする方式）の結論に従う（[`09-sandbox.md`](./09-sandbox.md) 3.5） |

### 3.6 Claude Code Adapter

| 項目 | 内容 |
|---|---|
| 起動 | 非対話モード（`claude -p` と構造化出力を候補とする）。引数・フラグは採用する版で確定する |
| 作業ディレクトリ | Sandbox 内の worktree |
| 設定 | `CLAUDE_CONFIG_DIR` を 3.4 の作業用の設定コピーに向ける。認証トークンの書き戻し（3.4）に、`.credentials.json` を書き込めるコピーが要るため |
| Project Context | Claude Code が作業ディレクトリから読む指示ファイル（`CLAUDE.md` 等）を `NativeInstructionFiles` に宣言する。`AGENTS.md` を CLI が自分で読まない場合は、Context Builder が固定した `AGENTS.md` の内容を渡す（実装時に確認） |
| ツール権限 | Harness の Tool Permissions と Profile の toolConfiguration を、CLI の許可設定に反映する |
| 認証 | Codex と同じく Q7 / Q8 に従う |

### 3.7 エラー分類の対応（requirements 9.5）

| 状況 | errorClass |
|---|---|
| CLI が見つからない、設定ファイルが不正、認証情報が無い | `config_error` |
| CLI が0以外で終了した、出力を解釈できない | `execution_failed` |
| Runner がタイムアウトで止めた | `timeout` |
| Runner が中止指示で止めた | `cancelled` |

`test_failed` は Adapter ではなく Gate 2 が付ける（[`10-verification-gates.md`](./10-verification-gates.md)）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 初期対象 | Codex、Claude Code | requirements 9.2 |
| ハーネスの作業範囲 | commit まで。push・PR 作成は Runner | requirements 8.5 |
| 実行場所 | Runner の許可済みディレクトリ（worktree）のみ | requirements 9.4 |
| 本人のグローバル設定・個人メモリ | 読み込ませない | requirements 7.5 |
| Capability | ハーネスごとに宣言し、無理に共通化しない | requirements 9.3 |
| 共通契約のエラー分類 | 設定不備、実行失敗、テスト失敗、タイムアウト、中止 | requirements 9.5 |
| Adapter の追加 | 中央のコードは登録処理以外を変更しない | requirements 20章 条件31 |

## 5. インターフェース

- 共通契約: HarnessRequest / HarnessResult（[`02-data-model.md`](./02-data-model.md) 5.7 / 5.8）
- エラー分類: [`02-data-model.md`](./02-data-model.md) 5.4
- Adapter 名: [`02-data-model.md`](./02-data-model.md) 4.1
- Sandbox の呼び出し: [`09-sandbox.md`](./09-sandbox.md) 5章

## 6. エラー処理

- CLI の異常終了は 3.7 の表で分類し、HarnessResult の `status=failed` として返す。Adapter 自身は panic しない。
- `Cancel` を呼んだ後、一定時間でプロセスが終わらなければ Sandbox ごと強制終了する（[`05-runner.md`](./05-runner.md) 3.6）。
- 出力に秘密情報が混ざる可能性に備え、ログは Runner の除外処理を通してから保存する（[`05-runner.md`](./05-runner.md) 3.5）。

## 7. スコープ外

- Gemini CLI、Factory Droid、OpenClaw、独自ハーネス（requirements 9.2 の将来候補）
- セッションの再開（Resume）を使った中断からの継続
- 調査・文書タスク向けの入出力

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`02-data-model.md`](./02-data-model.md) 共通契約、エラー分類
- [`03-registry.md`](./03-registry.md) Harness と Tool Permissions の登録
- [`05-runner.md`](./05-runner.md) 工程8、ログ収集、タイムアウト
- [`07-context.md`](./07-context.md) Execution Context と Harness Context
- [`09-sandbox.md`](./09-sandbox.md) Harness Sandbox と認証情報
- [`14-security-tests.md`](./14-security-tests.md) 設定環境の分離テスト

## 9. 実装単位

- [ ] [Backend] Adapter インターフェース、Capabilities、登録表
  - 受け入れ基準: Runner が Adapter 名だけで実装を引ける; 未登録の名前でエラーになる
- [ ] [Backend] ダミー Adapter による拡張試験（条件31）
  - 受け入れ基準: テスト用のダミー Adapter を新しいパッケージと登録の1行だけで追加し、その Adapter で Execution が succeeded まで通るテストがある
- [ ] [Backend] 専用設定環境の作成と記録（専用 HOME、設定ディレクトリの環境変数、ハーネス設定の配置、ハッシュ記録）
  - 受け入れ基準: Sandbox 内から本人の HOME 配下が見えない; 専用設定ディレクトリのファイル一覧とハッシュが context_revisions に保存される
- [ ] [Backend] HarnessResult の組み立て（headSha・commits・diff を Runner が worktree から取得）
  - 受け入れ基準: CLI の出力に関係なく、headSha が worktree の HEAD と一致する; diff の artifact が保存される
- [ ] [Backend] Codex Adapter
  - 受け入れ基準: テスト用プロジェクトの worktree で Codex を非対話で起動し、commit が1つ以上作られる; 中止で止まる; 異常終了が 3.7 の分類で返る
- [ ] [Backend] Claude Code Adapter
  - 受け入れ基準: テスト用プロジェクトの worktree で Claude Code を非対話で起動し、commit が1つ以上作られる; 中止で止まる; 異常終了が 3.7 の分類で返る
- [ ] [Backend] 作業用の設定コピー（3.4。設定ディレクトリへの書き込みが要る CLI 向け。Codex で使う）
  - 受け入れ基準: Codex Adapter が CODEX_HOME を Execution ごとの書き込み可能な設定コピーに向け、元の専用設定ディレクトリは読み取り専用のまま変わらない; コピーのハッシュが記録と一致しないと CLI を起動せず config_error になる; コピーは Execution の終了時（成功・失敗・中止）に消える; Claude Code Adapter と Test Sandbox の書き込み範囲は変わらない
- [ ] [Backend] 認証ファイルの受け渡しと書き戻し（3.4。Codex と Claude Code。同じハーネスの認証を使う Execution を同時に1本までにする）
  - 受け入れ基準: Codex Adapter と Claude Code Adapter が、保管場所の認証ファイルを作業用の設定コピーの対象パス（auth.json / .credentials.json）に置いてから CLI を起動し、Claude Code Adapter も CLAUDE_CONFIG_DIR を作業用の設定コピーに向ける; 保管場所に認証ファイルが無いと CLI を起動せず config_error になる; CLI がコピーの認証ファイルを書き換えた場合、Execution の終了時（成功・失敗・中止・タイムアウト）に保管場所の内容が書き換え後と一致する; JSON のオブジェクトとして読めない・64 KiB を超える・symlink の認証ファイルは書き戻さず、保管場所の内容が変わらない; 書き込みが途中で失敗しても保管場所の認証ファイルは書き戻し前の内容のまま読める; context_revisions に認証ファイルのパス・内容・ハッシュが保存されない; 同じハーネスの Execution を2本同時に始めると、2本目は1本目の書き戻しが終わるまで保管場所の認証ファイルを読まない; ロックを待っている Execution が中止指示で止まる; 開始時に読んだ認証の値と、認証確認の終了後に読み直した値（本実行で出力したもの）が Runner のログファイル・events に出ない; 通常のログ行は CLI の実行中にも逐次保存・配信される
- [ ] （人間） Claude Code・Codex の認証方式と、各社規約での自動実行の可否を確認して決める（Q7）
