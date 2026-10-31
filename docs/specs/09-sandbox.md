---
status: confirmed
範囲: Harness Sandbox と Test Sandbox の隔離条件、隔離方式の選択、ネットワークポリシー、秘密情報とモデル API 認証情報の扱い、ホスト側 Git 操作の防御
---

# 09. Sandbox・実行権限 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: requirements 15.5〜15.7 の権限境界のうち、ハーネスとテストを実行する隔離環境

## 1. 目的

- ハーネスが生成・変更できるコードを、セットアップやテストも含めて隔離環境で実行する（requirements 15.1 Untrusted Code Execution）。
- ハーネスと Test Sandbox から、Control API・管理 DB・秘密情報・認証情報に届かないようにする（requirements 15.7、条件14, 16）。
- 危険な操作を指示で禁じず、権限の外に置く（requirements 1.3 原則13）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | worktree のパス、実行するコマンド、専用設定環境、ネットワークポリシーの種別、資源の上限 |
| 出力 | プロセスの終了コード、標準出力・標準エラー、worktree への変更 |

## 3. 処理詳細

### 3.1 2つの Sandbox

| | Harness Sandbox | Test Sandbox |
|---|---|---|
| 実行するもの | Codex / Claude Code（AI によるコード変更と commit） | セットアップコマンド、テストコマンド、テスト基準値の取得 |
| 書き込みできる場所 | 当該 Execution の worktree（と commit に必要な Git 領域、3.6） | 当該 Execution の worktree（テストの出力先を含む） |
| 秘密情報 | 渡さない（モデル API の接続手段だけ例外、3.5） | 一切渡さない |
| ネットワーク | 3.3 のハーネス用ポリシー | 3.3 のテスト用ポリシー |

### 3.2 隔離の条件（requirements 15.7）

両方の Sandbox に次の条件を設ける。

| 条件 | 確かめ方 |
|---|---|
| Control API へのアクセスを禁止する | Sandbox からホストのループバック（127.0.0.1 と ::1）への接続を遮断する。127.0.0.1 への限定だけでは境界にならないため（requirements 15.7） |
| 管理 DB へのアクセスを禁止する | Evaloom のデータディレクトリ（SQLite、ログ、artifacts）を Sandbox に見せない |
| 秘密情報・認証情報へのアクセスを禁止する | 本人の HOME、キーチェーン、SSH エージェントのソケット、Runner トークン、GitHub の認証情報を Sandbox に見せない。環境変数は許可リスト方式で渡す |
| 不要なネットワークアクセスを禁止する | 3.3 |
| ホスト側の機密ディレクトリをマウントしない | マウントするのは worktree と、3.6 の Git 領域と、専用設定環境だけ |
| 管理権限につながるインターフェースを渡さない | Docker ソケット、launchd、Runner の制御用ソケットを渡さない |
| CPU・メモリ・実行時間に上限を設ける | 方式ごとの資源制限で設定する。値は実装時に確定（Q15） |

同じ macOS ユーザーで動かす場合、API トークンを別ファイルに置くだけでは隔離にならない（requirements 15.7）。どの方式でも、上の条件を [`14-security-tests.md`](./14-security-tests.md) の隔離突破テストで実際に確かめる。

### 3.3 ネットワークポリシー（requirements 15.7）

ハーネス実行用とテスト実行用で分ける。

| 種別 | 許可 | 遮断 | 状態 |
|---|---|---|---|
| ハーネス用 | モデル API の接続先（3.5） | ホストのループバック、GitHub（push の手段を与えない）、その他すべて | 案 |
| テスト用（setup） | プロジェクトが依存関係の取得に使うパッケージレジストリ（許可先の一覧は Project ごとに登録） | ホストのループバック、その他すべて | 決定（Q28） |
| テスト用（テスト実行） | なし | すべて | 決定（Q28） |

setup で依存関係を取得するにはネットワークが要る場合がある（requirements 15.7 の `composer install` の例）。setup とテスト実行でポリシーを分けるか、どこまで許可するかは requirements に無いため、本人が上の表のとおり決めた（Q28、2026-10-29）。

