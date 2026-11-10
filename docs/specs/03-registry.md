---
status: draft
written: 2026-11-10
範囲: Project の登録（6項目）、振り分けの表（routes）の登録、スペシャルハーネスの登録、Profile の版の登録と検証、承認を省く設定の検証
---

# 03. Registry 仕様

**親**: [`00-overview.md`](./00-overview.md) ｜ **担当**: Backend ｜ **範囲**: Project、振り分けの表、スペシャルハーネス、Profile の版の登録と固定

この文書は [`../vision.md`](../vision.md) と本人決定（2026-11-10、[`../vision-alignment.md`](../vision-alignment.md) 5章・6章）に合わせて、目標の形で書き直した。テーブルと列の名前は [`02-data-model.md`](./02-data-model.md)、API は [`01-architecture.md`](./01-architecture.md)、Profile 定義の形はハーネス契約 [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md)（以下「契約」）に従う。

Project のローカルパスとリポジトリの検証、コピー許可リスト、setup の許可宛先（旧 3.2・3.3・3.3.1、Q73・Q87）、テストコマンドとベースブランチの登録は、開発のスペシャルハーネス（software-engineer）の中身として software-engineer へ移し、この文書からは消した（本人決定 1・6）。役割ごとの割り当て（旧 3.6、project_harnesses）と Phase 1 の権限設定（旧 3.7、Q25）も消した。Project は頼むスペシャルハーネスと版を直接持ち、Tool Permissions は Profile 定義が宣言する（02 の Q118）。

## 1. 目的

- Project を vision.md 4章の6項目（名前、振り分けのラベル、頼むスペシャルハーネス、渡す値、承認を省く設定、Runner の指定）だけで登録できるようにする（本人決定 6）。Evaloom は仕事の種類に固有の情報を持たない。
- ラベルから振り分け先（Project またはスペシャルハーネス）を引く振り分けの表を、本人が管理画面で登録できるようにする（本人決定 2）。
- 1つのスペシャルハーネスを複数の Project で共有し、Project ごとに Profile の版を固定できるようにする（requirements 4.3）。
- Profile の版を、登録の時点で契約に合っているか確かめてから記録し、記録した commit の中身だけを実行に使うようにする（契約 4.2・5章）。

## 2. 入出力

| 区分 | 内容 |
|---|---|
| 入力 | 管理画面からの登録・更新（本人用 API）、スペシャルハーネスのリポジトリのローカルの複製にある Git のタグと `evaloom/profile.yaml` |
| 出力 | runners を参照する projects、routes、harnesses、harness_profiles の行（[`02-data-model.md`](./02-data-model.md) 3.2）、audit_logs |

## 3. 処理詳細

### 3.1 Project の登録・更新

入力は `id`、`name`、`harnessId`、`profileId`、`params`、`approvalWaivers`、`runnerId` の7つだけとする（[`01-architecture.md`](./01-architecture.md) 5.1）。振り分けのラベルは振り分けの表（3.2）で登録する。これ以外の項目を含む入力は 400 `validation_failed` にする。

| 項目 | 必須 | 検証 |
|---|---|---|
| `id` | 登録時に必須。更新では変えられない | 英小文字・数字・ハイフンの slug（形は Q125）。既存の Project と重なれば 409 |
| `name` | 必須 | 空でない。200 文字以内 |
| `harnessId` | 必須 | harnesses に登録済み |
| `profileId` | 必須 | harness_profiles に登録済みで、harness_id が `harnessId` と同じ。違えば 422 |
| `params` | 任意（既定 `{}`） | JSON オブジェクト。大きさの上限は Q129。Profile が `params.schema` を持てば、そのスキーマで形だけ確かめる。持たなければ `{}` だけを受け付ける（契約 5.2、Q115）。合わなければ 422 |
| `approvalWaivers` | 任意（既定 `[]`） | 3.5 のとおり |
| `runnerId` | 必須 | runners に登録済み（Runner の登録は `evaloomd runner register`、01 の 3.6） |

処理は次のとおりとする。

1. Control Plane が上の表の検証を行う。1つでも失敗したら保存しない（Fail Closed）。
2. 検証に通ったら projects に保存し、audit_logs（`project.create` / `project.update`）に変更前後の値を残す。`params` は値そのものを残す。
3. 更新（PATCH）でも、変えない項目を含めて検証をやり直す。`harnessId` を変えるときは `profileId` も同じ要求で新しいスペシャルハーネスの版に変えなければならない。`profileId` を変えたときは、新しい版の `params.schema` と `approvals` で `params` と `approvalWaivers` を確かめ直す。新しい版で `waivable: true` でない操作が `approvalWaivers` に残っていれば 422 にする（02 の projects）。

