---
status: draft
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
| 書き込みできる場所 | 当該 Execution の worktree（と commit に必要な Git 領域、3.6）、作業用の設定コピー（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.4。要る CLI のみ） | 当該 Execution の worktree（テストの出力先を含む） |
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
| CPU・メモリ・実行時間に上限を設ける | 方式ごとの資源制限で設定する。実行時間は Project の timeouts、無ければ Runner の `defaultTimeouts`（Q15 で決定）。CPU とメモリは Runner の設定ファイルの `container.cpus` / `container.memoryMiB`、既定は Sandbox 1つあたり CPU 4・メモリ 8 GiB（[`05-runner.md`](./05-runner.md) 4.1、案 Q68） |

同じ macOS ユーザーで動かす場合、API トークンを別ファイルに置くだけでは隔離にならない（requirements 15.7）。どの方式でも、上の条件を [`14-security-tests.md`](./14-security-tests.md) の隔離突破テストで実際に確かめる。

### 3.3 ネットワークポリシー（requirements 15.7）

ハーネス実行用とテスト実行用で分ける。

| 種別 | 許可 | 遮断 | 状態 |
|---|---|---|---|
| ハーネス用 | モデル API の接続先（3.5） | ホストのループバック、GitHub（push の手段を与えない）、その他すべて | 案 |
| テスト用（setup） | プロジェクトが依存関係の取得に使うパッケージレジストリ（許可先の一覧は Project ごとに登録） | ホストのループバック、その他すべて | 決定（Q28） |
| テスト用（テスト実行） | なし | すべて | 決定（Q28） |

setup で依存関係を取得するにはネットワークが要る場合がある（requirements 15.7 の `composer install` の例）。setup とテスト実行でポリシーを分けるか、どこまで許可するかは requirements に無いため、本人が上の表のとおり決めた（Q28、2026-10-29）。

許可先の持ち方（2026-11-06 追記）:

| 種別 | 許可先の一覧の置き場所 | 状態 |
|---|---|---|
| ハーネス用 | Adapter のパッケージの中の定数。Adapter が使う CLI が接続するモデル API と認証の更新先だけを `host:port` で並べる。Adapter を足すときは、そのパッケージの中で一覧も足す（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.2 の条件31の範囲に収まる）。一覧の候補は [`08-harness-adapter.md`](./08-harness-adapter.md) 3.5・3.6 | 案（Q71） |
| テスト用（setup） | projects.setup_allowed_hosts（`host:port` の配列、[`02-data-model.md`](./02-data-model.md)）。本人が Project の登録・更新で入れる。claim の `project.setupAllowedHosts` で Runner に渡る。空なら setup もネットワークを全遮断する | 案（Q73）。Q73 が決まるまで claim は常に空配列を返す |
| テスト用（テスト実行） | 持たない（全遮断） | 決定（Q28） |

宛先の書き方はコンテナ方式のプロキシの制約に合わせ、小文字の DNS ホスト名とポートの完全一致とする。ワイルドカード、IP リテラル、`localhost` などのローカル名は受け付けない。ハーネス用には `github.com` とそのサブドメインを入れない（push の手段を与えないため）。

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

決定（Q1、2026-10-29、一部）: macOS 専用のツールチェーンが要らない Project はコンテナで隔離する。Xcode 等が要る Project の方式は、次の第一案を実測してから決める（2026-11-05 に実測して採用した。下の「決定（Q1、2026-11-05、残り）」）。

- 第一案: 実行ごとに Tart（Apple の Virtualization.framework を使う VM ツール）で macOS VM をクローンし、終わったら削除する。ネットワークは Softnet で CIDR 単位に許可・遮断する（setup はレジストリの IP だけ許可、テスト実行は `0.0.0.0/0` を遮断）。マウントは作業ツリーだけを `--dir` で渡す。
- 別ユーザー + macOS のサンドボックスプロファイルを採らない理由: 宛先単位の制御にはホストのループバック上のプロキシが要り、3.2 の「ホストのループバックへの接続を遮断する」と両立しない。
- 第一案の制約: macOS ゲストの同時起動は Apple のライセンスで 2 台まで。Softnet の指定は CIDR だけでドメイン名では書けない。Softnet は SUID か passwordless sudo を要する。
- 実測すること: VM 内で `xcodebuild test` とシミュレータが動くか、クローンから起動までの時間とディスク使用量。

実測の結果（2026-11-05、Mac mini（macOS 27.2、14 コア、メモリ 64 GB）。Tart 2.41.1、Softnet 0.25.0、イメージ `ghcr.io/cirruslabs/macos-tahoe-xcode:latest`（ゲストは macOS 26.6.2、Xcode 27.2、iOS 27.2 シミュレータ）。VM は既定値の 4 CPU、メモリ 8 GB）:

