---
status: confirmed
範囲: Phase 1 の自動セキュリティテスト（設定環境の分離、権限境界、隔離突破、ホスト側 Git 操作）と、Gate の受け入れテスト
---

# 14. セキュリティテスト・受け入れテスト 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: requirements 14.2 のうち Phase 1 で必須のテストと、条件15・17・18 を確かめる受け入れテスト

## 1. 目的

- 権限設定が正しいと思い込まず、突破できないことをテストで確かめる（requirements 15.7）。
- 受け入れ条件 11・14・16（セキュリティ）と 15・17・18（Gate）を、繰り返し実行できる自動テストで確かめる。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | テスト用のプロジェクトリポジトリ（fixture）、テスト用のリモート（ローカルの bare リポジトリ）、プローブ用の Adapter、実際の Codex / Claude Code（3.1 のみ） |
| 出力 | テスト結果。失敗時は、どのアクセスが通ってしまったかの記録 |

## 3. 処理詳細

### 3.0 共通の道具

| 道具 | 内容 |
|---|---|
| プローブ Adapter | AI を呼ばず、決まったスクリプトを Harness Sandbox で実行するテスト用 Adapter（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.2 のダミー Adapter を拡張）。結果が決定的になる |
| プローブスクリプト | 3.2 / 3.3 の各アクセスを試み、成否を JSON で worktree 内に書き出す |
| fixture プロジェクト | テストコマンドと JUnit XML 出力を持つ小さなリポジトリ。setup とテストにプローブを仕込んだ版を用意する |
| テスト用リモート | ローカルの bare リポジトリ。push されたかどうかを ref で確かめる |
| カナリア | 読み込まれたら出力に現れる一意の文字列 |

プローブの結果ファイルはハーネス側が書くため、改ざんされうる。判定では結果ファイルだけでなく、Control Plane 側のアクセス記録（audit_logs、HTTP のアクセスログ）とテスト用リモートの ref も合わせて確かめる。

### 3.1 設定環境の分離テスト（条件11、requirements 7.5, 14.2）

Evaloom から起動したハーネスが、本人のグローバル設定を読み込んでいないことを確かめる。

1. テスト用の「本人の HOME」を用意し、`~/.claude/CLAUDE.md`、`~/.codex/AGENTS.md` 等の位置に、カナリア A を出力させる指示を置く。本人の実際の HOME のファイルは変更しない。
2. Profile の設定（専用設定環境に置くもの）に、カナリア B を出力させる指示を置く。
3. 実際の Codex と Claude Code をそれぞれ Execution として起動する。
4. 次をすべて満たせば合格とする。
   - 出力にカナリア A が現れない
   - 出力にカナリア B が現れる（専用設定環境が実際に読まれていることの確認。これが無いと「何も読んでいない」場合も合格してしまう）
   - CLI が読み込んだ設定ファイルを報告できる場合、その一覧に本人の HOME 配下のパスが無い
   - Sandbox 内から、本人の HOME 配下のパスが存在しない、または読めない

このテストはモデル API を使うため、認証情報が用意された Mac mini 上で実行する。CLI の版を上げたときに必ず実行する。

### 3.2 権限境界テスト（条件14、requirements 14.2）

プローブ Adapter で、Harness Sandbox から次を試み、すべて失敗することを確かめる。

| 試すこと | 合格条件 |
|---|---|
| 127.0.0.1 / ::1 上の Control API（本人用・Runner 用）への HTTP 接続 | 接続できない。Control Plane のアクセスログに記録が無い |
| 管理 DB（SQLite）のファイルの読み書き | パスが見えない、または読めない |
| Evaloom のデータディレクトリ（ログ、artifacts）の読み取り | 同上 |
| Runner トークンのファイルの読み取り | 同上 |
| GitHub の書き込み認証情報（Git Publisher の設定、キーチェーン、SSH エージェントのソケット）の取得 | 見えない、使えない |
| 環境変数の一覧 | 許可リスト以外の変数が無い。既知の秘密の値が含まれない |
| テスト用リモートへの `git push` | 失敗する。テスト用リモートの ref が変わらない |
| github.com への接続 | 接続できない |
| モデル API の認証情報の値の取得（Q8 で第一案を採った場合） | 環境変数・ファイルに値が無い |

### 3.3 隔離突破テスト（条件16、requirements 14.2）

fixture プロジェクトのセットアップコマンドとテストスクリプトに、管理 DB や Control API へアクセスする処理を仕込み、Test Sandbox から実際にアクセスできないことを確かめる（requirements 15.7）。

| 試すこと | 実行する場所 | 合格条件 |
|---|---|---|
| Control API への接続 | setup、テスト | 接続できない。アクセスログに記録が無い |
| 管理 DB の読み書き | setup、テスト | 見えない、読めない |
| 秘密情報（Runner トークン、GitHub 認証情報、本人の HOME、プロジェクトの `.env`） | setup、テスト | 見えない、読めない |
| Docker ソケット等の管理インターフェース | setup、テスト | 存在しない |
| 外部ネットワーク | テスト実行 | 接続できない（Q28 の結論に従う） |
| 外部ネットワーク | setup | Q28 で許可した宛先以外に接続できない |

### 3.4 ホスト側 Git 操作の防御テスト（[`09-sandbox.md`](./09-sandbox.md) 3.6）

1. プローブ Adapter で、worktree が参照する Git 領域に、実行されたら印のファイルを作るフック（post-checkout、pre-push、reference-transaction など）と、`core.fsmonitor` などの実行系設定を書き込む。
2. Runner が Gate 1〜3 と push を進める。
3. 印のファイルが作られないことを確かめる。
4. 作業用クローン案（Q27）を採った場合、本人のローカルリポジトリの ref と設定が変わらないことも確かめる。

