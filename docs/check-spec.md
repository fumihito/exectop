# exectop チェック仕様

この文書はチェック作者向けの正本です。項目の分類は次のとおりです。

- **必須**: exectop がチェックとして発見・実行するため、または結果を正しく解釈するために必要な契約。
- **推奨**: チェックの安全性、再現性、診断性を高めるため、原則として実装する項目。
- **任意**: 用途に応じて指定・実装する項目。不要なら省略できます。

## 要件一覧

### 必須

- root 配下の通常ファイルとして配置し、実行ビットを付ける。
- 直接起動できる shebang（例: `#!/bin/sh`）を付ける。exectop は shell 経由で起動しない。
- 終了コードで状態を返す。既定では `0=OK`、`2=WARN`、`3=UNKNOWN`、その他は `FAIL`。
- stdout の1行目を短い SUMMARY として出力する。診断情報は stderr または2行目以降に出す。
- 実行時に必要な依存性（コマンド、OS、パス、ネットワーク）を宣言する。

### 推奨

- `set -eu` 等で未定義変数・途中失敗を検出し、チェックを冪等にする。
- timeout より十分短く終了し、ネットワーク処理には独自 timeout も設定する。
- stdout/stderr に機密情報、端末制御文字、大量の不要なデータを出さない。
- 外部入力、引数、環境変数、ファイル内容を検証し、失敗理由を stderr に出す。
- 測定値を返す場合は stdout 1行目を1行の JSON オブジェクトにし、`summary`、`metrics`、任意キーを使う。

### 任意

- 表示名・説明、実行間隔、timeout、タグ、引数、追加環境変数、cwd、nice、出力上限を設定する。
- `runbook` URI、`references`（参照 URL のカンマ区切りリスト）、永続状態（`EXECTOP_STATE_DIR`）、`.exectop.column` による表示カラムを利用する。
- `ok` / `warn` / `unknown` を変更して、終了コードの意味を用途に合わせる。

## 配置・発見・設定

`--root` 配下を再帰走査し、通常ファイルかつ実行ビットを持つファイルを登録します。`--root` を省略した場合、カレントディレクトリに `exectop.d` ディレクトリが存在すればそれを root とし、なければ `./checks` を使用します。ID は root からの `/` 区切り相対パス、GROUP は最初のパス要素です。`.`/`_` で始まるファイル・ディレクトリ、`.toml`、`.md`、`.txt`、`.disabled` は除外します。root 外を指す symlink は既定で除外し、`--follow-symlinks` で許可します。

ヘッダは先頭 32 行かつ 4KiB 内の `# exectop: key=value`（`//`, `;`, `--`, `*`, `/*` も可）です。先頭 512 バイト以内に NUL があるファイルはバイナリとしてヘッダを解析しません。設定キーは `short_name`（表示用短縮名）、`name`（後方互換の表示名）、`description`, `interval`, `min_interval`, `min_interval_fail`, `timeout`, `grace`, `ok`, `warn`, `unknown`, `tags`, `run` (`always`/`once`)、`args`, `env`, `cwd`, `nice`, `output_limit`, `inherit_env`, `references` です。`disabled` は設定できません。有効・無効は実行可能属性、ファイル名、ディレクトリ構造による発見規則だけで決まります。短縮名は前後空白を除去し、Unicode文字数20文字までに切り詰めます。未指定時の短縮名はファイル名です。CLI、ディレクトリ個別設定、ヘッダ、祖先 defaults、グローバル、組み込み既定値の順で解決します。既定値は interval/timeout=10s、grace=2s、ok=[0]、warn=[2]、unknown=[3]、output_limit=64KiB です。安全検査の緩和はチェック設定では行えず、明示的な `--allow-unsafe` だけで有効になります。

実行時には shell を介さず直接起動し、args のみを引数として渡します。stdin=/dev/null、cwd は設定値（既定はチェックのディレクトリ）、独立プロセスグループ、timeout→SIGTERM→grace→SIGKILL、stdout/stderr 各 output_limit の上限を適用します。超過時は `truncated=true` です。`ok`/`warn`/`unknown` 以外の終了コードは FAIL、起動失敗は ERROR です。

## 実行と出力の契約

### 実行環境

既定の環境継承は `PATH`、`HOME`、`USER`、`LANG`、`LC_*`、`TZ`、`TERM` のみです。`--inherit-env` でも `LD_*`、`DYLD_*`、`BASH_ENV`、`ENV` は除外します。`EXECTOP`、`EXECTOP_VERSION`、`EXECTOP_CHECK_ID`、`EXECTOP_MODE`、`EXECTOP_TRIGGER`、`EXECTOP_ITER`、`EXECTOP_TIMEOUT`、`EXECTOP_PREV_STATUS`、`EXECTOP_PREV_TS`、`EXECTOP_STATE_DIR` を付与します。状態ディレクトリは `${XDG_STATE_HOME:-$HOME/.local/state}/exectop/<root-hash>/state/<ID の `/` を `%2F` に置換>/`（または `--state-dir` 配下）に mode 0700 で作成し、`--no-state` では渡しません。`EXECTOP_*` 管理変数はチェック側の追加環境変数では上書きできません。

stdout の 1 行目が SUMMARY、2 行目以降が詳細、stderr は診断情報です。先頭行が `{"summary":"...","metrics":{...}}` なら数値 metrics を保持します。表示時は ESC/C0 制御文字を除去し、JSON/SQLite には原文を保存します。チェックは `setsid` 等でプロセスグループから離脱しないでください。

## Runbook URI

チェックには `runbook` 属性を指定できます。値は失敗時に参照する任意の URI
（例: `https://wiki.example/runbooks/disk`、`file:///opt/runbooks/disk.md`、
`opsrunbook://service/disk`）です。exectop は URI を opaque な文字列として保存・
表示し、内容の取得、実行、外部ハンドラの起動は行いません。空白のみ、制御文字、
16384バイトを超える値は不正です。未設定または参照不能でもチェック結果は変更しません。

`watch --rescan` 中にチェック本体または設定が変わると、直近結果は次回実行まで stale として表示されます。一覧では状態に `*` を付け、詳細画面では変更時刻と stale であることを表示します。変更検出だけでは自動再実行しません。

TTY の `watch` は top 相当の画面更新を行い、起動時・更新時に画面をクリアします。TUI の
root 表示ではサブディレクトリ配下をディレクトリ単位に集約し、Enter でディレクトリ内へ
移動します。root 以外には仮想行 `..` をヘッダ直下に表示し、Enter で親へ戻ります。
集約状態は配下に失敗系（FAIL/TIMEOUT/ERROR/UNKNOWN）が1件でもあれば FAIL、次に WARN、
RUNNING、PENDING、SKIP、OK の順で決定します。上下キーはカラム選択を解除して行選択に
戻します。非TTYや構造化出力はこの画面制御を行いません。

個別チェックの詳細画面では `p` キーでスクリプト本体を `PAGER`（未設定時は `less`）へ
渡して表示します。Pager は shell を介さずコマンドと引数として起動し、終了後に TUI へ
復帰します。

TUI の一覧には次回実行までの時間、連続状態回数、履歴の平均実行時間を表示します。
詳細画面の `HISTORY GRAPH` は直近の状態を ASCII 文字の `O`（OK）、`W`（WARN）、`X`
（FAIL/TIMEOUT/ERROR）、`?`（UNKNOWN）、`-`（SKIP）、`>`（RUNNING）で表します。
