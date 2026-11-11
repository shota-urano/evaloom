# Runnerの鍵の保管

正本は`docs/specs/01-architecture.md`の3.5節、名前と環境変数は
`docs/contract/harness-contract-v1.md`の11章。

登録と入れ替えはMac mini上で次のCLIを使う。値を引数に書かない。

```sh
evaloom-runner -config /absolute/path/runner.json secret set -access read software-engineer/github_token
evaloom-runner -config /absolute/path/runner.json secret set -access write software-engineer/github_token
```

`-access`は必須で、既定値は持たない。端末から入力するときはエコーを止めて
1行を読む。パイプからはEOFまでのバイト列をそのまま読む。空白や末尾の改行も
鍵の一部になる。空の値と環境変数に渡せないNULを含む値は拒否する。
CLIはRunnerの設定を読み、データディレクトリだけに書く。
Runnerトークンの読み込み、Control Planeへの接続、管理DBの読み書きは行わない。

内部の保存形式は`dataDir/secrets/<harness-id>/<slot>`の1ファイル。
先頭行は登録した`read`か`write`、残りが鍵の値。ファイルは0600、
`secrets`以下のディレクトリは0700にする。鍵と使い方を同じファイルで
一時ファイルからrenameして入れ替え、途中までの値を読ませない。
symlink、hardlink、通常ファイル以外、所有者や権限が違うものは拒否する。
データディレクトリは従来どおりGit管理外に置く。

段の実行器は、Sandboxの作成・開始を`Runner.WithStageSecrets`で囲む。
この関数は全宣言を照合してから開始関数を呼ぶ。鍵の不足や不正、prepareでの
登録済みwrite鍵の選択は、開始関数を呼ばず`secretstore.ConfigError`を返す。
呼び出し側は`Status()`と`ErrorClass()`で`failed`と`config_error`を報告する。
work・check・proposalは鍵なしだけを受け付ける。宣言していない鍵は読まない。

開始関数が受け取る`Resolved.Environment()`は段専用の環境変数、
`RedactionValues()`はログと成果物の除外処理へ渡す値である。
Runnerの段の実行器はまだ無いため、このタスクでは開始前の照合境界までを実装する。
Sandboxへの実際の受け渡しと、段のログ・成果物への除外処理の接続は
鍵の受け渡しの後続タスクで行う。