`params` は Evaloom が解釈しない値である（vision.md 4章）。鍵やパスワードを `params` に入れない。鍵は Runner の鍵の保管に入れる（[`01-architecture.md`](./01-architecture.md) 3.5）。Evaloom は `params` の中身から鍵を見分けられないため、管理画面にこの注意を出す（[`13-management-ui.md`](./13-management-ui.md)）。

Project の変更は、claim 済みの Execution には影響しない。Profile の版、渡す値、Runner は claim の時点で Execution に固定する（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.3）。claim 前の Execution は、claim の時点の Project の値で動く。

`GET /api/projects` と `GET /api/projects/{id}` の応答には、routes から引いた振り分けのラベル（無ければ `null`）を含める。

### 3.2 振り分けの表（routes）の登録・更新・削除

ラベルから振り分け先を1つ引く表である（本人決定 2、[`02-data-model.md`](./02-data-model.md) routes）。

入力（`POST /api/routes`）は `label` と、`projectId` か `harnessId` のどちらか一方とする。検証は次のとおりとする。

| 確認 | 失敗したとき |
|---|---|
| `label` が全体設定 `linear.routingLabelPrefix`（既定 `route:`）で始まり、接頭辞の後に1文字以上ある。前後の空白を含まない。100 文字以内 | 400 |
| `label` が全体設定 `linear.triggerLabel`（`ai-ready`）と同じでない | 400 |
| `projectId` と `harnessId` のちょうど一方がある | 400 |
| 振り分け先が登録済み | 422 |
| 同じ `label` が無い。同じ Project、同じスペシャルハーネスへのラベルが無い（1つの振り分け先にラベルは1つ） | 409 |

- ラベルの照合は完全一致とする。大文字と小文字は区別する。
- `PATCH /api/routes/{label}` は振り分け先だけを変えられる。ラベルの名前を変えるときは、消してから登録し直す。
- `DELETE /api/routes/{label}` はラベルを表から消す。
- 登録・変更・削除は audit_logs（`route.create` / `route.update` / `route.delete`）に変更前後の値を残す。
- 振り分け先は、タスクを `queued` にした時点で tasks に書き、その後は変えない（02 の tasks）。そのため、表の変更は `queued` 以降のタスクに影響しない。振り分けの提案の判断待ち（02 の 5.6 `route`）には影響する。本人が承認するラベルは、判断の時点の表にあるものでなければならない（02 の Q112）。
- スペシャルハーネスへ直接振り分けるラベルは、そのスペシャルハーネスの `default_profile_id` が無くても登録できる。そのラベルで来たタスクは、既定の版が設定されるまで取り込みを保留する（02 の 5.7 `config_missing_profile`、[`15-linear-intake.md`](./15-linear-intake.md)）。管理画面はこのとき警告を出す。
- Linear 側にそのラベルが実際にあるかの扱いは Q128 で決める。
- 全体設定 `linear.routingLabelPrefix` を変えたときに、既存のラベルが新しい接頭辞で始まらない場合の扱いは Q127 で決める。

Project を作っても、振り分けのラベルは自動では作らない。ラベルの無い Project には、管理画面からの手動のタスク（[`04-task-lifecycle.md`](./04-task-lifecycle.md) 3.1）と、振り分けの提案の承認でだけ仕事が届く。

### 3.3 スペシャルハーネスの登録・更新

入力は `id`、`name`、`repoUrl`、`repoPath` とする。更新（PATCH）では `name`、`repoUrl`、`repoPath`、`defaultProfileId` を変えられる（[`01-architecture.md`](./01-architecture.md) 5.1）。

| 項目 | 検証 |
|---|---|
| `id` | Project と同じ slug の形（Q125）。既存と重なれば 409。登録後は変えられない |
| `name` | 空でない。200 文字以内 |
| `repoUrl` | 表示用の URL。`https://` で始まる。Evaloom はここから取得しない（Q126） |
| `repoPath` | スペシャルハーネスのリポジトリのローカルの複製。絶対パスで、ディレクトリがあり、`git -C <path> rev-parse --show-toplevel` が同じ実パスを返す。シンボリックリンクを解決した実パスで保存する。失敗は 422 |
| `defaultProfileId` | NULL か、このスペシャルハーネスの harness_profiles の行。違えば 422 |

