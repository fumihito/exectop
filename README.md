# exectop

実行可能なチェック・スクリプトを定期実行し、状態・履歴・出力を一貫して確認するための軽量な監視ツールです。

## 最短の使い方

```sh
./build
./exectop
```

カレントディレクトリに `exectop.d/` があればそこをチェック root として全件を実行します。なければ `./checks/` を使います。

## 設計方針

- **スクリプト中心**: チェックは実行可能ファイルとして配置します。言語やランタイムを固定せず、終了コードと標準出力を結果として扱います。
- **一貫した実行管理**: daemon が root 単位の実行、鮮度判定、同時実行制御、結果配信を管理します。複数の端末や cron から同じ状態を参照できます。
- **結果を失わない**: 最新結果と履歴を SQLite に保存し、daemon 再起動後も復元します。stdout・stderr・数値 metrics も保持します。
- **安全な既定値**: チェックは shell 経由ではなく直接起動し、タイムアウト、出力上限、プロセスグループ、root とファイルの権限を検査します。

## ディレクトリ構成

```text
project/
├── exectop.d/             # 既定のチェック root（存在時に優先）
│   ├── disk/
│   │   └── usage.sh
│   ├── network.sh
│   └── .exectop.column     # watch の表示カラム（任意）
├── checks/                  # exectop.d/ がない場合の既定 root
├── exectop.toml            # root 全体の設定（任意）
└── .exectop.toml           # ディレクトリ単位の設定（任意）
```

チェックIDは root からの相対パスです。例えば `exectop.d/disk/usage.sh` のIDは `disk/usage.sh` になります。stdout 1行目を JSON オブジェクトにすると、そのキーを `.exectop.column` のカラムとして表示できます。設定は親ディレクトリを継承し、rootにもなければ既定カラムを使います。

## チェックの書き方

```sh
#!/bin/sh
# exectop: short_name="ディスク使用率" interval=30s timeout=5s tags=disk

usage=$(df -P / | awk 'NR == 2 { print $5 }')
case "$usage" in
  [0-7][0-9]%|[0-8][0-9]%) exit 0 ;;
  *) echo "使用率が高すぎます: $usage" >&2; exit 1 ;;
esac
```

- 終了コード `0` は既定で OK、`2` は WARN、`3` は UNKNOWN、それ以外は FAIL。対象スクリプトの消失は VANISHED として区別します。
- 標準出力の1行目を SUMMARY として表示し、2行目以降を詳細として保持します。
- `short_name` は表示用の短縮名です。前後空白を除去し、Unicode文字数20文字までに切り詰めます。
- `name` は `short_name` の互換キーです。未指定時のNAMEはファイル名です。
- 設定可能な主な項目は `interval`、`min_interval`、`timeout`、`grace`、`tags`、`args`、`env`、`cwd`、`references` などです。必須・推奨・任意の分類は [チェック仕様](docs/check-spec.md)を参照してください。

## 基本操作

| コマンド | 用途 |
| --- | --- |
| `exectop` | `watch` の省略形。継続監視を開始。 |
| `exectop oneshot` | 全件を一度だけ実行し、結果を表示。旧来の `run` も利用可能。 |
| `exectop list` | チェックと解決済みメタデータを表示。実行はしない。 |
| `exectop validate` | チェックを実行せず、設定と宣言依存性を検証。依存性一覧も表示。 |
| `exectop check ID` | 指定したチェックだけを強制実行。 |
| `exectop run` | 全チェックを通常の鮮度・キャッシュポリシーで一度実行。 |
| `exectop watch` / `exectop top` | 継続監視。TTYではTUI、非TTYではイベント出力。 |
| `exectop history ID` | 指定チェックの実行履歴を表示。 |
| `exectop daemon status\|stop\|restart` | daemon の状態確認・停止・再起動。 |
| `exectop db path\|vacuum\|prune` | データベースの場所確認・保守。 |
| `exectop archive create FILE` | 現在の `exectop.d/` をgzip圧縮tarとして保存。 |
| `exectop archive extract SOURCE` | ローカルパス、HTTP/HTTPS/FTP URLから `exectop.d/` を展開。 |
| `exectop examples` | 配布済みのサンプルチェック一式をインストール。 |
| `exectop generate PATH` | 新しいチェックの実行可能な `UNKNOWN` 雛形を作成。 |