### 3.4 隔離方式の選択（Q1）

requirements 21章で「ハーネスとテストを隔離する具体的な方式と、それぞれのネットワークポリシー」は詳細設計事項とされている。候補と判断基準は次のとおり。

| 方式 | 長所 | 短所 |
|---|---|---|
| コンテナ（Linux VM 上のコンテナ） | ファイル・ネットワーク・プロセスの分離をまとめて設定できる。ループバックが既定で分かれる | macOS 専用のツールチェーン（Xcode 等）が要るプロジェクトのテストを動かせない。ホストへの抜け道（`host.docker.internal` 等）を明示的に塞ぐ必要がある。Docker ソケットを渡さない運用が要る |
| 別の macOS ユーザー + macOS のサンドボックスプロファイル | ホストと同じツールチェーンで動く | ネットワークの宛先単位の制御が弱い。ファイル権限とプロファイルの設定漏れが起きやすい |
| VM（macOS または Linux） | 分離が最も強い | 起動が重い。ディスクとメモリを多く使う |

判断基準:

1. 3.2 の条件をすべて満たし、[`14-security-tests.md`](./14-security-tests.md) の隔離突破テストに合格すること（必須）
2. 対象プロジェクトのセットアップとテストが動くこと
3. 3.3 のネットワークポリシーを宛先単位で表せること
4. Execution ごとの起動時間と資源の使用量

コンテナで実行するだけで安全になるわけではない。ファイル共有、ネットワーク、権限の設定まで含めて検証する（requirements 15.7）。

決定（Q1、2026-10-29、一部）: macOS 専用のツールチェーンが要らない Project はコンテナで隔離する。Xcode 等が要る Project の方式は、次の第一案を実測してから決める。決まるまでその種の Project は対象外とする。

- 第一案: 実行ごとに Tart（Apple の Virtualization.framework を使う VM ツール）で macOS VM をクローンし、終わったら削除する。ネットワークは Softnet で CIDR 単位に許可・遮断する（setup はレジストリの IP だけ許可、テスト実行は `0.0.0.0/0` を遮断）。マウントは作業ツリーだけを `--dir` で渡す。
- 別ユーザー + macOS のサンドボックスプロファイルを採らない理由: 宛先単位の制御にはホストのループバック上のプロキシが要り、3.2 の「ホストのループバックへの接続を遮断する」と両立しない。
- 第一案の制約: macOS ゲストの同時起動は Apple のライセンスで 2 台まで。Softnet の指定は CIDR だけでドメイン名では書けない。Softnet は SUID か passwordless sudo を要する。
- 実測すること: VM 内で `xcodebuild test` とシミュレータが動くか、クローンから起動までの時間とディスク使用量。

### 3.5 秘密情報とモデル API の認証情報（requirements 15.6）

- Harness Sandbox、Test Sandbox には、管理用の認証情報もプロジェクトの API キーや `.env` の中身も渡さない。
- 本物の秘密情報を使うのは Trusted Runner と Git Publisher の中だけで、用途は Evaloom 自身の操作（fetch、push、PR 作成）に限る。
- テストに値が必要な場合は、テスト専用のダミーの値を使う。
- 例外はハーネスがモデル API へ接続するための認証情報である（Q8）。
  1. 案: Sandbox の外に認証を付けるプロキシを置き、Sandbox からはプロキシ経由でだけモデル API に届くようにする。ハーネスから認証情報の値は見えない。
  2. 案: Evaloom 専用で、用途と利用上限を絞った認証情報を渡す。
  3. 決定（Q7・Q8、2026-10-29）: Q7 でサブスクリプションを採ったため 2 とする。サブスクリプションのログイン情報は CLI 自身が持つので、1 のプロキシでは値を隠せない。ハーネスからは認証情報の値が見えるため、本人の普段の認証情報とは分け、Evaloom 専用のものだけを専用設定環境（[`08-harness-adapter.md`](./08-harness-adapter.md)）に置く。

