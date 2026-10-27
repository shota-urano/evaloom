# launchdユーザーエージェント

`01-architecture.md`の3.1、3.2、4節に基づく、独立した2つの常駐プロセスのplistテンプレート。
`RunAtLoad`でロード時に起動し、`KeepAlive`で終了後に再起動する。
シェルや`go run`を経由せず、ビルド済みバイナリを直接起動する。

`__EVALOOM_ROOT__`はlaunchdが展開する変数ではない。利用前にリポジトリの絶対パスへ置換する。

## ビルドとplist生成

リポジトリのルートから実行する。生成物はGit管理外の`data/`内に置く。
同名の生成済みplistがある場合、以下の処理は上書きせず失敗する。

```sh
mkdir -p data/bin data/logs data/launchd
(cd frontend && npm ci && npm run build)
(cd backend && go build -o ../data/bin/evaloomd ./cmd/evaloomd)
(cd backend && go build -o ../data/bin/evaloom-runner ./cmd/evaloom-runner)
python3 - <<'PY'
from pathlib import Path
import plistlib

root = Path.cwd().resolve()
for template in sorted((root / "backend/launchd").glob("*.plist")):
    with template.open("rb") as source:
        config = plistlib.load(source)
    config["ProgramArguments"] = [
        value.replace("__EVALOOM_ROOT__", str(root))
        for value in config["ProgramArguments"]
    ]
    for key in ("WorkingDirectory", "StandardOutPath", "StandardErrorPath"):
        config[key] = config[key].replace("__EVALOOM_ROOT__", str(root))
    with (root / "data/launchd" / template.name).open("xb") as target:
        plistlib.dump(config, target)
PY
plutil -lint data/launchd/*.plist
```

launchdはDBとログの親ディレクトリを作成しないため、`data/`と`data/logs/`を事前に用意する。
`evaloomd`はマイグレーションが成功してからHTTP待ち受けを開始する。
DB初期化やポートの確保に失敗した場合は、標準エラーへ理由を出して終了コード1で終了する。
両プロセスともSIGINTとSIGTERMで終了する。

## 待ち受けの確認

`evaloomd`の起動ログに実際の待ち受けアドレスを出力する。
実行中のプロセスのPIDを指定して確認する。

```sh
lsof -nP -a -p <evaloomdのPID> -iTCP -sTCP:LISTEN
```

表示されるTCPリスナーは`127.0.0.1:8080`の1本だけとする。
ホストはコード内でIPv4の`127.0.0.1`に固定し、ホストを変更するフラグは設けない。
Runnerは待ち受けソケットを作らず、管理DBにもアクセスしない。

## 実装時に決めた値と範囲

- 既定ポートは8080。変更する場合は`evaloomd -port <番号>`またはplistの`-port`引数を指定する。`-port 0`はOSが空きポートを割り当てる検証用。
- HTTPヘッダー受信の上限は5秒。SIGTERM等を受けた後のHTTP終了待機も5秒とし、超過した場合は接続を閉じてエラー終了する。
- launchdのラベルは`org.evaloom.evaloomd`と`org.evaloom.evaloom-runner`。バイナリ、DB、ログの配置例は`data/`配下とする。これらは仕様で固定された名称やパスではない。
- 管理画面は`frontend`のビルド成果物（`frontend/dist`）を`evaloomd -web <ディレクトリ>`で同一オリジン配信する。`-web`を省略すると管理画面は配信しない。指定したディレクトリに`index.html`が無い場合は起動時にエラー終了する。`/api/`と`/auth/`以外で拡張子の無いパスは`index.html`を返す（画面側のルーティング用）。
- Runnerは`-config <JSONファイル>`を読み、起動直後からheartbeatとclaimのpullループを動かす。設定例と通信時の動作は次節を参照する。plistは`data/runner.json`を指定するため、登録前に設定ファイルとトークンファイルを用意する。

## Runnerの設定と起動時heartbeat