### 3.5 Gate の受け入れテスト（条件15, 17, 18）

プローブ Adapter に、決まった変更を commit させて Execution を通す。どのケースでも「テスト用リモートに push されない」ことをリモートの ref で確かめる。

| ケース | 期待する結果 | 条件 |
|---|---|---|
| 既存のテストファイルを削除 | blocked（gate1_test_change）、通知が要求される | 17 |
| 既存のテストの assertion を変更 | blocked（gate1_test_change）、通知 | 17 |
| 新規テストに `.skip(` を追加 | blocked、通知 | 17 |
| 新規テストに `describe.only` を追加 | blocked（Gate 1）。Gate 1 を続行で通した場合も Gate 2 で blocked（gate2_missing_tests） | 17, 18 |
| テストの実行設定（`package.json` の test スクリプト等）を変更 | blocked、通知 | 17 |
| CI の定義（`.github/workflows`）を変更 | blocked、通知 | 17 |
| fixture のテスト設定で既存テストの一部を実行対象から外す変更を、Gate 1 のパターンに掛からない形で行う | blocked（gate2_missing_tests） | 18 |
| skip 数が増える | blocked（gate2_missing_tests） | 18 |
| Gate 2 の後に commit を追加する（テスト用のフックで差し込む） | blocked（gate3_mismatch） | 15 |
| Gate 2 の後に未追跡ファイルを置く | blocked（gate3_mismatch） | 15 |
| 実装コードだけを変更し、テストが通る | push され、リモートの SHA が verified_sha と一致し、PR が作られる（GitHub 部分はテスト用の偽 API） | 12, 15 |

### 3.6 テストの置き場所と実行

| テスト | 置き場所 | 実行のタイミング |
|---|---|---|
| 3.2〜3.5 | evaloom リポジトリ（中央の横断テスト、requirements 14.3） | 変更のたびに Mac mini 上で実行する。Sandbox の実体が要るため |
| 3.1 | evaloom リポジトリ | モデル API の認証情報が要るため、Mac mini 上で手動または定期実行する。CLI の版を上げたとき・Sandbox の設定を変えたときは必須 |

どのテストを `make verify` などの通常の検証に含めるかは実装時に確定する。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| 隔離の検証方法 | 実際にアクセスを試みる自動テスト | requirements 15.7 |
| 設定環境の分離の検証 | ハーネスが実際に読み込んだ指示を検証するテスト | requirements 7.5, 14.2 |
| Phase 1 で必須のテスト分類 | 設定環境の分離テスト、権限境界テスト、隔離突破テスト | requirements 14.2 |
| 横断テストの置き場所 | 中央リポジトリ | requirements 14.3 |

## 5. インターフェース

- プローブ Adapter: [`08-harness-adapter.md`](./08-harness-adapter.md) 3.1 のインターフェースを実装する
- Sandbox: [`09-sandbox.md`](./09-sandbox.md) 5章
- 判定に使う記録: audit_logs、gate_results、execution_blocks（[`02-data-model.md`](./02-data-model.md)）

## 6. エラー処理

- テストの前提（Sandbox の実体、モデル API の認証情報）が無い環境では、テストを「スキップ」ではなく「実行不能」として失敗扱いにする。黙ってスキップして合格に見えることを防ぐ。
- プローブの結果ファイルが無い場合は失敗とする。

## 7. スコープ外

- Contract Test、Integration Test、E2E Test、Regression Test、Evaluation Benchmark（Phase 3 以降、requirements 14.2）
- 非公開の受け入れテスト（Phase 3、requirements 11.6）
- テストのときだけ通る抜け道や、テストを少しずつ甘くする変更の検出（PR レビューで本人が確認）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) 認証と権限境界
- [`08-harness-adapter.md`](./08-harness-adapter.md) 専用設定環境、ダミー Adapter
- [`09-sandbox.md`](./09-sandbox.md) 隔離の条件
- [`10-verification-gates.md`](./10-verification-gates.md) Gate 1〜3
- [`11-git-publisher.md`](./11-git-publisher.md) push と PR
- [`12-notification.md`](./12-notification.md) Blocked の通知

## 9. 実装単位

- [ ] [Backend] テスト用の道具（プローブ Adapter、プローブスクリプト、fixture プロジェクト、テスト用 bare リモート、偽の GitHub API）
  - 受け入れ基準: プローブ Adapter で fixture プロジェクトの Execution が succeeded まで通る; 前提が無い環境ではテストが失敗扱いになる
- [ ] [Backend] 権限境界テスト（3.2）
  - 受け入れ基準: 3.2 の全項目がテストとして実装され、Mac mini 上で合格する; Sandbox の設定を意図的に緩めた状態（例: ループバックを許可）では該当項目が失敗する
- [ ] [Backend] 隔離突破テスト（3.3）
  - 受け入れ基準: setup とテストの両方に仕込んだアクセスがすべて失敗する; Control Plane のアクセスログに Sandbox からの要求が無い
- [ ] [Backend] ホスト側 Git 操作の防御テスト（3.4）
  - 受け入れ基準: 仕込んだフックと実行系設定が Runner と Git Publisher の Git 操作で実行されない
- [ ] [Backend] Gate の受け入れテスト（3.5）
  - 受け入れ基準: 3.5 の全ケースで期待する結果になり、Blocked のケースでテスト用リモートの ref が変わらない; 実装だけの変更のケースで push された SHA が verified_sha と一致する
- [ ] [Backend] 設定環境の分離テスト（3.1、実際の Codex / Claude Code を使う）
  - 受け入れ基準: Codex と Claude Code の両方で、カナリア A が出力に現れず、カナリア B が現れる
