---
status: draft
written: 2026-11-10
範囲: 段ごとの Sandbox（Prepare・Work・Check・Publish）と Proposal Sandbox の隔離の条件、隔離方式の選択、段ごとのネットワーク、鍵とモデル API の認証情報の受け渡し、通話口、固定版の読み出し
---

# 09. Sandbox・実行権限 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: requirements 15.5〜15.7 の権限境界のうち、段ごとの隔離環境と、振り分けの提案の隔離環境

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。隔離は Evaloom の役目そのものである（vision.md 3章「安全な場所の用意」）。Git 領域の防御、GitHub 側の CI の扱い、setup の許可宛先（projects.setup_allowed_hosts）は software-engineer へ移し、この文書からは消した（本人決定 1）。段ごとに見える範囲の正本はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）6.2 の表であり、この文書はそれを Evaloom 側でどう実現するかを定める。

## 1. 目的

- 作業中の AI と、AI が書いたもの（作業領域の中身）を、段ごとの隔離環境の中だけで動かす（requirements 15.1 Untrusted Code Execution）。
- どの段からも、Control API、管理 DB、Evaloom の認証情報に届かないようにする（requirements 15.7、条件14・16）。
- 鍵は準備の段（読み取り用だけ）と出す段にだけ渡し、作業の段と確かめる段と振り分けの提案には渡さない（vision.md 6章、本人決定 4・7）。
- 鍵を受け取る段から AI を呼べないようにし、AI を動かす段には外への接続をモデルの API のほかに渡さない（本人決定 4・7）。
- 危険な操作を指示で禁じず、権限の外に置く（requirements 1.3 原則13）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | 段の種類、マウントする場所（契約 6.2 の `profile`、`workspace`、`out`、`inputs`、`records`、`scratch`）と書き込みの可否、専用設定環境（作業の段と振り分けの提案だけ）、環境変数の許可リスト、渡す鍵、ネットワークの許可、通話口のソケット、資源の上限、実行するコマンド |
| 出力 | プロセスの終了コード、標準出力・標準エラー、`out` と作業領域への変更 |

## 3. 処理詳細

### 3.1 段ごとの Sandbox

| | Prepare Sandbox | Work Sandbox | Check Sandbox | Publish Sandbox | Proposal Sandbox |
|---|---|---|---|---|---|
| 動かすもの | Profile の固定版の準備のプログラム（任意の段） | ミクロハーネス（[`08-harness-adapter.md`](./08-harness-adapter.md)） | Profile の固定版の確かめるプログラム | Profile の固定版の出すプログラム | ミクロハーネス（振り分けの提案、[`15-linear-intake.md`](./15-linear-intake.md)） |
| AI | 呼ばない | 使う | 呼ばない | 呼ばない | 使う |
| 書き込める場所 | `workspace`、`out`、`scratch` | `workspace`、`out`、`scratch`、作業用の設定コピー（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.5） | `out`、`scratch` | `out`、`scratch` | `out`、`scratch`、作業用の設定コピー |
| 読むだけの場所 | `profile` | `inputs`、`records`、専用設定ディレクトリ | `profile`、`workspace`、`inputs` | `profile`、`workspace`、`inputs` | 提案の入力（Issue と振り分けの表、[`15-linear-intake.md`](./15-linear-intake.md)） |
| 鍵 | Profile が `access: read` と宣言し、`read` で登録した鍵だけ | 無し（モデル API の認証情報だけ、3.5） | 無し | Profile が宣言した鍵だけ | 無し（モデル API の認証情報だけ） |
| ネットワーク | 3.3 | 3.3 | 3.3 | 3.3 | 3.3 |
| 通話口（3.6） | 中止の確認だけ | 無し | 中止の確認だけ | 中止の確認と、出す操作の許可 | 無し |

- 作業の段には `profile` を置かない。確かめる段のプログラムと基準が、作業中の AI から見えないようにするためである（契約 6.2、15章の1）。作業の段に置くのは、Profile の `stages.work` で宣言したファイルだけで、専用設定ディレクトリに読み取り専用で置く。
- 確かめる段と出す段では、作業領域を読み取り専用でマウントする。確かめた後に作業領域が変わることは無い。確かめたものと出すものが同じであることを保証するのはスペシャルハーネスである（本人決定 5、契約 12章）。
- 確かめる段は、作業中の AI が書いたもの（作業領域の中のテストなど）を動かすことがある。このため、確かめる段は鍵も外への接続も持たない、最も強く隔離した環境にする。
- 段ごとに Sandbox を作り直す。前の段の Sandbox を次の段に使い回さない（[`01-architecture.md`](./01-architecture.md) 3.2）。