正本は`docs/specs/05-runner.md`の3.1・5・9節。
以下は現在の実装上の選択を説明したもので、仕様の固定値ではない。

起動引数は`evaloom-runner -config <JSONファイル>`。
設定の各項目を必須とし、未知の項目や複数のJSON値を拒否する。
設定パスの既定値は設けない。launchdのテンプレートは`data/runner.json`を指定する。

```json
{
  "runnerId": "mac-mini",
  "controlPlaneUrl": "http://127.0.0.1:8080",
  "tokenFile": "/absolute/path/data/runner.token",
  "worktreeRoot": "/absolute/path/worktrees",
  "dataDir": "/absolute/path/data/runner"
}
```

3つのファイル・ディレクトリのパスは絶対パスで指定する。
ディレクトリの作成、worktreeの作成、データディレクトリがGit管理外かの検証は、この実装単位では行わない。
データディレクトリは利用者がGit管理外に配置する。

- Runner IDの制約は登録コマンドに合わせる。接続先はHTTP(S)の絶対URLとし、認証情報、クエリ、フラグメントを含むURLを拒否する。パスを含む場合は、その下に`api/runners/{id}/heartbeat`を追加する。
- トークンファイルには登録コマンドが発行したトークンだけを保存する。前後の空白・改行を取り除き、既存の認証と同じ32バイトの正規base64url形式を確認する。トークンファイルは本人だけが読める権限（0600）で用意する。設定やトークンの読み込み失敗・形式不正では通信せず起動を中止する。
- bootIdは起動ごとに生成する16バイトの暗号学的乱数を小文字hexで表し、プロセス内の再送では同じ値を使う。ファイルに保存しない。
- heartbeatには`Authorization: Bearer <token>`を付ける。実行中IDが無いときも`runningExecutionIds`は空配列で送る。応答の`cancelExecutionIds`に対応する登録済みExecutionの中止関数を呼ぶ。未知・終了済みのIDは無視し、中止後も終了処理が登録を解除するまでは実行中IDとして送る。中止関数は繰り返し呼べるものを登録する。
- HTTP要求全体の上限は5秒とし、終了要求のcontextでも通信を中断する。リダイレクトは追わない。heartbeatの応答は200とJSONの中止ID配列を必須にし、1MiBまで読み込む。通信失敗、非200、応答不正では実行中Executionを止めず、エラーを返す。起動時の送信に失敗してもプロセスは次回の送信を待つ。エラー出力にはトークンや応答本文を含めない。
- AdapterとSandboxの停止・`cancelled`の報告はタイムアウトとキャンセルの実装単位で扱う。

## Runnerのpullループと同時実行上限

正本は`docs/specs/05-runner.md`の3.2・9節と`04-task-lifecycle.md`の3.3節。
以下の待機間隔と未実装工程の扱いは、今回の実装上の選択である。

- 起動直後にheartbeatを送り、成功した場合だけ空き枠を確認してclaimを1回送る。各周回の通信後に5秒待つ。実行中が2本でもheartbeatは続ける。起動直後のheartbeatに失敗した場合も、成功するまでclaimせず、bootIdによる前回起動の回収を先に行う。
- claimは同じ接続先の`api/runners/{id}/claim`へBearerトークン付きでPOSTする。要求全体の上限は5秒、リダイレクトは禁止。HTTP 204は仕事なしとして待機する。HTTP 200は最大1MiBのJSONを読み、Execution IDとTask IDを必須にする。それ以外のステータスや応答不正はログに出し、次の周回へ進む。応答本文やトークンはログに含めない。
- Runnerは登録済みExecutionと通信中のclaimの予約枠を合わせて2本までに制限する。claimが仕事なし・エラーなら予約枠を解放し、実行処理を呼ばない。仕事があれば中止関数を登録して別のgoroutineで実行処理を呼ぶ。処理が戻った後に登録を解除する。同じRunnerでpullループを重複起動することは拒否する。
- 通信失敗では実行中の処理を止めない。次の周回でheartbeatとclaimの通信を再試行するが、取得済みExecutionを再実行しない。中止指示ではそのExecutionのcontextを中止し、実行処理が戻るまでは枠とheartbeatのIDを保持する。終了要求では通信と各実行処理のcontextを中止し、開始済みの実行処理が戻るのを待つ。
- `Runner.Run`はclaimの入力とcontextを実行関数へ渡す。実行関数は処理停止と結果報告を終えてから戻る契約とする。現在のCLIは取得した仕事を中止・終了要求まで待機させる仮の関数を渡す。worktree作成、Adapter起動、結果報告は行わず、仕事を成功扱いにしない。CLIの起動ログにも工程未実装を明記する。