### 3.6 Git 領域とホスト側の Git 操作の防御

ハーネスは commit するために、worktree が参照する Git の共有領域（オブジェクト、ref）に書き込む必要がある。このため、ハーネスはその Git 領域の設定やフックも書き換えられる。requirements 1.3 原則14「AI が書き換えられるコードも信用しない」に従い、次を必須とする。

1. **ホスト側の Git 操作でフックと実行系の設定を無効にする**: Trusted Runner と Git Publisher がホスト上で Git を実行するときは、フック（`core.hooksPath` を空のディレクトリに向ける）と、外部プログラムを起動しうる設定（`core.fsmonitor`、`core.sshCommand`、`credential.helper`、filter / diff / merge のドライバ、`core.pager`、`gpg.program` など）を無効にする。対象設定の一覧は実装時に確定する。
2. **本人のローカルリポジトリの Git 領域をハーネスに見せない**: Evaloom が管理する作業用クローンを Project ごとに Worktree Root 配下に置き、worktree はそこから作る（Q27 で決定、2026-10-29、[`06-worktree.md`](./06-worktree.md) 3.2.2）。本人のローカルリポジトリの `.git` を共有する worktree にすると、ハーネスが本人のブランチの ref や設定を書き換えられるためである。[`06-worktree.md`](./06-worktree.md) の `worktree add` は作業用クローンに対して行い、本人のローカルリポジトリはコピー許可リストの読み取り元としてだけ使う。
3. **ハーネスの自己申告を使わない**: HEAD、commit、差分は Runner が取得する（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.3）。

### 3.7 Runner が行わない操作（requirements 15.3）

承認の仕組みを導入する Phase 2 までは、Evaloom とハーネスは requirements 15.3 の承認対象操作を実行しない。指示で禁じるのではなく、次の権限で拒否する。

| 操作 | 拒否の仕組み |
|---|---|
| PR マージ | ハーネスには GitHub の認証情報もネットワーク経路も無い。Git Publisher のコードにマージの処理を持たせず、GitHub 側の設定でもベースブランチを更新できないようにする（[`11-git-publisher.md`](./11-git-publisher.md) 3.5） |
| 本番環境へのデプロイ、外部メール送信 | Sandbox にその認証情報とネットワーク経路が無い。Runner にその処理を持たせない |
| 重要ファイル削除 | ハーネスが書き込めるのは worktree（と 3.6 の Git 領域）だけ |
| 認証・権限変更、共通契約・評価基準・中央セキュリティポリシーの変更 | Sandbox から Control API に届かない。Runner トークンでは権限設定を変更する API を呼べない（[`01-architecture.md`](./01-architecture.md) 3.4） |

### 3.8 GitHub 側の CI（requirements 15.7）

ハーネスは CI の定義やテストを変更できるため、PR に対して動く CI も未信頼のコードとして扱う。

- ハーネスが変更できるコードで動く CI には、秘密情報、リポジトリへの書き込み権限、デプロイの権限を渡さない。設定は本人が各リポジトリで行う。
- CI の定義の変更は PR のレビューで本人が確認する（requirements 8.5、Q36）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| setup・テストの実行場所 | Test Sandbox。ホストで直接実行しない | requirements 8.6, 15.7 |
| Sandbox への本物の秘密情報 | 一切渡さない（モデル API の接続手段のみ例外） | requirements 15.6 |
| 秘密情報を使う場所 | Trusted Runner と Git Publisher の中だけ | requirements 15.6 |
| ネットワークポリシー | ハーネス用とテスト用で分ける | requirements 15.7 |
| 隔離条件 | 3.2 の7項目 | requirements 15.7 |
| 承認対象操作（Phase 1） | Runner の実行権限で拒否する | requirements 15.3 |
| 隔離の検証 | 実際にアクセスを試みる自動テストで確かめる | requirements 15.7, 14.2 |