### 3.2 隔離の条件（requirements 15.7）

すべての Sandbox に次の条件を設ける。

| 条件 | 確かめ方 |
|---|---|
| Control API へのアクセスを禁止する | Sandbox からホストのループバック（127.0.0.1 と ::1）とホストの IP への接続を遮断する。127.0.0.1 への限定だけでは境界にならないため（requirements 15.7） |
| 管理 DB へのアクセスを禁止する | Evaloom のデータディレクトリ（SQLite、ログ、成果物、鍵の保管、認証ファイルの保管）を Sandbox に見せない |
| Evaloom の認証情報へのアクセスを禁止する | 本人の HOME、キーチェーン、SSH エージェントのソケット、Runner トークン、Linear の API キー、本人のセッションを Sandbox に見せない |
| 宣言していない鍵を渡さない | 環境変数は許可リスト方式で渡す（5章）。鍵は 3.5 のとおり、段と宣言に合うものだけを環境変数で渡し、ファイルとしては置かない |
| ほかのタスクに触れない | マウントするのは、その Execution の作業領域と、その段の `out`・`inputs`・`records`・`scratch`、`profile`、専用設定環境だけ。ほかのタスクの作業領域と保持中のセッションのファイルを見せない |
| 不要なネットワークアクセスを禁止する | 3.3 |
| 管理権限につながるインターフェースを渡さない | Docker ソケット、launchd、Runner の制御用ソケットを渡さない。通話口（3.6）は段ごとに作り、その段に許した要求だけを中継する |
| CPU・メモリ・実行時間に上限を設ける | 実行時間は Profile の `stages.<段>.timeoutSec`（全体設定 `limits.stageTimeoutMaxSec` が上限、契約 10.2）。振り分けの提案の上限時間は [`15-linear-intake.md`](./15-linear-intake.md) の Q201（300 秒の案）。CPU とメモリは Runner の設定ファイルの `container.cpus` / `container.memoryMiB`、既定は Sandbox 1つあたり CPU 4・メモリ 8 GiB（[`05-runner.md`](./05-runner.md) 4.1、案 Q68） |

同じ macOS ユーザーで動かす場合、API トークンを別ファイルに置くだけでは隔離にならない（requirements 15.7）。どの方式でも、上の条件を [`14-security-tests.md`](./14-security-tests.md) のテストで実際に確かめる。

### 3.3 段ごとのネットワーク（requirements 15.7）

| Sandbox | 許可 | 遮断 |
|---|---|---|
| Prepare | Profile の `stages.prepare.network.allow` の宛先だけ。モデル API の宛先は、宣言してあっても通さない（Q146） | ホストのループバックとホストの IP、LAN、モデル API、そのほかすべて |
| Work | 作業の段のミクロハーネスの Adapter が宣言したモデル API の宛先だけ（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.6・3.7） | ホストのループバックとホストの IP、LAN、そのほかすべて |
| Check | 無し。名前解決も通さない | すべて |
| Publish | Profile の `stages.publish.network.allow` の宛先だけ。モデル API の宛先は、宣言してあっても通さない（Q146） | ホストのループバックとホストの IP、LAN、モデル API、そのほかすべて |
| Proposal | `routing.proposer` のミクロハーネスの Adapter が宣言したモデル API の宛先だけ | ホストのループバックとホストの IP、LAN、そのほかすべて |

許可先の持ち方は次のとおりとする。

| 許可先 | 置き場所 |
|---|---|
| モデル API の宛先 | Adapter のパッケージの中の定数。Adapter が使う CLI が接続するモデル API と認証の更新先だけを `host:port` で並べる。Adapter を足すときは、そのパッケージの中で一覧も足す（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.2 の条件31の範囲）。案（Q71） |
| 準備の段と出す段の宛先 | Profile の `stages.prepare.network.allow` と `stages.publish.network.allow`（契約 5.2）。Runner は claim で固定した Profile の値を使う |

宛先の書き方は、小文字の DNS ホスト名とポートの完全一致とする（契約 5.1）。ワイルドカード、IP リテラル、`localhost` などのローカル名は受け付けない。作業の段と確かめる段と振り分けの提案には、Profile から宛先を足せない（契約 5.2）。