## Executionの工程実行器

正本は`docs/specs/05-runner.md`の3.3・9節と`04-task-lifecycle.md`の3.8節。
`backend/internal/runner/execution.go`の`ExecutionStages()`で、仕様表の14工程と
工程番号・Execution状態を固定する。呼び出し側が表を変更しても実行順には影響しない。

- `NewExecutor`に`StageID`ごとの工程関数を渡す。全14工程を必須とし、不足・nil・未知の工程を拒否する。登録後のmapの変更は実行器に反映しない。前回PRがない場合の工程2など、条件付きの処理は工程関数が判断する。
- `Executor.Execute(ctx, claim)`は通常は工程1から、`resumePoint=gate2`なら工程11、`gate3`なら工程12、`publish`なら工程13から実行する。null以外の未知の値や空文字は工程を呼ぶ前に拒否する。この拒否方法は実装上の選択である。
- 工程関数には同じcontextと、実行ごとに確保した`Execution`を渡す。`Execution.Claim`はpullで受け取った入力を保持する。工程がエラーを返したら直ちに打ち切り、再試行や後続工程の呼び出しはしない。Blockも工程側からエラーとして返す。`StageError`で工程番号と元のエラーを保持し、`errors.Is/As`で分類を取り出せる。エラー文字列には工程関数の自由文を含めない。
- `Runner.Run`の実行関数から`Execute`を呼び、その結果の報告を終えてから戻る。工程表の状態は報告用の情報であり、実行器はControl Planeの状態を直接変更しない。工程開始・終了の報告、各工程の中身、再開時のHEAD照合、ログ・使用量、Adapter/Sandboxの停止・タイムアウトは各実装で扱う。contextが中止済みなら次の工程を開始しない。
- 各工程の実装が揃うまではCLIに実行器を接続せず、上記の待機処理を維持する。未実装工程を成功するスタブに置き換えて本番の仕事を進めない。

## Runnerのログ収集と再送

正本は`docs/specs/05-runner.md`の3.4・6・9節、イベント形式は
`02-data-model.md`の5.3節。以下はログ収集の実装上の選択である。