- スペシャルハーネスのリポジトリは独立した Git リポジトリである（契約 4.1）。Evaloom が `repoPath` で行うのは、タグの解決と、記録した commit の中身の読み出しだけである。作業ツリーとブランチは使わず、書き込みもしない。
- 専門分野、対応タスク、Adapter、対応モデル、利用可能ツール、Tool Permissions は登録しない。どれも Profile 定義が宣言する（02 の harnesses、Q118）。
- `defaultProfileId` は、スペシャルハーネスへ直接振り分けたタスクで使う版である。これを設定したとき、`config_missing_profile` で保留しているタスクの再判定を要求する（[`15-linear-intake.md`](./15-linear-intake.md)）。
- `repoPath` と `defaultProfileId` の変更は audit_logs（`harness.update`）に変更前後の値を残す。

### 3.4 Profile の版の登録と検証（requirements 5.4、契約 4.2・5章）

入力（`POST /api/harness-profiles`）は `harnessId` と `version`（SemVer）とする。

1. Control Plane が、スペシャルハーネスの `repoPath` で、タグ `profile/v<version>` を commit SHA に解決する。タグが無ければ 422。Evaloom はリモートから取得しない（Q126）。
2. その commit の `evaloom/profile.yaml` を読み、JSON にする。ファイルが無い、YAML として読めないときは 422。
3. 下の表の検証をすべて行う。1つでも失敗したら登録しない（Fail Closed）。応答の `details` に、失敗した項目のパスと理由を全部入れる。
4. 通ったら、definition（JSON にした全文）、definition_hash、commit_sha、tag_name、contract_version を harness_profiles に保存する。operation_status は `provisional` とする（requirements 11.1）。audit_logs に `profile.register` を残す。

同じ (スペシャルハーネス, 版) を同じ commit で登録し直した場合は、何もせず既存の行を返す。別の commit で登録しようとしたら 409 にし、audit_logs に残す（タグの付け替えを見つけた記録）。

検証の項目は契約の正本に従う。この表は Evaloom が登録の時点で行う確認の一覧である。

| 確認 | 規則 | 契約 |
|---|---|---|
| 契約の版 | `contractVersion` が、Evaloom が対応する版（`"1"`、02 の 4.3）のどれか | 3章 |
| 知らない項目 | 宣言した契約の版に無い項目があれば拒否する | 3章 |
| 版の一致 | `profileVersion` が SemVer で、タグの版と一致する | 4.2 |
| 必須の項目 | `name`、`description`（2000 文字以内）、`acceptanceCriteria`（`required` / `optional`）、`stages.work`、`stages.check`、`stages.publish` がある | 5.2 |
| 参照するパス | `stages.<段>.command` の先頭、`stages.work.instructions` / `skills` / `settings`、`params.schema` は、同じ commit の中の相対パスで、その commit に実在する。`..` を含むパス、絶対パス、シンボリックリンク（Git のモード 120000）、サブモジュール（160000）は拒否する | 4.1、5.2 |
| 確かめる段と作業の段の分離 | prepare・check・publish の `command` の先頭のパスが、`stages.work` で宣言したファイル・ディレクトリの中に無い | 4.1 |
| `command` | prepare・check・publish で必須。文字列の配列で、空でない | 5.2、6.4 |
| `timeoutSec` | どの段も 1 以上で、全体設定 `limits.stageTimeoutMaxSec` のその段の値以下 | 5.2、10.2 |
| `network.allow` | prepare と publish にだけ書ける。各要素は小文字の DNS ホスト名と 1〜65535 のポートを `:` でつないだもの。ワイルドカード、IP リテラル、ローカルの名前（ドットを含まない1語の名前と、最後のラベルが `localhost`・`local`・`internal` の名前）は拒否する。同じ宛先の重複も拒否する | 5.2 |
| `secrets` | prepare には `access: read` だけ、publish には `read` と `write` を書ける。work と check には書けない。`slot` は `^[a-z][a-z0-9_]{0,31}$`（Q138）。同じ段で同じ `slot` を2回書けない | 5.2、11章 |
| `network.allow` とモデルの API | prepare と publish の `network.allow` に、登録済みのすべての Adapter が宣言したモデルの API の宛先が1つでもあれば拒否する（[`09-sandbox.md`](./09-sandbox.md) 3.3、Q146） | 5.2、12章の5 |
| `runtime` | prepare・check・publish にだけ書ける。`kind` が `container`（ダイジェスト付きのイメージ）か `macos_vm`。macOS VM の実装までは `macos_vm` を拒否する（[`09-sandbox.md`](./09-sandbox.md) 3.4、Q147） | 5.2 |
| `stages.work.microHarness` | Evaloom に登録された Adapter の名前（`claude_code` / `codex`、02 の 4.2） | 5.2 |
| `stages.work.toolPermissions` | `stages.work.microHarness` の Adapter の変換（[`08-harness-adapter.md`](./08-harness-adapter.md) 3.9）で CLI の設定に変換できる（Q145） | 5.2 |
| `stages.work.resume` | `required` / `optional` のどちらか | 5.2、8章 |
| `stages.work.model` | 空でない文字列。Model Registry は持たない（Q19） | 5.1 |
| `params.schema` | JSON Schema（draft 2020-12）として読める | 5.2 |
| `approvals` | `operation` が英小文字・数字・`_` で 64 文字以内、Profile の中で一意。`waivable` は真偽値（既定 `false`） | 5.2 |
| `records` | `keep` の各要素が `<段>/<相対パス>` で、段が prepare・check・publish のどれか。`pass.recent` が 0〜50 の整数 | 5.2、9章 |