モデル API を準備の段と出す段から外すのは、鍵を持つ段で AI を呼ばせないためである（本人決定 4）。ただし、Evaloom が知らないモデルの宛先までは見分けられない。鍵を持つ段で AI を呼ばないことは、スペシャルハーネスの義務でもある（契約 12章の5）。

### 3.4 隔離方式の選択（Q1）

requirements 21章で、隔離の具体的な方式とネットワークの方針は詳細設計事項とされている。候補は次のとおり。

| 方式 | 長所 | 短所 |
|---|---|---|
| コンテナ（Linux VM 上のコンテナ） | ファイル・ネットワーク・プロセスの分離をまとめて設定できる。ループバックが既定で分かれる | macOS 専用のツールチェーン（Xcode など）が要る段を動かせない。ホストへの抜け道（`host.docker.internal` など）を明示的に塞ぐ必要がある。Docker ソケットを渡さない運用が要る |
| 別の macOS ユーザー + macOS のサンドボックスプロファイル | ホストと同じツールチェーンで動く | ネットワークの宛先単位の制御が弱い。ファイル権限とプロファイルの設定漏れが起きやすい |
| VM（macOS または Linux） | 分離が最も強い | 起動が重い。ディスクとメモリを多く使う |

判断基準は次のとおりとする。

1. 3.2 の条件をすべて満たし、[`14-security-tests.md`](./14-security-tests.md) のテストに合格すること（必須）
2. スペシャルハーネスが宣言した段のプログラムが動くこと
3. 3.3 のネットワークの許可を宛先単位で表せること
4. 段ごとの起動時間と資源の使用量

コンテナで実行するだけで安全になるわけではない。ファイル共有、ネットワーク、権限の設定まで含めて検証する（requirements 15.7）。

決定（Q1、2026-10-29、一部）: macOS 専用のツールチェーンが要らない段はコンテナで隔離する。コンテナ方式では、宛先を絞るプロキシ（[`05-runner.md`](./05-runner.md) 4.1 の `container.proxyImage`）を通して 3.3 の許可を実現する。Xcode などが要る段の方式は、次の第一案を実測してから決める（2026-11-05 に実測して採用した。下の「決定（Q1、2026-11-05、残り）」）。

- 第一案: 実行ごとに Tart（Apple の Virtualization.framework を使う VM ツール）で macOS VM をクローンし、終わったら削除する。ネットワークは Softnet で CIDR 単位に許可・遮断する。マウントは要る場所だけを `--dir` で渡す。
- 別ユーザー + macOS のサンドボックスプロファイルを採らない理由: 宛先単位の制御にはホストのループバック上のプロキシが要り、3.2 の「ホストのループバックへの接続を遮断する」と両立しない。
- 第一案の制約: macOS ゲストの同時起動は Apple のライセンスで 2 台まで。Softnet の指定は CIDR だけでドメイン名では書けない。Softnet は SUID か passwordless sudo を要する。

実測の結果（2026-11-05、Mac mini（macOS 27.2、14 コア、メモリ 64 GB）。Tart 2.41.1、Softnet 0.25.0、イメージ `ghcr.io/cirruslabs/macos-tahoe-xcode:latest`（ゲストは macOS 26.6.2、Xcode 27.2、iOS 27.2 シミュレータ）。VM は既定値の 4 CPU、メモリ 8 GB）は次のとおり。

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

実測から分かった制約は次のとおり。

- Softnet の既定と NAT の既定は、ホストで待ち受けるサービスに届く。3.2 の「ホストのループバックへの接続を遮断する」を満たすには、どの Sandbox でも `--net-softnet-block=0.0.0.0/0` を必ず付け、要る宛先だけを `--net-softnet-allow` で足す。
- `0.0.0.0/0` を遮断するとゲートウェイの DNS も止まる。名前解決が要る段（準備の段と出す段）では、許可した公開 DNS（例 1.1.1.1）にゲストの DNS を向けてから段を始める。確かめる段では DNS も通さない。
- Softnet は CIDR でしか書けないため、宛先のホスト名を段の開始時に名前解決して得た IP を許可する。解決した IP が変わる宛先では届かないことがある。
- 1台の起動に約 28 秒かかる。段ごとに VM を作り直すと、4つの段で起動だけで約 2 分増える。