- 工程は`Runner.NewLog(executionID, stream, registrar)`でログを作る。streamは`harness`・`setup`・`test`。stdoutとstderrをそれぞれ`Log.Read`へ渡して並行に読める。別のgoroutineで`Log.Deliver(ctx)`を動かし、全readerの終了後に`Log.Finish(redacted)`を呼ぶ。送信が終了するまでDeliverを待つ。工程のcontextが中止されても、送信用contextを別に保持すればログを送れる。未実装のAdapter・setup・テスト工程をCLIへ接続する変更は行っていない。
- Runnerは`dataDir/logs/<ULID>.log`を新規作成する。既存ファイルを上書きしない。親ディレクトリは0700、ファイルは0600。書き込みはバッファせず、終了時にSyncして閉じる。LF・CRLFをLFへ統一し、末尾に改行のない行にもLFを付ける。空行と64KiBを超える行も収集する。同じstream内ではファイルへの書き込み順に送る。stdoutとstderrの間の順序はreaderが行を書き込んだ順になる。
- Deliverはファイルを読み、最大100行、通常256KiBまでを1バッチで送る。1行が256KiBを超える場合は、その行を分割せずに送る。全履歴をメモリに積まない。新着確認と送信失敗後の再試行は1秒間隔。HTTP要求全体の上限は既存の5秒で、Bearer認証とリダイレクト禁止も共通とする。送信処理が待機・失敗してもreaderはファイルへの書き込みを続ける。書き込み・読み取り・Syncの失敗は呼び出し元へ返す。
- `POST /api/executions/{id}/events`は`{"events":[{"id":"<log ULID>:<行先頭のbyte offset>","stream":"harness","text":"出力行"}]}`を受け、204を返す。1要求は1〜100行。textは改行を含めず、SSEのpayloadは`sourceId`・`stream`・`text`とし、sourceIdには要求のidを保存する。SSEのevent idは従来どおりDBの連番を使う。RunnerにControl PlaneのDB接続を持たせない。
- Control Planeは既存の`execution_events.payload`のsourceIdをExecution内で照合し、重複を排除する。新しいテーブルや列は追加しない。同じExecutionで同じID・payloadの再送は204、同じIDで内容を変えた要求は409。バッチの一部だけは保存しない。認証したRunnerの所有権を同じトランザクションで確認する。終了済み・過去のExecutionにも所有するRunnerの未送信ログを受け付ける。別Runnerの報告は403と監査記録を返す。
- 全行の送信とFinishの完了後、Deliverは渡された`ArtifactRegistrar.RegisterLogArtifact`を呼ぶ。引数にはログのULID・Execution ID・kind（`harness_log`・`setup_log`・`test_log`）・ローカルパス・redactedを渡す。登録側は同じIDの再試行を重複登録しない契約とする。Artifact登録のHTTP経路は仕様未定のため接続せず、新しい公開APIは追加しない。テストではスタブで登録要求とファイル内容を確認する。registrarの省略はエラーとし、登録していないログを登録済みとは扱わない。
- イベント・Artifactの送信失敗は終了後も再試行する。Deliverのcontextを中止すると送信を止め、ローカルファイルを保持する。同じLogでDeliverを再度呼ぶと先頭から読み直し、保存済みイベントのsourceIdで重複を排除する。Runnerプロセス自体の再起動時にログを自動回収する処理は含めない。既存仕様どおり最初のheartbeatで前回のExecutionをinterruptedにする。
- 機密情報の除外は次の実装単位で扱う。Readへ渡す前に除外処理を適用する契約とし、Finishの引数でArtifactのredactedを引き継ぐ。今回の収集処理だけで除外済みとは扱わない。タイムアウト、Adapterの中止、使用量の記録にも変更を加えていない。

結合テストは実際の認証・events受信・SSEを通し、出力の読み取り中にSSEへ届くことを確認する。Artifact登録はスタブで呼び出しを確認する。
HTTPサーバーとDBを閉じている間にログを読み、同じDB・URLで再開して全行が届くことも確認する。
保存成功後にHTTP応答だけを失わせ、再送でイベントが増えないことも検証する。

## macOS通知ヘルパーのビルド・署名・配置

通知方式の正本は`docs/specs/12-notification.md`の3.3節。
上記のバイナリとplistの生成に続けて、ログイン中の本人のアカウントで実行する。
Swiftコンパイラを含むXcode Command Line Toolsが必要になる。
`make verify`はこのSwiftビルド・署名・配置や実通知を実行しない。

リポジトリのルートで、毎回新しいビルド先を作る。

```sh
mkdir -p data
notify_build_dir="$(mktemp -d "$PWD/data/notification-build.XXXXXX")"
notify_app="$notify_build_dir/Evaloom Notifications.app"
mkdir -p "$notify_app/Contents/MacOS"
cp -f backend/launchd/notification-helper/Info.plist "$notify_app/Contents/Info.plist"
xcrun swiftc -O -framework AppKit -framework UserNotifications \
  -module-cache-path "$notify_build_dir/module-cache" \
  backend/launchd/notification-helper/main.swift \
  -o "$notify_app/Contents/MacOS/evaloom-notification-helper"
plutil -lint "$notify_app/Contents/Info.plist"
codesign --sign - "$notify_app"
codesign --verify --strict --verbose=2 "$notify_app"
```

