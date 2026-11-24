# exectop check authoring guide

このディレクトリは、exectop のバイナリから `exectop examples` で生成された
チェックの開始地点です。新しいチェックは、監視したいものを一つに絞り、実行可能な
ファイルとして配置してください。雛形は [`check-template.txt`](./check-template.txt)
にあります。

## 必須要件

- `#!/bin/sh` などの shebang を付ける。exectop は shell 経由で起動しない。
- 正常は終了コード `0`、通常の異常は `1`、警告は `2`、判定不能は `3`。
- stdout 1行目は短い要約。構造化する場合は、1行のJSONオブジェクトにする。
- stdout 2行目以降は詳細、stderrは診断情報にする。
- 必要な依存性をヘッダで宣言する（`requires`、`requires_os`、`requires_path`、`network`）。

## 推奨事項

- `set -eu` を付け、不要な副作用を起こさない。
- 機密情報、端末制御文字、大量出力を出さない。
- timeoutより十分短く終了する。ネットワーク処理には独自のtimeoutも設定する。
- 外部入力、引数、環境変数、ファイル内容を検証し、失敗理由を stderr に出す。
- チェックは冪等にし、実行のたびに不要な副作用を起こさない。

## 任意項目

- `short_name`、`description`、`interval`、`timeout`、`tags` などのヘッダ設定。
- `args`、`env`、`cwd`、`nice`、`output_limit`、`runbook` の指定。
- stdout 1行目の JSON による `metrics`・任意キーの出力と `.exectop.column` による表示。
- `EXECTOP_STATE_DIR` を使ったチェック固有の永続状態。

## JSON変数

測定値をカラムに表示する場合、stdoutの最初の行にJSONを1行だけ出します。
`summary` は概要、`metrics` は数値メトリクス、それ以外のキーは任意変数です。

```sh
printf '%s\n' '{"summary":"queue depth","value":12,"threshold":50,"unit":"items"}'
```

JSONの値は `.exectop.column` からキー名で参照できます。値がない場合は `-` と表示
されます。JSONでない通常の1行出力も使用できますが、任意カラムは使えません。
JSON文字列に外部入力を埋め込むときは、必ず適切にJSONエスケープしてください。

## カラム

`.exectop.column` はディレクトリ単位の表示設定です。空白またはカンマ区切りで、
表示順にキーを書きます。

```text
status name value threshold unit summary
```

組み込みキーは `status`, `name`, `summary`, `interval`, `history`, `group`, `last`,
`duration`, `path` です。それ以外はJSONの任意変数です。

現在のディレクトリに設定がなければ親を順に探し、rootにもなければ既定カラムを使います。
子ディレクトリの設定は親への追記ではなく、最も近いファイルによる置換です。

## 実行環境と依存性

- stdinは `/dev/null`、cwdはチェックのディレクトリ（ヘッダの `cwd` で変更可能）。
- PATHなどの環境変数は限定継承。必要な入力はヘッダの `env` や安全な既定値で定義する。
- Linux専用の `/proc` や `/sys` を使う場合は、説明と利用不能時の判定を実装する。
- 外部コマンド、ファイル、ネットワーク、権限への依存性をコメントで列挙する。
- SQLiteはexectopバイナリに内蔵され、外部 `sqlite3` CLIは不要。ただしDB、state、socket
  の保存先は書き込み可能である必要がある。
- 対象スクリプト自身が消えた場合、exectopは `VANISHED` として扱う。

依存性はヘッダで宣言できます。

```text
# exectop: requires=awk,df requires_os=linux requires_path=/proc/meminfo network=required
```

`requires` はPATH上のコマンド、`requires_os` は実行OS、`requires_path` は存在が必要な
パスです。`network=required` はネットワークを使うという宣言で、到達性そのものは
チェック本体が判定します。宣言された依存性がない場合、実行前に `ERROR` になります。
SQLiteは内蔵ドライバのため `requires=sqlite3` は書きません。

## 作成後の検証

```sh
chmod 700 ./new-check.sh
sh -n ./new-check.sh
./new-check.sh
exectop list --root .
exectop run --no-daemon --root . --output json new-check.sh
exectop validate --root . new-check.sh
```

確認すること:

1. stdoutの1行目が有効なJSON（構造化出力の場合）である。
2. `.exectop.column` の全キーが期待どおり表示される。
3. 成功・警告・異常・依存性不足の終了コードが意図どおりである。
4. timeout、権限不足、入力不正、対象消失を安全に扱う。
5. 監視対象や依存コマンドがない環境でも、理由がstderrまたはJSONに残る。

`exectop validate` はチェックを実行せず、設定エラーと宣言依存性を検証しながら、各チェックの依存性も表示します。
実測値や副作用を確認する場合は `run --no-daemon` を使い、必要なら一時rootで試します。

コマンドの使い分け:

- `list` は発見されたチェックと解決済み設定を見るための表示コマンドです。
- `validate` は実行前の設定・依存性検査です。チェック本体は実行しません。
- `run` は全チェックを通常ポリシーで実行します。`check ID` は指定したチェックだけを強制実行します。
- `examples` はサンプル一式の導入、`generate PATH` は新しいチェックの雛形作成です。

## 推奨workflow

1. `exectop generate --root ./checks new-check.sh` で `UNKNOWN` の雛形を作る。
2. 監視目的、閾値、依存性、OS、ネットワーク、timeoutを決めて実装する。
3. `exectop validate --root ./checks new-check.sh` で設定と宣言依存性を確認する。
4. 一時ディレクトリで `exectop run --no-daemon --no-state --root TEMP` を実行する。
5. 問題がなければ本番rootへ配置し、失敗した場合は新しいファイルを削除して戻す。

`--no-daemon` はdaemonのDB/socketを共有しない試験用、`--no-state` はチェック固有stateを
作らない試験用です。これはOS sandboxではないため、ネットワーク・ファイル変更・外部コマンド
の副作用はチェック作者が明示的に避けるか、一時環境で実行してください。