決定（Q1、2026-11-05、残り）: Xcode などが要る段は第一案（実行ごとに使い捨てる Tart の macOS VM + Softnet）で隔離する。上の実測で、`xcodebuild test` とシミュレータが VM 内で動き、宛先の許可・遮断も働いたためである。次を条件とする。

- どの Sandbox でも `--net-softnet-block=0.0.0.0/0` を付け、3.3 の表で許可する宛先だけを `--net-softnet-allow` で足す。Softnet の既定のままにしない（ホストに届くため）。
- macOS ゲストの同時起動は 2 台まで（Apple のライセンス）。全体の同時実行の上限2本の中で収まるように、1本の Execution が同時に起動する VM は1台にする。
- どの段を macOS VM で動かすかはスペシャルハーネスが宣言する（Q147）。宣言の形が決まって実装を起票するまで、macOS VM を要する Profile は登録を拒否する。

### 3.5 鍵とモデル API の認証情報（requirements 15.6）

#### スペシャルハーネスの鍵

- 鍵は Runner が保管する（[`01-architecture.md`](./01-architecture.md) 3.5）。Control Plane と管理 DB には値を置かない。
- Runner は、準備の段と出す段の Sandbox にだけ、Profile が宣言した鍵を環境変数 `EVALOOM_SECRET_<SLOT>` で渡す（契約 11章）。準備の段には、`access: read` と宣言し、かつ `read` で登録した鍵だけを渡す。`write` で登録した鍵は、Profile が `read` と宣言しても準備の段に渡さない。
- 作業の段、確かめる段、振り分けの提案には、どの鍵も渡さない。Profile の登録で、`stages.work` と `stages.check` に `secrets` を書けない（契約 5.2）。
- 鍵を受け取る段（準備と出す）では AI を呼ばない（本人決定 4）。3.3 のとおり、この2つの段からモデル API には届かない。
- 鍵をファイルとして Sandbox に置かない。Runner は、渡した値を段のログと成果物から除外してから保存・配信する（[`05-runner.md`](./05-runner.md)）。除外は補助であり、スペシャルハーネスが鍵を書かない義務は変わらない（契約 11章）。
- 宣言した鍵が保管に無いときは、Sandbox を作らずに `config_error` とする。

#### Evaloom の認証情報

Runner トークン、Linear の API キー、本人のパスワードのハッシュとセッションは、どの Sandbox にも渡さない（[`01-architecture.md`](./01-architecture.md) 3.1・3.6）。

#### モデル API の認証情報（Q7・Q8）

作業の段と振り分けの提案では、ミクロハーネスがモデル API へ接続するための認証情報だけを例外として渡す。

1. 案: Sandbox の外に認証を付けるプロキシを置き、Sandbox からはプロキシ経由でだけモデル API に届くようにする。ミクロハーネスから認証情報の値は見えない。
2. 案: Evaloom 専用で、用途と利用上限を絞った認証情報を渡す。
3. 決定（Q7・Q8、2026-10-29）: Q7 でサブスクリプションを採ったため 2 とする。サブスクリプションのログイン情報は CLI 自身が持つので、1 のプロキシでは値を隠せない。ミクロハーネスからは認証情報の値が見えるため、本人の普段の認証情報とは分け、Evaloom 専用のものだけを専用設定環境（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.5）に置く。
4. 規約の確認（Q7、2026-11-05）: 両社とも自動実行には API キーを推奨するが、サブスクリプションでの自動実行も次の範囲で使えると判断した。
   - Claude Code: サブスクリプションのログインは、プランの購入者が改変していない Claude Code を使うためのもので、第三者が利用者の認証を預かる使い方を禁じている。Evaloom は本人が自分のログインで使うので当たらない。プランの利用上限は通常の個人利用を前提にしている（Claude Code Docs「Legal and compliance」）。
   - Codex: ChatGPT のログインでの自動実行は、信頼できる非公開の自動化に限った例外として手順が公開されている。公開リポジトリでは使わない、`auth.json` を同時に実行するジョブで共有しない、更新されたトークンを書き戻して上書きしない、が条件である（OpenAI「Codex CI/CD auth」）。
   - 運用の条件: Evaloom を使うのは本人だけとする。同じ認証情報を、作業の段と振り分けの提案をあわせて2本で同時に使わない（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.5 の排他ロック）。利用上限に当たるようになったら、API キーへの切り替えを見直す。
   - 書き戻しと同時実行の守り方（2026-11-05 決定）: 段の終わりに、作業用の設定コピーの認証ファイル（Codex は `auth.json`、Claude Code は `.credentials.json`）1つだけを、JSON の形と大きさを確かめてから Evaloom 専用の認証の保管場所へ書き戻す。認証ファイルは読み込んだ構成の記録（context_revisions）に含めない。詳細は [`08-harness-adapter.md`](./08-harness-adapter.md) 3.5。