次の処理で`~/Applications/Evaloom Notifications.app`に配置する。
同名のアプリがある場合は上書きせず失敗する。更新する場合は既存アプリを確認し、
本人が退避してから実行する。`/tmp`内のアプリからは通知許可を得られなかったため、
ビルド先から起動せず、配置後のアプリを使う。

```sh
python3 - "$notify_app" <<'PY'
from pathlib import Path
import shutil
import sys

target = Path.home() / "Applications" / "Evaloom Notifications.app"
target.parent.mkdir(parents=True, exist_ok=True)
shutil.copytree(sys.argv[1], target)  # 既存の配置先は上書きしない
PY
```

初回は配置したアプリを起動し、本人が通知を許可する。
システム設定の「通知」で「Evaloom Notifications」の許可も確認する。
以下の`task-001`とポートは、確認するタスクとevaloomdの実ポートに置き換える。

```sh
/usr/bin/open -n -a "$HOME/Applications/Evaloom Notifications.app" --args \
  'evaloom: 完了' "$(printf '通知の確認\ntask-001\nsucceeded')" \
  'http://127.0.0.1:8080/tasks/task-001'
```

ヘルパーは`LSUIElement`を指定してDockに出さず、`UNUserNotificationCenter`で
通知する。クリック時は通知に保存したタスクURLを`NSWorkspace`で開く。
URLは`http://127.0.0.1:<port>/tasks/<taskId>`に限定する。
通知登録後は60秒待って終了する。通知センターからのクリックでmacOSが再起動した
場合も、通知を追加せず応答を受け付ける。

evaloomdはmacOSで`macos`経路だけを登録し、配置先のアプリを`open -n -a`で起動する。
本文に使うのはタスクの件名・ID・分類だけで、PR URL・理由・停止工程・Execution IDは使わない。
分類には検証済みのBlock原因またはエラー分類を使い、値がない場合は通知対象の状態を使う。
完了の分類は`succeeded`とする。これらの表示形式と待ち時間は実装時の判断であり、仕様の固定値ではない。

`open`の成功だけでは通知の許可を確認できない。
evaloomdは呼び出しごとに権限0700の一時ディレクトリを作り、
`EVALOOM_NOTIFICATION_RESULT`環境変数に結果ファイルのパスを設定する。
`open`が環境変数を新しいアプリに引き継ぐため、件名・本文・URLの3引数は変えない。
ヘルパーは[通知許可の取得](https://developer.apple.com/documentation/usernotifications/unusernotificationcenter/requestauthorization(options:completionhandler:))と
通知登録を終えてから、固定コードをファイルに原子的に書き込む。
`sent`以外の結果、起動失敗、結果待ちの30秒超過はNotifierのエラーとして返す。
ファイルは成功・失敗どちらでも削除する。許可拒否を成功扱いせず、既存のDispatcherへ
エラーを返す。失敗イベントとDashboardへの記録は別タスク`evaloom-6ie.3`が扱う。

## launchdへの登録と実機確認

ビルド・署名・配置と通知許可の確認を済ませてから、生成済みplistを本人のguiドメインへ登録する。
既に同じラベルを登録している場合は、この登録手順を繰り返さない。

```sh
launchctl bootstrap "gui/$(id -u)" "$PWD/data/launchd/org.evaloom.evaloomd.plist"
launchctl bootstrap "gui/$(id -u)" "$PWD/data/launchd/org.evaloom.evaloom-runner.plist"
```

起動したevaloomdでタスクを完了または失敗させ、デスクトップへの表示と、
通知をクリックした際にそのタスクの詳細画面が開くことを確認する。
これは`make verify`の範囲外の手動確認とする。
