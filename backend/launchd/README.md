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
- Runnerは`-config <JSONファイル>`を読み、起動直後にheartbeatを1回送信して終了要求を待つ。設定例と通信時の動作は次節を参照する。plistは`data/runner.json`を指定するため、登録前に設定ファイルとトークンファイルを用意する。定期送信とclaimはpullループの実装単位で扱う。

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
- HTTP要求全体の上限は5秒とし、終了要求のcontextでも通信を中断する。リダイレクトは追わない。応答は200とJSONの中止ID配列を必須にし、1MiBまで読み込む。通信失敗、非200、応答不正では実行中Executionを止めず、エラーを返す。起動時の送信に失敗してもプロセスは終了要求を待つ。エラー出力にはトークンや応答本文を含めない。
- この実装単位は起動時の1回送信と、後続のpullループから呼べるheartbeat処理までを持つ。定期送信・再送のスケジューリングとclaimは次の実装単位、AdapterとSandboxの停止・`cancelled`の報告はタイムアウトとキャンセルの実装単位で扱う。

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