モデル API の認証情報は、準備・確かめる・出すの段には渡さない。

### 3.6 通話口

- Runner は、準備・確かめる・出すの段の Sandbox の中に、段ごとの通話口（Unix ドメインソケット）を置き、パスを `EVALOOM_CHANNEL` で渡す（契約 7.4、方式は Q114）。作業の段と振り分けの提案には置かない。
- Runner が中継してよい要求は、準備の段と確かめる段では `cancel_check` だけ、出す段では `cancel_check` と `authorize` だけである（契約 6.2）。ほかの要求は Runner が拒否し、Control Plane に送らない。
- `authorize` は、Runner が Runner 用 API `POST /api/executions/{id}/authorize` に中継する（[`01-architecture.md`](./01-architecture.md) 5.2）。Runner トークンは Sandbox に渡さない。中継に失敗したら、Runner は段に `allowed: false`（`unavailable`）を返す。
- 通話口は段が終わったら閉じる。

### 3.7 固定版の読み出し

- Runner は、Profile の固定版を、スペシャルハーネスのリポジトリの作業ツリーではなく、claim で固定した commit から読み出す（契約 4.2）。準備・確かめる・出すの段には、その commit の中身を `profile` に読み取り専用で置く。作業の段には `stages.work` で宣言したファイルだけを置く（[`07-context.md`](./07-context.md) 3.2）。
- Runner がホストでスペシャルハーネスのリポジトリを Git で読むときは、フック（`core.hooksPath` を空のディレクトリに向ける）と、外部プログラムを起動しうる設定（`core.fsmonitor`、`core.sshCommand`、`credential.helper`、filter / diff / merge のドライバ、`core.pager`、`gpg.program` など）を無効にする。対象の設定の一覧は実装時に確定する。
- 読み出した中身に、commit の外を指すシンボリックリンクやサブモジュールがあれば、段を始めずに `config_error` とする（契約 4.1）。

### 3.8 権限で拒否する操作（requirements 15.3）

requirements 15.3 の承認の対象になる操作は、指示で禁じるのではなく、次の権限で拒否する。

| 操作 | 拒否の仕組み |
|---|---|
| 外の状態を変える操作（マージ、投稿、送信など） | 作業の段・確かめる段・振り分けの提案には、鍵も外への経路も無い。準備の段は読み取り用の鍵と宣言した宛先だけを持つ。出す段は、操作の直前の問い合わせで `allowed: true` を受けたときだけ操作する（契約 7.4、[`17-approval.md`](./17-approval.md)） |
| 本番環境へのデプロイ、外部へのメール送信 | Profile が宣言しない限り、どの段にもその鍵と宛先が無い。宣言した場合も、承認の要る操作は 17 の承認を通す |
| 重要なファイルの削除 | 書き込めるのは 3.1 の表の場所だけ。本人のリポジトリとファイルは Sandbox に見せない |
| 認証・権限の変更、ハーネス契約・評価基準・中央のセキュリティポリシーの変更 | Sandbox から Control API に届かない。通話口では 3.6 の要求しか送れない。Runner トークンでは権限設定を変える API を呼べない（[`01-architecture.md`](./01-architecture.md) 3.6） |

Control Plane と Runner は、スペシャルハーネスに代わって外の状態を変える処理を持たない（[`01-architecture.md`](./01-architecture.md) 3.1）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| Sandbox の種類 | Prepare、Work、Check、Publish、Proposal | 01 の 3.1、契約 6.2 |
| 段ごとに見える範囲 | 契約 6.2 の表 | 契約 6.2 |
| 鍵を受け取る段 | 準備（読み取り用だけ）と出す。どちらも AI を呼ばない | 本人決定 4・7、vision.md 6章 |
| 作業の段の外への接続 | モデルの API だけ | 本人決定 7 |
| 確かめる段の外への接続 | 無し（名前解決も通さない） | 契約 6.2 |
| 準備の段と出す段の外への接続 | Profile が宣言した宛先だけ。モデル API は通さない | 本人決定 4・7 |
| Evaloom の認証情報 | どの Sandbox にも渡さない | requirements 15.6 |
| 隔離の条件 | 3.2 の8項目 | requirements 15.7 |
| 隔離の検証 | 実際にアクセスを試みる自動テストで確かめる | requirements 15.7、14.2 |

