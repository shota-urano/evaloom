# JUnit XML の解析

`ParseJUnitXML` は仕様10の3.3で必要なテストIDと件数を読み取る。
`testsuite` と `testsuites` をルートとするレポート、および入れ子の
`testsuite` を扱う。パスの解決、Sandbox、DBへの保存、Gate判定は呼び出し側が担当する。

`Total` はskipを含む件数で、`Executed()` はskipを除いた件数を返す。
`Failed` は `failure` と `error` の件数を合わせる。
`TestIDs` にはskipを除いたテストのIDを格納し、重複も保持する。
親の集計値を子の件数に加算しない。集計属性と明細が食い違う場合は解析エラーを返す。
明細がなく集計属性だけがある場合は件数を返し、IDは取得不能として扱う。

## テストIDの規則

仕様10の3.3.2（Q38）に従い、`classname`と`name`を`.`でつなぐ。
`classname`が無いか空なら`name`だけを使う。`id`属性は使わない。
基準値取得とGate 2は同じ解析器を使う。

skipしたcaseを含め、`name`が無いか空のcaseが1件でもあれば、
`IDsAvailable=false`、`TestIDs=nil`として件数だけを返す。
取得できた一部のIDを完全な一覧として返すことはしない。
