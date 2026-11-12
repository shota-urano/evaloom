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
`Runner.ExecuteStages`は、この照合を段の実行器とSandboxの起動前に行う。
`StageInvocation.SandboxSpec`で、照合した鍵を`SandboxSpec.Secrets`のslotと値へ
変換する。SandboxはランタイムのCLI環境へ値を置き、引数には変数名だけを渡す。
work・check・proposalの鍵なし検査はSandbox側でも行う。

段の実行器は`StageInvocation.NewLog`で作ったログへ標準出力・標準エラーを渡す。
このログはRunnerトークンと渡した鍵を除外してから、ローカル保存・events送信・
ログ成果物の登録を行う。モデルの認証値は`NewLog`の追加引数に渡せる。
`StageInvocation.CollectArtifact`は、readerから受け取った内容をメモリ内で除外処理し、
処理後のファイルと`Redacted`付きの登録情報を返す。既存の成果物登録処理は
このファイルを送信する。JSONのエスケープされた文字列も除外対象にする。

段が終わると、Runnerは保持する`out/`と`result.json`、終了報告の結果JSONと
質問を除外処理する。後続の段は処理後の`out`を入力として受け取る。
段の実行器は全プロセスと収集処理が止まってから戻る。Runnerはその後、
段専用の鍵・Sandbox設定・ログが参照する値を解放する。
コンテナとAdapterの組み立て、成果物登録の呼び出しは既存の段実行器の境界に残る。
