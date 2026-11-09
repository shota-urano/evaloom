# 本人のパスワード設定

仕様は[`18-remote-access.md`](../../../docs/specs/18-remote-access.md)の3.3・3.8節、確定事項は[`00-overview.md`](../../../docs/specs/00-overview.md)のQ89・Q90を参照する。

```sh
evaloomd -db <DBのパス> owner set-password -id <ID>
```

端末ではパスワードを非表示で2回入力する。一致しない場合や入力が途中で終わった場合は、DBを作成・更新しない。テストとデモ向けに標準入力のパイプも受け付ける。改行で区切った2回分を渡す。末尾の改行とCRLFを除き、空白を含めて入力した値をそのまま使う。

Q90が実装時に委任した最小長は15文字とする。UTF-8のUnicodeコードポイントで数え、文字種の組み合わせは要求しない。

ハッシュにはargon2idを使う。[RFC 9106の4節](https://www.rfc-editor.org/rfc/rfc9106.html#section-4)の第2推奨構成に従い、メモリ64 MiB、反復3回、並列度4、salt 16バイト、出力32バイトとする。毎回暗号学的乱数でsaltを作り、方式・バージョン・パラメータ・salt・出力をPHC形式の文字列にして`owner_credentials.password_hash`へ保存する。

資格情報は最大1行とし、IDを変更した場合も最初の`created_at`を保つ。資格情報の更新、`owner_sessions`の全行削除、`audit_logs`への記録を1つのトランザクションで行う。どれかに失敗したらすべて取り消す。

監査記録は`actor=owner`、`action=owner.set_password`、`target_type=owner_credentials`、`target_id=設定したID`とする。パスワードとハッシュは監査記録やコマンド出力に含めない。