鍵の値は登録の時点では確かめない。宣言した鍵が Runner の保管に無いことは、段を始める前に Runner が見つけて `config_error` にする（[`01-architecture.md`](./01-architecture.md) 3.5、[`05-runner.md`](./05-runner.md) 3.6）。

登録済みのタグが別の commit を指すようになったことは、登録し直しのときと、Runner が Profile の中身を読み出すとき（[`05-runner.md`](./05-runner.md) 3.4）に見つける。見つけたら、その版での実行を止める（`config_error`）。Execution では harness_profiles に保存した commit SHA を使い、タグの指す先は使わない（契約 4.2）。

新しい版を登録しても、Project の `profileId` とスペシャルハーネスの `defaultProfileId` は自動では変わらない（requirements 3.4、4.3）。本人が版を選び直す。

### 3.5 承認を省く設定の検証

Project の `approvalWaivers` は、承認を省く操作の名前の配列である（本人決定 6、契約 7.3）。

| 確認 | 失敗したとき |
|---|---|
| 文字列の配列で、同じ名前を2回含まない | 400 `validation_failed`（field は `approvalWaivers`） |
| 各名前が、`profileId` の Profile の `approvals[].operation` にある | 422 `precondition_failed`（reason は `unknown_operation`） |
| その操作が `waivable: true` と宣言されている | 422 `precondition_failed`（reason は `not_waivable`） |

- 承認を省く設定を変えられるのは本人だけである。Runner とスペシャルハーネスには変える経路を作らない（requirements 15.3）。変更前後の値を audit_logs に残す。
- 省いた操作でも、出す段の直前の問い合わせ（中止の確認）は省かない。問い合わせの時点で Project の `approvalWaivers` を読み直し、省く設定が外れていれば許可しない（02 の approvals、[`17-approval.md`](./17-approval.md)）。
- スペシャルハーネスへ直接振り分けた仕事には、承認を省く設定は無い（02 の Q113）。

## 4. 設定値・確定値

| 項目 | 値 | 出典 |
|---|---|---|
| Project が持つもの | 名前、振り分けのラベル、頼むスペシャルハーネス（と固定する版）、渡す値、承認を省く設定、Runner の指定の6つだけ | vision.md 4章、本人決定 6 |
| 振り分けのラベル | 振り分けの表の中で一意。1つの振り分け先に1つ。接頭辞は `linear.routingLabelPrefix`（既定 `route:`） | 本人決定 2、02 の Q120 |
| 渡す値 | Evaloom は解釈しない。Profile の `params.schema` で形だけ確かめる | 契約 5.2、Q115 |
| 承認を省ける操作 | Profile が `waivable: true` と宣言した操作だけ | 契約 7.3 |
| Profile の版 | SemVer が正本。commit SHA を記録 | requirements 5.4、契約 4.2 |
| 公開済みタグの付け替え | 禁止。見つけたらその版での実行を止める | requirements 5.4、契約 4.2 |
| 登録した Profile の状態 | 暫定運用（`provisional`） | requirements 11.1 |
| 新しい版の反映 | Project とスペシャルハーネスの既定の版へ自動では反映しない | requirements 3.4、4.3、4.4 |
| スペシャルハーネスのソース | Project にコピーしない。参照は Registry で持つ | requirements 3.4 |
| 仕事の種類に固有の値（ローカルパス、テストコマンド、setup、ベースブランチなど） | Evaloom は持たない。要るならスペシャルハーネスが `params` で受け取るか、自分で読む | 本人決定 1・6 |