`list` は発見・設定の表示、`validate` は実行前検査です。`run` は全件の通常実行、`check ID` は指定したチェックの強制実行です。`examples` はサンプル導入、`generate` は新規チェック作成に使います。

サブコマンドを省略してフラグだけ指定することもできます。

```sh
./exectop --root ./checks -o json
./exectop --tag disk
./exectop --no-daemon
```

### チェックセットの配布

Goバイナリとアーカイブだけを配布先へ持ち込み、次のように展開できます。

```sh
./exectop archive create checks.tar.gz --root ./exectop.d
./exectop archive extract checks.tar.gz --mode merge
./exectop archive extract https://example.test/checks.tar.gz --mode replace --target ./exectop.d
```

アーカイブはgzip圧縮tarです。展開先が存在する場合のモードは `merge`（既存のローカル変更を保持し、未変更なら更新）、`replace`（全置換）、`refuse`（既存なら拒否）です。既定は安全側の `refuse` で、既存の `exectop.d/.archive.data` に前回アーカイブのファイルSHA-256を保存します。

## 出力

TTYでは表形式、パイプやリダイレクトではJSONLが既定です。表形式では短いNAMEとrootからの相対パスを分離して表示します。

```text
ST  NAME                  LAST               DUR   CODE  SUMMARY  PATH
OK  ディスク使用率          now                4ms   0              disk/usage.sh
```

`PATH` はチェックID（rootからの相対パス）です。機械処理には `-o json` または `-o jsonl` を使用してください。

## 設定と実行モデル

- 設定の優先順位は、CLI、ディレクトリ個別設定、チェックヘッダ、祖先のdefaults、グローバル設定、組み込み既定値です。
- daemon は root の正規パスとユーザーごとに1つです。複数クライアントからの同一チェック要求は重複実行せず、結果を共有します。
- 鮮度期間内の要求はキャッシュ結果を返し、`--force` で再実行できます。
- `--no-daemon` は単独実行用です。`--db` を指定した場合もdaemonを使わず永続化します。
- 既定の状態・DB・socketの場所はユーザー単位で管理され、rootごとに分離されます。

## 安全性と制約

- 実行ファイルは直接起動し、shellによる引数解釈を行いません。
- timeout後にSIGTERM、grace経過後にSIGKILLをプロセスグループへ送ります。
- stdout/stderrには個別の出力上限があります。超過時は `truncated=true` になります。
- rootやチェックの所有者・書き込み権限を検査します。必要な場合だけ明示的に `--allow-unsafe` を指定します。
- `setsid` などでプロセスグループから離脱した子プロセスを完全に回収できない場合があります。詳細は [既知の制約](docs/limitations.md)を参照してください。

## ビルドとテスト

```sh
./build
go test ./...
go test -race ./...
go vet ./...
```

`build` は引き数なしなら `go build ./src/cmd/exectop` を実行し、引き数ありならそのまま `go build` に渡します。

## 終了コード

| コード | 意味 |
| --- | --- |
| `0` | 結果がすべてOK。 |
| `1` | FAIL、TIMEOUT、ERRORなどがあり、既定のfail-on判定に該当。 |
| `2` | WARNがあり、既定のfail-on判定に該当。 |
| `3` | UNKNOWNがあり、既定のfail-on判定に該当。 |
| `64以上` | 引き数・設定・実行環境などexectop自体のエラー。 |

## 関連資料

- [設計書](docs/exectop-design.md)
- [チェック仕様](docs/check-spec.md)
- [既知の制約](docs/limitations.md)