| 項目 | 結果 |
|---|---|
| イメージの取得 | 初回の pull に 6079 秒。ホストのディスク上で 77 GB（仮想ディスクは 140 GB） |
| `tart clone` | 0.05〜0.06 秒。APFS のクローンなので、クローン直後のディスク増分はほぼ 0 |
| 起動から VM 内でコマンドを実行できるまで | 26.7〜28.2 秒（`tart exec` が応答するまで。NAT と Softnet で差は無い） |
| 実行中のディスク増分 | 起動して止めただけで約 0.4〜0.6 GB。`xcodebuild test` を3回流した後で約 2.8 GB。`tart delete` で解放される |
| `xcodebuild test`（iOS シミュレータ） | 動いた。Swift パッケージのテスト2件が iPhone 18 Pro のシミュレータで成功した。シミュレータの起動を含めて約 50 秒。`--dir` で渡した共有ディレクトリの上で直接ビルドしても成功した（約 48 秒） |
| `--dir` の共有 | ゲストの `/Volumes/My Shared Files/<名前>` に見え、ゲストから書き込める（`:ro` を付けると読み取り専用） |
| Softnet の既定（`--net-softnet` だけ） | インターネットに届く。LAN（192.168.1.1）には届かない。**ホスト（ゲートウェイの IP）で待ち受けるサービスには届く**。NAT の既定も同じくホストに届く |
| `--net-softnet-block=0.0.0.0/0 --net-softnet-allow=1.1.1.1/32` | 許可した 1.1.1.1 だけに届く。8.8.8.8、ホスト、LAN には届かない。ゲートウェイの DNS にも届かないため、名前解決が失敗する |
| 上の設定で、ゲストの DNS を許可した 1.1.1.1 に向けた場合 | 名前解決はできる（`sudo networksetup -setdnsservers`。イメージの admin ユーザーはパスワードなしで sudo を使える）。解決した先の IP は、許可していなければ届かない。ホストには届かないまま |
| ゲートウェイの IP | Softnet では起動ごとに帯が変わる（10.104.249.209、192.168.34.233 など）。ホストの IP を前もって許可・遮断の指定に書くことはできない |

実測から分かった制約:

- Softnet の既定と NAT の既定は、ホストで待ち受けるサービスに届く。3.2 の「ホストのループバックへの接続を遮断する」を満たすには、どの Sandbox でも `--net-softnet-block=0.0.0.0/0` を必ず付け、要る宛先だけを `--net-softnet-allow` で足す。
- `0.0.0.0/0` を遮断するとゲートウェイの DNS も止まる。setup で名前解決が要る場合は、許可した公開 DNS（例: 1.1.1.1）にゲストの DNS を向けてから setup を始める。テスト実行では DNS も通さない。
- 1台の起動に約 28 秒かかり、シミュレータの起動を含めると `xcodebuild test` に約 50 秒かかる。Harness Sandbox・setup・テスト実行で VM を作り直すと、起動だけで Execution ごとに約 1.5 分増える。

決定（Q1、2026-11-05、残り）: Xcode 等が要る Project は第一案（実行ごとに使い捨てる Tart の macOS VM + Softnet）で隔離する。上の実測で、`xcodebuild test` とシミュレータが VM 内で動き、宛先の許可・遮断も効いたためである。次を条件とする。

- どの Sandbox でも `--net-softnet-block=0.0.0.0/0` を付け、3.3 の表で許可する宛先だけを `--net-softnet-allow` で足す。Softnet の既定のままにしない（ホストに届くため）。
- setup で名前解決が要る場合は、許可した公開 DNS にゲストの DNS を向ける。テスト実行では DNS も通さない。
- macOS ゲストの同時起動は 2 台まで（Apple のライセンス）。全体の同時実行上限 2 本（[`05-runner.md`](./05-runner.md)）の中で収まるように、1本の Execution が同時に起動する VM は1台にする。
- 実装は、対象の Project を登録するときに起票する。それまで、Xcode 等が要る Project を登録しても Execution は始めない（現状どおり対象外として扱う）。

### 3.5 秘密情報とモデル API の認証情報（requirements 15.6）