## 5. インターフェース

Runner 内部のインターフェースは次の案とする。

```go
type SandboxKind string // "prepare" | "work" | "check" | "publish" | "proposal"

type Mount struct {
    HostPath  string
    GuestPath string // 契約 6.1 の paths（/evaloom/profile など）
    Writable  bool
}

type SandboxSpec struct {
    Kind           SandboxKind
    Mounts         []Mount            // 3.1 の表の場所だけ
    ConfigDir      string             // 専用設定環境（work と proposal だけ）
    ConfigWritable bool               // ConfigDir が作業用の設定コピーのときだけ true
    Env            map[string]string  // 許可リスト方式
    Secrets        map[string]string  // slot → 値。prepare と publish だけ。EVALOOM_SECRET_<SLOT> で渡す
    Network        NetworkPolicy      // 3.3。Allow は host:port の配列。check は空で名前解決も止める
    ChannelPath    string             // 通話口（prepare・check・publish だけ）
    Limits         ResourceLimits     // CPU、メモリ、実行時間
}

type Sandbox interface {
    Run(ctx context.Context, spec SandboxSpec, cmd []string) (Process, error)
}
```

段の組み立て（`internal/stage`）と Adapter（[`08-harness-adapter.md`](./08-harness-adapter.md)）は、このインターフェースだけを使う。隔離方式（Q1）を変えても呼び出し側は変えない。`Kind` が `work`・`check`・`proposal` の `SandboxSpec` に `Secrets` が入っていたら、`Run` は Sandbox を作らずにエラーを返す。

Runner が `SandboxSpec` に入れる値は次のとおりとする。

| 項目 | 値 | 理由 |
|---|---|---|
| 準備・確かめる・出すの段のコマンド | Profile の `stages.<段>.command` の配列をそのまま使う。シェルを通さない。作業ディレクトリは `profile` | 契約 6.4 |
| `Env`（準備・確かめる・出す） | `EVALOOM_REQUEST`、`EVALOOM_RESULT`、`EVALOOM_CHANNEL`、`HOME`（`scratch` の中の空のディレクトリ）、`PATH`、`LANG`、`TZ=UTC` だけ。鍵は `Secrets` で渡す | 契約 6.4 |
| `Env`（作業の段と振り分けの提案） | 専用設定環境の変数（`CODEX_HOME` / `CLAUDE_CONFIG_DIR`）、`HOME`、`PATH`、`LANG`、`TZ=UTC`、作業の段では `EVALOOM_REQUEST` | [`08-harness-adapter.md`](./08-harness-adapter.md) 3.5 |
| `Limits.Time` | Profile の `stages.<段>.timeoutSec`（振り分けの提案は 15 の Q201 の上限時間） | 契約 10.2 |
| `Limits` の CPU・メモリ | [`05-runner.md`](./05-runner.md) 4.1 の `container.cpus` / `container.memoryMiB` | 案（Q68） |

## 6. エラー処理

| 状況 | 処理 |
|---|---|
| Sandbox を作れない | `failed`（`sandbox_error`）。ホストでの直接実行に切り替えない（Fail Closed） |
| 資源の上限に達して強制終了された | 実行時間なら `timeout`、それ以外は `failed`（`sandbox_error`） |
| 隔離の設定を確かめられない（プロキシが起動していない、など） | 段を始めない。`failed`（`sandbox_error`） |
| 宣言した鍵が保管に無い | 段を始めない。`failed`（`config_error`） |
| 準備の段と出す段の宛先にモデル API の宛先がある | 段を始めない。`failed`（`config_error`）。登録の時点でも拒否する（Q146） |
| 通話口に許していない要求が来た | Runner が拒否し、Control Plane に送らない。Runner のログに残す |

## 7. スコープ外