## 5. インターフェース

[`01-architecture.md`](./01-architecture.md) 5.1 の次のエンドポイント。

- `GET /api/projects`、`POST /api/projects`、`GET /api/projects/{id}`、`PATCH /api/projects/{id}`
- `GET /api/routes`、`POST /api/routes`、`PATCH /api/routes/{label}`、`DELETE /api/routes/{label}`
- `GET /api/harnesses`、`POST /api/harnesses`、`GET /api/harnesses/{id}`、`PATCH /api/harnesses/{id}`
- `GET /api/harness-profiles`、`POST /api/harness-profiles`、`GET /api/harness-profiles/{id}`

Project・スペシャルハーネス・Profile を消すエンドポイントは作らない（Q130）。

テーブルは [`02-data-model.md`](./02-data-model.md) 3.2 の runners、harnesses、harness_profiles、projects、routes。

## 6. エラー処理

| 状況 | 応答 |
|---|---|
| 6項目と ID のほかの入力、形の合わない値 | 400 `validation_failed` |
| 振り分けのラベルの接頭辞の不一致、`ai-ready` と同じ名前 | 400 `validation_failed` |
| ID・ラベルの重複、1つの振り分け先への2つ目のラベル | 409 `conflict` |
| `profileId` が `harnessId` の版でない、渡す値がスキーマに合わない、省けない承認を省く設定に入れた | 422 `precondition_failed` |
| スペシャルハーネスの `repoPath` が無い、Git リポジトリでない | 422 `precondition_failed`。`details` にどの確認で失敗したかを入れる |
| タグが無い、`evaloom/profile.yaml` が無い・読めない、契約の検証（3.4 の表）に通らない | 422 `precondition_failed`。`details` に失敗した項目を全部入れる |
| 同じ版を別の commit で登録 | 409 `conflict`。audit_logs に記録する |

## 7. スコープ外

- Model Registry（requirements 5.3）。Profile が Model ID を文字列で持つ（Q19）
- Profile の改善候補の管理、ロールバック API、影響する Project の検出（requirements 4.4、評価と改善の Phase）
- Project の登録でのローカルパス・リポジトリの検証、コピー許可リスト、setup の許可宛先、テストコマンド。software-engineer へ移した（本人決定 1・6）
- 鍵の登録。Mac mini の CLI で Runner の保管に入れる（[`01-architecture.md`](./01-architecture.md) 3.5）
- Linear の Team の登録。照会先は全体設定 `linear.teamId` の1つだけで、振り分けには使わない（本人決定 2）

## 8. 関連仕様

- [`../contract/harness-contract-v1.md`](../contract/harness-contract-v1.md) ハーネス契約（Profile 定義、承認を省く設定）
- [`00-overview.md`](./00-overview.md) 全体仕様
- [`01-architecture.md`](./01-architecture.md) API 一覧、鍵の保管
- [`02-data-model.md`](./02-data-model.md) projects、routes、harnesses、harness_profiles
- [`04-task-lifecycle.md`](./04-task-lifecycle.md) claim の時点での版・渡す値・Runner の固定
- [`05-runner.md`](./05-runner.md) Profile の中身の読み出しとタグの確認
- [`13-management-ui.md`](./13-management-ui.md) Project・振り分けの表・スペシャルハーネスの画面
- [`15-linear-intake.md`](./15-linear-intake.md) 振り分けの表を使った振り分けと保留の再判定
- [`17-approval.md`](./17-approval.md) 承認を省く設定の効果
- [`19-routing-and-stages.md`](./19-routing-and-stages.md) 振り分け先・段・安全な場所

## 9. 実装単位

