# launchdユーザーエージェント

`01-architecture.md`の3.1、3.2、4節に基づく、独立した2つの常駐プロセスのplistテンプレート。
`RunAtLoad`でロード時に起動し、`KeepAlive`で終了後に再起動する。
シェルや`go run`を経由せず、ビルド済みバイナリを直接起動する。

このタスクではplistを生成・検査するだけで、launchctlへの登録やホスト側への配置は行わない。
`__EVALOOM_ROOT__`はlaunchdが展開する変数ではない。利用前にリポジトリの絶対パスへ置換する。

## ビルドとplist生成

リポジトリのルートから実行する。生成物はGit管理外の`data/`内に置く。
同名の生成済みplistがある場合、以下の処理は上書きせず失敗する。

```sh
mkdir -p data/bin data/logs data/launchd
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
- HTTPハンドラは空の`net/http.ServeMux`。API共通規約、認証、UI配信は後続タスクで実装する。
- Runnerは終了要求を待つ起動骨格だけを持つ。設定読み込み、bootId、heartbeat、pullループは`05-runner.md`の実装単位で扱う。

launchctlによる起動確認は、ホスト変更を伴うため本タスクでは実施しない。