- 複数 Runner の間のネットワーク構成（Phase 6）
- 遠隔アクセスの認証・ネットワーク構成（requirements 15.5、[`18-remote-access.md`](./18-remote-access.md)）
- 作業領域の中の Git 領域の防御、GitHub 側の CI の扱い、依存関係を取得する宛先の選び方。スペシャルハーネス（software-engineer）が決める（vision.md 4章）

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) 6.2 段ごとに用意するもの、6.4 プログラムの段、7.4 通話口、11章 鍵
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) 構成要素と権限の表、鍵の保管
- [`05-runner.md`](./05-runner.md) 段の実行手順、コンテナの設定
- [`07-context.md`](./07-context.md) 固定版からの `stages.work` の読み出し
- [`08-harness-adapter.md`](./08-harness-adapter.md) 専用設定環境、モデル API の宛先
- [`14-security-tests.md`](./14-security-tests.md) 段ごとの隔離のテスト
- [`15-linear-intake.md`](./15-linear-intake.md) 振り分けの提案の入力と上限時間
- [`17-approval.md`](./17-approval.md) 出す操作の直前の問い合わせ

## 9. 実装単位

- [ ] （人間） 隔離方式（Q1）、モデル API 認証の方式（Q8）を決める
- [ ] [Backend] Sandbox インターフェースと、採用した方式による実装（段ごとのマウント、環境変数の許可リスト、資源の上限）
  - 受け入れ基準: どの Sandbox の中からも本人の HOME、Evaloom のデータディレクトリ、Runner トークンのファイルが見えない; 許可リストに無い環境変数が Sandbox に渡らない; 作業の段から `profile` が見えない; 確かめる段と出す段で作業領域に書き込めない; 実行時間の上限で止まる
- [ ] [Backend] 段ごとのネットワークとループバックの遮断（3.3）
  - 受け入れ基準: どの Sandbox からも 127.0.0.1 の Control API に接続できない; 作業の段と振り分けの提案からモデル API のほかに接続できない; 確かめる段から名前解決も含めて外に接続できない; 準備の段と出す段から宣言していない宛先とモデル API に接続できない
- [ ] [Backend] 鍵の受け渡し（3.5）
  - 受け入れ基準: 作業の段・確かめる段・振り分けの提案の環境変数とファイルに鍵の値が無い; 準備の段に `write` で登録した鍵が渡らない; 出す段に宣言していない鍵が渡らない; `Kind` が work・check・proposal の `SandboxSpec` に鍵を入れると Sandbox が作られない
- [ ] [Backend] 通話口の作成と中継（3.6）
  - 受け入れ基準: 準備の段と確かめる段の通話口で authorize が拒否される; 出す段の authorize が Runner 用 API に中継される; 作業の段と振り分けの提案に通話口が無い; 段が終わると通話口が閉じる
- [ ] [Backend] 固定版の読み出し（3.7）
  - 受け入れ基準: スペシャルハーネスのリポジトリにフックや core.fsmonitor を仕込んでも、Runner の読み出しでそれらが実行されない; 作業ツリーを書き換えても固定した commit の中身が読まれる
- [ ] [Backend] モデル API の認証付与プロキシ（Q8 の第一案を採る場合）
  - 受け入れ基準: Sandbox の中の環境変数とファイルにモデル API の認証情報の値が無い状態で、ミクロハーネスがモデル API を使える

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q146 | 準備の段と出す段からモデル API を外す方法 | 登録済みのすべての Adapter が宣言したモデル API の宛先を1つの一覧にまとめ、Profile の登録で `stages.prepare.network.allow` と `stages.publish.network.allow` にその宛先があれば 422（`precondition_failed`）で拒否する。段の開始のときにも同じ一覧で確かめ、あれば `config_error` にする。一覧に無いモデルの宛先は見分けられないため、契約 12章の5の義務に頼ることを契約 5.2 に明記した |
| Q147 | 段を動かす環境（コンテナのイメージ、macOS VM）を誰がどう決めるか（Project は6項目だけになり、Xcode が要るかどうかを持たなくなった） | 契約 第1版の任意項目として、Profile の `stages.<段>.runtime`（契約 5.2 に足した）に `{ "kind": "container", "image": "<ダイジェスト付きのイメージ>" }` または `{ "kind": "macos_vm", "image": "<Tart のイメージ>" }` を足す。省略したら Runner の設定ファイルの既定のコンテナのイメージを使う。作業の段は Runner の既定のイメージだけとする（ミクロハーネスの認証と設定の置き方を Adapter が決めるため）。macOS VM の実装までは `macos_vm` の Profile を登録で拒否する |