- [ ] [Backend] Project の登録・取得・更新 API（6項目）
  - 受け入れ基準: 7つの入力のほかの項目（例 `localRepoPath`、`testCommand`）を含む要求が 400 になる; `profileId` が `harnessId` の版でないと 422; Profile が `params.schema` を持たないとき `{}` 以外の `params` が 422、持つときスキーマに合わない `params` が 422; `harnessId` だけを変える PATCH が 422; 更新で変更前後の値が audit_logs に残る; 一覧と詳細に振り分けのラベルが入る
- [ ] [Backend] 承認を省く設定の検証（3.5）
  - 受け入れ基準: 同じ名前を2回含むと 400; Profile に無い操作が 422（`unknown_operation`）; `waivable: false` の操作が 422（`not_waivable`）; `profileId` を、残っている操作が `waivable: true` でない版に変える PATCH が 422 になり値が変わらない
- [ ] [Backend] 振り分けの表の API（3.2）
  - 受け入れ基準: 接頭辞で始まらないラベル、`ai-ready`、`projectId` と `harnessId` の両方・どちらも無い入力が 400; 同じラベル、同じ Project への2つ目のラベルが 409; 登録・変更・削除が audit_logs に残る; ラベルを消しても `queued` 以降のタスクの振り分け先が変わらない
- [ ] [Backend] スペシャルハーネスの登録・更新 API（3.3）
  - 受け入れ基準: Git リポジトリでない `repoPath` が 422; シンボリックリンクの `repoPath` が実パスで保存される; 別のスペシャルハーネスの版を `defaultProfileId` にすると 422; `defaultProfileId` の設定で `config_missing_profile` の保留の再判定が要求される
- [ ] [Backend] Profile の版の登録（タグの解決、`evaloom/profile.yaml` の読み込み、3.4 の表の検証、SHA の記録）
  - 受け入れ基準: 契約 5.1 の例が登録できる; 3.4 の表の各規則に1つずつ違反した定義がそれぞれ 422 になり、`details` に項目のパスが入る（知らない項目、版の不一致、work の `network.allow`、check の `secrets`、prepare の `access: write`、IP リテラルの宛先、上限を超える `timeoutSec`、リポジトリの外を指すパス、シンボリックリンクの参照、`stages.work` の中にある check の `command`、Adapter が変換できない `toolPermissions`、publish の `network.allow` にあるモデルの API の宛先、形の合わない `slot` を含む）; 同じ版を別の commit で登録すると 409 で audit_logs に残る; 同じ commit の再登録は既存の行を返す; 登録した行の operation_status が `provisional`

## 10. 要確認

| # | 内容 | 案 |
|---|---|---|
| Q125 | Project とスペシャルハーネスの ID の形 | `^[a-z0-9][a-z0-9-]{0,62}$`（英小文字・数字・ハイフン、先頭は英小文字か数字、63 文字以内）。登録後は変えない |
| Q126 | Profile の登録で、タグをどこから読むか | `repoPath` のローカルの複製だけを読む。Evaloom は `repoUrl` から取得しない（Control Plane はスペシャルハーネスの鍵を持たないため）。本人が先に `git fetch --tags` しておく。管理画面に、タグが見つからないときはこの手順を案内する |
| Q127 | 全体設定 `linear.routingLabelPrefix` を変えたとき、新しい接頭辞で始まらないラベルが振り分けの表に残る場合 | 残っている間は設定の変更を 422 にする。本人が先にラベルを登録し直す |
| Q128 | 振り分けのラベルが Linear 側に無いとき | 登録は拒否しない。`linear.teamId` が設定されていれば、登録時に Team のラベルを照会し、無ければ応答に警告を入れる。Evaloom が Linear にラベルを作ることはしない。振り分けの承認の後のラベルの追加が失敗したら、通知の失敗として出す（[`16-linear-feedback.md`](./16-linear-feedback.md)） |
| Q129 | 渡す値（`params`）の大きさの上限 | 正規化した JSON の UTF-8 で 64 KiB まで。超えたら 400 |
| Q130 | Project・スペシャルハーネス・Profile を消せるか | この版では消さない（Execution と記録が参照しているため）。使わなくするには振り分けのラベルを消す。消す機能は、参照の扱いを決めてから足す |
| Q138 | 鍵の `slot` の名前の形（環境変数 `EVALOOM_SECRET_<SLOT>` と保管のファイル名に使う） | `^[a-z][a-z0-9_]{0,31}$`。契約 5.2 に書き足した。Profile の登録で検証する |