- Harness Sandbox、Test Sandbox には、管理用の認証情報もプロジェクトの API キーや `.env` の中身も渡さない。
- 本物の秘密情報を使うのは Trusted Runner と Git Publisher の中だけで、用途は Evaloom 自身の操作（fetch、push、PR 作成）に限る。
- テストに値が必要な場合は、テスト専用のダミーの値を使う。
- 例外はハーネスがモデル API へ接続するための認証情報である（Q8）。
  1. 案: Sandbox の外に認証を付けるプロキシを置き、Sandbox からはプロキシ経由でだけモデル API に届くようにする。ハーネスから認証情報の値は見えない。
  2. 案: Evaloom 専用で、用途と利用上限を絞った認証情報を渡す。
  3. 決定（Q7・Q8、2026-10-29）: Q7 でサブスクリプションを採ったため 2 とする。サブスクリプションのログイン情報は CLI 自身が持つので、1 のプロキシでは値を隠せない。ハーネスからは認証情報の値が見えるため、本人の普段の認証情報とは分け、Evaloom 専用のものだけを専用設定環境（[`08-harness-adapter.md`](./08-harness-adapter.md)）に置く。
  4. 規約の確認（Q7、2026-11-05）: 両社とも自動実行には API キーを推奨するが、サブスクリプションでの自動実行も次の範囲で使えると判断した。
     - Claude Code: サブスクリプションのログインは、プランの購入者が改変していない Claude Code を使うためのもので、第三者が利用者の認証を預かる使い方を禁じている。Evaloom は本人が自分のログインで使うので当たらない。プランの利用上限は通常の個人利用を前提にしている（Claude Code Docs「Legal and compliance」）。
     - Codex: ChatGPT のログインでの自動実行は、信頼できる非公開の自動化に限った例外として手順が公開されている。公開リポジトリでは使わない、`auth.json` を同時に実行するジョブで共有しない、更新されたトークンを書き戻して上書きしない、が条件である（OpenAI「Codex CI/CD auth」）。
     - 運用の条件: Evaloom を使うのは本人だけで、対象は本人の非公開リポジトリとする。同じ認証情報を2本の Execution で同時に使わない（同時実行は全体で2本まで、[`05-runner.md`](./05-runner.md)）。利用上限に当たるようになったら、API キーへの切り替えを見直す。
     - 書き戻しと同時実行の守り方（2026-11-05 決定）: Execution の終了時に、作業用の設定コピーの認証ファイル（Codex は `auth.json`、Claude Code は `.credentials.json`）1つだけを、JSON の形と大きさを確かめてから Evaloom 専用の認証の保管場所へ書き戻す。同じハーネスの認証を使う Execution は同時に1本までにする（全体の上限2本は変えない）。認証ファイルは読み込んだ構成の記録（context_revisions）に含めない。詳細は [`08-harness-adapter.md`](./08-harness-adapter.md) 3.4。

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
| 重要ファイル削除 | ハーネスが書き込めるのは worktree（と 3.6 の Git 領域、Execution ごとに捨てる作業用の設定コピー）だけ |
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
    ConfigWritable bool           // ConfigDir が作業用の設定コピーのときだけ true（書き込み可でマウントする）
    Env         map[string]string // 許可リスト方式
    Network     NetworkPolicy
    Limits      ResourceLimits    // CPU、メモリ、実行時間
}

type Sandbox interface {
    Run(ctx context.Context, spec SandboxSpec, cmd []string) (Process, error)
}
```

Adapter（[`08-harness-adapter.md`](./08-harness-adapter.md)）と Gate 2・setup（[`06-worktree.md`](./06-worktree.md)、[`10-verification-gates.md`](./10-verification-gates.md)）はこのインターフェースだけを使う。隔離方式（Q1）を変えても呼び出し側は変えない。

Runner が `SandboxSpec` に入れる値（2026-11-06 追記、決定）:

| 項目 | 値 | 理由 |
|---|---|---|
| setup・テスト・基準値のコマンド | `["/bin/sh", "-c", <Project Registry のコマンド文字列>]` | コマンドは projects.setup_command / test_command に1つの文字列で登録する（[`02-data-model.md`](./02-data-model.md)）。コンテナのイメージは Linux（[`05-runner.md`](./05-runner.md) 4.1 の `container.image`）なので、POSIX のシェルの `/bin/sh` で解釈する。既存の Adapter の認証確認も Sandbox 内の `/bin/sh` を使っている |
| `Env`（setup・テスト・基準値） | Runner からは足さない（空） | 3.2 の許可リスト方式の最小。テストに値が要る場合は、コピー許可リストのダミーの値を使う（3.5） |
| `Env`（ハーネス） | 専用設定環境の変数（`CODEX_HOME` / `CLAUDE_CONFIG_DIR`）と、commit の作者情報を与える Git の設定の変数（`GIT_CONFIG_COUNT`、`GIT_CONFIG_KEY_n`、`GIT_CONFIG_VALUE_n`。[`05-runner.md`](./05-runner.md) 4.1 の `commitAuthor`）だけ | [`08-harness-adapter.md`](./08-harness-adapter.md) 3.3・3.4 |
| `Limits.Time` | その工程の上限時間の残り | [`05-runner.md`](./05-runner.md) 3.6 |
| `Limits` の CPU・メモリ | [`05-runner.md`](./05-runner.md) 4.1 の `container.cpus` / `container.memoryMiB` | 案（Q68） |
| テストレポートの形式名 | projects.test_report_format が `junit_xml` のとき JUnit XML として読む（[`02-data-model.md`](./02-data-model.md)、[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.3.1 の例と同じ値）。ほかの値は「レポートを解釈できない」として [`10-verification-gates.md`](./10-verification-gates.md) 3.3 の表に従う | 02 の projects |

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