## 5. インターフェース

Runner 内部のインターフェース（案）:

```go
type SandboxKind string // "harness" | "test_setup" | "test_run"

type SandboxSpec struct {
    Kind        SandboxKind
    Worktree    string            // 書き込み可能
    GitDir      string            // commit に必要な Git 領域（harness のみ書き込み可）
    ConfigDir   string            // 専用設定環境（harness のみ）
    Env         map[string]string // 許可リスト方式
    Network     NetworkPolicy
    Limits      ResourceLimits    // CPU、メモリ、実行時間
}

type Sandbox interface {
    Run(ctx context.Context, spec SandboxSpec, cmd []string) (Process, error)
}
```

Adapter（[`08-harness-adapter.md`](./08-harness-adapter.md)）と Gate 2・setup（[`06-worktree.md`](./06-worktree.md)、[`10-verification-gates.md`](./10-verification-gates.md)）はこのインターフェースだけを使う。隔離方式（Q1）を変えても呼び出し側は変えない。

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Sandbox を作れない | `failed`（`sandbox_error`）。ホストでの直接実行に切り替えない（Fail Closed） |
| 資源上限に達して強制終了された | 実行時間なら `timed_out`、それ以外は `failed`（`sandbox_error`） |
| 隔離の設定を確認できない（プロキシが起動していない等） | ハーネスを起動しない。`failed`（`sandbox_error`） |

## 7. スコープ外

- 本番デプロイ・メール送信などの承認付き実行（Phase 2 以降）
- 複数 Runner 間のネットワーク構成（Phase 6）
- 遠隔アクセスの認証・ネットワーク構成（requirements 15.5）

## 8. 関連仕様

- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) 構成要素と権限の表
- [`06-worktree.md`](./06-worktree.md) setup の実行と作業用クローン
- [`08-harness-adapter.md`](./08-harness-adapter.md) 専用設定環境
- [`10-verification-gates.md`](./10-verification-gates.md) テストの実行
- [`11-git-publisher.md`](./11-git-publisher.md) GitHub の認証情報の扱い
- [`14-security-tests.md`](./14-security-tests.md) 権限境界テスト、隔離突破テスト

## 9. 実装単位

- [ ] （人間） 隔離方式（Q1）、モデル API 認証の方式（Q8）、setup とテストのネットワークポリシー（Q28）、作業用クローン案（Q27）を決める
- [ ] [Backend] Sandbox インターフェースと、採用した方式による実装（ファイルのマウント範囲、環境変数の許可リスト、資源上限）
  - 受け入れ基準: Sandbox 内から本人の HOME、Evaloom のデータディレクトリ、Runner トークンのファイルが見えない; 許可リストに無い環境変数が Sandbox に渡らない; 実行時間の上限で止まる
- [ ] [Backend] ネットワークポリシー（ハーネス用、setup 用、テスト用）とループバックの遮断
  - 受け入れ基準: どの Sandbox からも 127.0.0.1 の Control API に接続できない; ハーネス用 Sandbox から github.com に接続できない; テスト実行用 Sandbox から外部に接続できない
- [ ] [Backend] モデル API の認証付与プロキシ（Q8 の第一案を採る場合）
  - 受け入れ基準: Sandbox 内の環境変数とファイルにモデル API の認証情報の値が無い状態で、ハーネスがモデル API を使える
- [ ] [Backend] ホスト側 Git 操作の安全なラッパー（フック無効化、実行系設定の無効化）
  - 受け入れ基準: worktree の Git 領域に post-checkout などのフックや core.fsmonitor を仕込んでも、Runner と Git Publisher の Git 操作でそれらが実行されない
- [ ] [Backend] 本人のローカルリポジトリの保護の確認（Q27 で A を採用した場合。作業用クローン自体は [`06-worktree.md`](./06-worktree.md) で実装する）
  - 受け入れ基準: ハーネスが Git 領域の ref を書き換えても、本人のローカルリポジトリのブランチと設定が変わらない
