# exectop 設計書(統合版)

- 版: 1.1 — 本書が唯一の正本。旧名 cmdtop、および「第1部 v0.1」「第2部 v0.2」は本書に統合済みで、参照しないこと
- 対象: 実装担当エージェント
- 実装言語: Go 1.22+ / 対象OS: Linux(他 Unix は best effort)

---

## 1. 目的と非目標

### 1.1 目的

特定ディレクトリ配下に置かれた実行ファイル群(以後「チェック」)を再帰的に発見し、並列に実行し、終了コードから状態を判定し、結果を一覧する CLI ツール。

- `run`: 全チェックを一度実行(または鮮度ポリシーに従い前回結果を返却)し、結果表を出力し、集約終了コードで終了する。
- `watch`: `top` のようにチェックを継続的に再実行し、画面上で結果を更新し続ける。
- 実行本体は root ディレクトリ単位の常駐プロセス(daemon)が持ち、複数の exectop 呼び出し(cron・複数端末)の間で「最終実行時刻」「実行中」「前回結果」を一貫させる。
- 実行結果は SQLite に永続化し、履歴の参照と daemon 再起動後の復元に使う。

### 1.2 非目標(v1 では扱わない)

- チェック間の依存関係・実行順序の保証(全チェックは独立とみなす)
- 複数ホストへの分散実行、リモート収集
- アラート通知(メール/Slack 等)
- サンドボックス(seccomp/namespace)。信頼済みのチェックを前提にする(§14)
- OS 起動時の daemon 自動起動(systemd unit の雛形出力は任意項目)

### 1.3 設計上の前提(推測を含む。発生率は設計者の見積り)

| # | 前提 | 種別 | 見積り |
|---|---|---|---|
| P1 | 単一ホスト上で、チェックの作者は運用者本人または信頼できる同僚 | 推測 | 85% |
| P2 | チェック数は数十〜数百、1本あたり数十ms〜数秒 | 推測 | 80% |
| P3 | 主用途は対話的な TTY だが、cron/CI からの非対話実行もある | 推測 | 75% |
| P4 | Linux 固有 API(プロセスグループ、/proc、SO_PEERCRED)に依存してよい | 推測 | 95% |
| P5 | SQLite の書き込みは想定規模ではボトルネックにならない(§9.3) | 事実+推測 | 90% |

---

## 2. 用語

| 用語 | 意味 |
|---|---|
| root | チェックを探索する起点ディレクトリ(`--root`, 既定 `./checks`) |
| root-hash | root の正規パス(`filepath.Abs` → `EvalSymlinks`)の SHA-256 先頭 16 hex |
| チェック(check) | root 配下で発見された実行可能ファイル1本。ID は root からの相対パス |
| 実行(execution / run) | チェックの1回の起動〜終了 |
| 結果(result) | 1回の実行から得た状態・終了コード・所要時間・出力 |
| 状態(status) | `OK / WARN / FAIL / UNKNOWN / TIMEOUT / ERROR / VANISHED / RUNNING / PENDING / SKIP` |
| グループ | root 直下からのサブディレクトリ名。表示上の分類にのみ使う |
| daemon | root × uid ごとに 1 つ常駐する実行所有者プロセス |
| クライアント | `exectop run/watch/list/...` として起動される CLI。daemon への RPC と描画を担う |
| cached | 鮮度ポリシーにより実行せず前回結果を返した応答 |
| joined | 実行中のチェックの完了を待って同じ結果を受け取った応答 |

---

## 3. 全体アーキテクチャ

```
  exectop run/watch/list/history        exectop run (cron)        exectop watch (別端末)
        |  (client)                          |                          |
        +------------- UNIX socket (NDJSON RPC) ----------------------------+
                                             |
   +-----------------------------------------v---------------------------------------+
   |  exectop daemon  (root × uid ごとに 1 プロセス)                                   |
   |                                                                                 |
   |   RPC server ──> Coordinator ──> Scheduler ──> Runner pool ──> fork/exec         |
   |       |             |  ^            |                |                          |
   |       |             |  |            |                v                          |
   |       |             |  +------ MemStore (最新結果・実行中・履歴リング) <──+     |
   |       |             |                |                                    |     |
   |       |             +---(結果イベント fan-out)---> 購読クライアント        |     |
   |       |                                                                  |     |
   |       +--- history クエリ ---> Persist reader                             |     |
   |                                  ^                                       |     |
   |   Persist writer  <--- bounded queue <----- 結果 ------------------------+     |
   |        |  (バッチ, 1 tx / ≤N 件 or ≤T ms)                                       |
   |        v                                                                        |
   |   SQLite (WAL)  $XDG_STATE_HOME/exectop/<root-hash>/exectop.db                    |
   +---------------------------------------------------------------------------------+
```

コンポーネントの責務:

| コンポーネント | 所在 | 責務 |
|---|---|---|
| Discover | daemon | root を走査し、実行対象ファイルの一覧(パス, 所有者, パーミッション)を返す |
| Spec | daemon | ファイルヘッダ・ディレクトリ設定・グローバル設定からチェックごとの実行仕様を確定する |
| Coordinator | daemon | 鮮度ポリシー(§7)の判定、同時要求の合流(join)、結果の fan-out。Scheduler と Runner pool を所有 |
| Scheduler | daemon | `watch` 用の周期投入。重複実行の抑止 |
| Runner | daemon | 1実行のプロセス制御(起動、環境、出力上限、タイムアウト、プロセスグループ kill) |
| MemStore | daemon | チェックごとの最新結果・実行中フラグ・履歴リングを保持し、スナップショットを配る。**鮮度判定の正はここ** |
| Persist | daemon | 結果をキュー経由でバッチ書き込みし、履歴クエリと起動時復元を提供する |
| RPC server / client | daemon / クライアント | NDJSON プロトコル(§10) |
| Launcher | クライアント | daemon の自動起動と接続リトライ(§11.2) |
| Renderer / TUI | クライアント | 非対話出力 / 対話画面 |

各コンポーネントは goroutine とチャネルで疎結合にし、共有状態は MemStore のみ(mutex 保護)。

**単独モード**: `--no-daemon` で daemon を使わず、クライアントプロセス内で Discover〜Runner を直接動かす(§8.6)。退避経路であり、Phase 1 の完成形でもある(§21)。

---

## 4. チェック(配置される実行ファイル)の仕様

この節が「契約」であり、チェック作者向けの公開仕様になる。実装者はこの節を `check-spec.md` として別出力すること。

### 4.1 配置と発見ルール

1. root 配下を再帰的に走査する。走査順は各ディレクトリ内でファイル名の辞書順(バイト順)。
2. 対象は **通常ファイル** かつ **実行ビット(所有者/グループ/その他のいずれか)が立っているもの**。
3. 以下は無視する:
   - 名前が `.` または `_` で始まるファイル・ディレクトリ(`_lib/`, `_helper.sh` など、共通関数の置き場に使う)
   - 拡張子が `.toml`, `.md`, `.txt`, `.disabled` のファイル
   - root の外を指すシンボリックリンク(既定)。`--follow-symlinks` で許可
4. チェック ID は root からの相対パス(例: `disk/usage.sh`)。表示用短縮名 `short_name` はヘッダで指定でき、前後空白を除去して Unicode 文字数20文字までに切り詰める。未指定時はファイル名を使う。既存の `name` は `short_name` の互換エイリアスとする。
5. ディレクトリ階層は実行対象の有効・無効を決める配置規約であり、TUI の表示階層でもある。設定値やヘッダでチェックを無効化してはならない。root 表示では各ディレクトリ配下を1行に集約し、Enter でそのディレクトリへ移動する。
6. 発見は daemon 起動時に1回。`rescan`(§10.3)または `--rescan=<duration>`(既定 0 = 再走査なし)で再走査でき、追加/削除/更新(mtime 変化)を反映する。

サンプルの配置例は次のとおり。`host/` の直下にはチェックを置かず、分類ディレクトリだけを置く。

```text
examples/
├── host/{io,memory,disk,software,processors}/
└── net/{internet,ethernet}/
```
`processors/` は load average 用の追加分類である。`net/ethernet/` には Ethernet interface の
RX/TX error 累積値を検査するチェックを配置する。

### 4.2 メタデータ

チェックごとの実行仕様は次の優先順で解決する(上が強い):

1. CLI フラグによる一括上書き(`--timeout`, `--interval` など)
2. ディレクトリ設定 `.exectop.toml` の `[checks."<basename>"]`(そのディレクトリ内の特定ファイルへの上書き)
3. ファイル先頭のヘッダコメント
4. ディレクトリ設定 `.exectop.toml` の `[defaults]`(最も近い祖先ディレクトリから root へ順に適用)
5. グローバル設定 `exectop.toml`(root 直下または `--config`)
6. 組み込み既定値

#### 4.2.1 ヘッダコメント

ファイル先頭 **32 行以内**、かつ先頭 4KiB 以内に現れる、次の形式の行を解釈する。

```
<任意のコメント記号>  exectop: key=value key=value ...
```

- コメント記号は `#`, `//`, `;`, `--`, `*`, `/*` のいずれか(前後の空白は無視)。
- `exectop:` の後は空白区切りの `key=value`。値に空白を含む場合はダブルクォートで囲む。
- 複数行あれば結合する(後の行が同じキーを上書き)。
- 先頭 512 バイトに NUL を含む(バイナリと判断される)ファイルはヘッダ解析をしない。バイナリは §4.2.2 の設定ファイルで指定する。

例:

```sh
#!/usr/bin/env bash
# exectop: name="Root disk usage" interval=30s timeout=5s min_interval=20s
# exectop: tags=disk,storage warn=2
```

#### 4.2.2 ディレクトリ設定 `.exectop.toml`

ディレクトリ設定およびグローバル設定は実行条件の上書きに使用できるが、チェックの
有効・無効を制御するキーは持たない。`disabled` は予約禁止キーであり、指定された場合は
設定エラーとする。有効・無効は §4.1 のパス・ファイル発見規則だけで決定する。

```toml
[defaults]
interval = "10s"
timeout  = "5s"

[checks."latency_probe"]      # バイナリなど、ヘッダを書けないファイル向け
name         = "DB latency"
interval     = "5s"
timeout      = "3s"
min_interval = "5s"
warn         = [2]
```

#### 4.2.3 キー一覧

| key | 型 | 既定 | 意味 |
|---|---|---|---|
| `short_name` | string | ファイル名 | 表示用短縮名(20文字まで) |
| `name` | string | `short_name` | `short_name` の互換エイリアス |
| `description` | string | "" | 説明(詳細画面に表示) |
| `runbook` | string | "" | 失敗時に参照する任意の opaque URI。内容の取得・実行・外部ハンドラ起動は行わない |
| `interval` | duration | グローバル既定 `10s` | **watch のスケジュール間隔**。開始時刻基準(§8.2)。`min_interval` より短ければ `min_interval` に切り上げ、警告を出す |
| `min_interval` | duration | `0` | **最低実行周期**。この期間内に再度要求されても実行せず、前回結果を返す(§7)。`0` は毎回実行 |
| `min_interval_fail` | duration | `min_interval` と同じ | 前回が FAIL/TIMEOUT/ERROR/UNKNOWN のときに使う最低周期。短くすると異常時だけ早く再試行する |
| `timeout` | duration | グローバル既定 `10s` | これを超えたら TIMEOUT 扱いで kill |
| `grace` | duration | `2s` | SIGTERM から SIGKILL までの猶予 |
| `ok` | int list | `[0]` | OK と判定する終了コード |
| `warn` | int list | `[2]` | WARN と判定する終了コード |
| `unknown` | int list | `[3]` | UNKNOWN と判定する終了コード |
| `tags` | string list | [] | フィルタ用タグ(カンマ区切り) |
| `run` | `always` / `once` | `always` | `once` は `watch` でも初回のみ実行(静的検査向け) |
| `args` | string list | [] | 起動時に渡す引数 |
| `env` | `K=V` list | [] | 追加環境変数 |
| `cwd` | string | チェックのあるディレクトリ | 作業ディレクトリ |
| `nice` | int | 0 | `setpriority` で設定する nice 値(0 なら変更なし) |
| `output_limit` | bytes | グローバル既定 `64KiB` | stdout/stderr 各々の保持上限 |

`ok` / `warn` / `unknown` に含まれない終了コードはすべて **FAIL**。

### 4.3 終了コード → 状態

| 終了の仕方 | 状態 | 備考 |
|---|---|---|
| exit ∈ `ok`(既定 0) | OK | |
| exit ∈ `warn`(既定 2) | WARN | |
| exit ∈ `unknown`(既定 3) | UNKNOWN | 判定材料が得られなかった等 |
| その他の exit(既定 1, 4 以上) | FAIL | |
| シグナルで終了 | FAIL | `reason: signal SIGxxx` |
| `timeout` 超過で exectop が kill | TIMEOUT | 集約時は FAIL 相当 |
| 起動失敗(ENOEXEC, EACCES, shebang 先が無い等) | ERROR | 集約時は FAIL 相当 |
| 対象スクリプトが実行前または実行中に消失 | VANISHED | ERROR とは区別し、集約時は FAIL 相当 |

既定を 0/1/2/3 = OK/FAIL/WARN/UNKNOWN としたのは、素朴なシェルスクリプト(`exit 1` = 失敗)をそのまま置いても意図通りに動くことを優先したため。Nagios 互換(1=WARN, 2=CRIT)が必要なチェックは `ok=0 warn=1` を書く。

### 4.4 出力規約

- **stdout の 1 行目 = サマリ**。一覧表の SUMMARY 列にそのまま出す(表示時は 1 行・幅で切り詰め)。
- stdout の 2 行目以降 = 詳細。詳細画面 / `--verbose` / JSON で見せる。
- stderr = 診断情報。状態判定には使わない。ERROR/FAIL 時に詳細画面へ併記する。
- 各ストリームは `output_limit` を超えた分を切り捨て、`truncated: true` を記録する。先頭を保持し末尾を捨てる。
- 出力は UTF-8 とみなす。表示時に制御文字・ESC シーケンスは除去する(§14.4)。
- **出力は SQLite に保存される**(§9)。秘密情報(トークン、パスワード)を出力しないこと。
- **構造化出力(オプション)**: stdout の 1 行目が JSON オブジェクトなら、全キーを任意変数として結果に保持する。`summary` は SUMMARY 列、`metrics`(数値 map)は従来どおり `run_metrics` に保持する。任意キーは TUI のカラムファイルから参照できる。

#### 4.4.1 任意変数とディレクトリ別カラム

チェックは stdout の最初の行に JSON オブジェクトを出力できる。2 行目以降は詳細出力
として扱う。結果の `variables` に全キーを保持し、`summary` と `metrics` は互換のため
専用フィールドにも反映する。`variables` は RPC/JSON に含め、永続化結果の復元時も
stdout の先頭行から再構成する。

watch の表示カラムは、表示中ディレクトリの `.exectop.column` から読む。内容は空白または
カンマ区切りのキー列で、組み込みキーは `status`, `name`, `summary`, `interval`,
`history`, `group`, `last`, `duration`, `path`、その他は `variables` のキーとする。
現在のディレクトリにファイルがなければ親を順に探索し、root にもなければ既定カラムを
使用する。子ディレクトリの設定は最も近いファイルで置換され、追記ではない。

### 4.5 実行時契約(exectop がチェックに保証すること)

| 項目 | 内容 |
|---|---|
| 起動方法 | シェルを介さず直接 `execve`。シェバンは kernel が解釈する。引数は `args` のみ |
| stdin | `/dev/null` |
| cwd | `cwd`(既定: チェックファイルのあるディレクトリ) |
| 環境変数 | 既定は最小セットのみ継承: `PATH`, `HOME`, `USER`, `LANG`, `LC_*`, `TZ`, `TERM`。`--inherit-env` で全継承(ただし `LD_PRELOAD` 等の除外リストは常に適用) |
| 追加環境変数 | 下表 |
| プロセスグループ | チェックは独自のプロセスグループのリーダーになる。kill はグループ単位で行う |
| タイムアウト | `timeout` 超過で `SIGTERM` → `grace` 後に `SIGKILL`。グループ全体に送る |
| 同時実行 | 同一チェックは同時に 2 つ以上走らない(§7.2, §8.2) |
| 最低周期 | `min_interval` 内の再要求では起動されない。前回結果が返る(§7) |

exectop が付与する環境変数:

| 変数 | 内容 |
|---|---|
| `EXECTOP=1` | exectop 配下で動いていることの目印 |
| `EXECTOP_VERSION` | exectop のバージョン |
| `EXECTOP_CHECK_ID` | チェック ID |
| `EXECTOP_MODE` | `run` / `watch`（実行単位のモード。CLI の単発実行は `oneshot`、旧 `run` も互換） |
| `EXECTOP_TRIGGER` | `schedule` / `request` / `manual` / `startup` |
| `EXECTOP_ITER` | このチェックの通算実行回数(1 始まり。daemon 再起動で復元される) |
| `EXECTOP_TIMEOUT` | タイムアウト秒(整数) |
| `EXECTOP_PREV_STATUS` | 前回の状態(初回は `NONE`) |
| `EXECTOP_PREV_TS` | 前回開始時刻の Unix 秒(初回は空) |
| `EXECTOP_STATE_DIR` | チェック専用の永続ディレクトリ(§4.6)。作成済み |

### 4.6 状態ディレクトリ

差分監視(前回値との比較など)のため、チェックごとに書き込み可能なディレクトリを提供する。

- 場所: `${XDG_STATE_HOME:-$HOME/.local/state}/exectop/<root-hash>/state/<チェック ID をエスケープしたもの>/`(`/` は `%2F`)
- exectop は作成(`0700`)と `EXECTOP_STATE_DIR` の提示のみ行い、内容には関知しない。
- `--state-dir` で起点を変更可。`--no-state` で無効化(変数も渡さない)。

### 4.7 サンプルチェック

```sh
#!/usr/bin/env bash
# exectop: name="Disk / usage" interval=30s timeout=5s tags=disk
set -eu
pct=$(df -P / | awk 'NR==2 {gsub("%","",$5); print $5}')
echo "/ used ${pct}%"
if   [ "$pct" -ge 95 ]; then exit 1   # FAIL
elif [ "$pct" -ge 85 ]; then exit 2   # WARN
fi
exit 0
```

```sh
#!/usr/bin/env bash
# exectop: name="Nightly backup age" min_interval=10m min_interval_fail=1m timeout=30s tags=ops
# 重い集計。10 分以内の再要求には前回結果を返す。失敗時だけ 1 分で再試行できる。
set -eu
age=$(( $(date +%s) - $(stat -c %Y /backup/latest.tar) ))
echo "backup age ${age}s"
[ "$age" -lt 90000 ] || exit 1
```

---

## 5. 設定と CLI

### 5.1 CLI

端末向けのカラー表示は ANSI 16 色を前提とし、色名や true color には依存しない。状態マーカー、エラー表示、凡例を含む端末出力は ASCII のみとする。色覚多様性に配慮し、状態は ASCII マーカーと状態名でも判別できるようにする。`FAIL`、`TIMEOUT`、`ERROR`、`UNKNOWN` は赤系またはマゼンタ系の強調に加えて太字・反転などの形状差を使い、特にエラー行を目立たせる。

```
exectop             [flags]                   `watch` の省略形。継続実行して結果を更新する
exectop oneshot     [flags] [--root DIR]     全件を一度だけ実行して結果を返す
exectop run     [flags] [--root DIR]     `oneshot` の互換エイリアス
exectop watch   [flags] [--root DIR]     継続実行(TTY なら TUI、非TTY なら周期テキスト出力)
exectop top     [flags] [--root DIR]     watch のエイリアス
exectop list    [--root DIR]             発見結果と解決済みメタデータを表示(実行しない)
exectop validate [--root DIR] [CHECK_ID...] 設定と宣言依存性を検証し、依存性一覧も表示(実行しない)
exectop check   <ID>... [flags]          指定チェックのみ実行(デバッグ用。--force を暗黙に含む)
exectop examples [--root DIR]            配布済みのサンプルチェック一式をインストール
exectop generate --root DIR PATH         新しいチェックの UNKNOWN 雛形を生成(既存ファイルは上書きしない)
exectop archive create FILE [--root DIR]                  チェックセットをgzip tarで保存
exectop archive extract SOURCE [--target DIR] [--mode M]  チェックセットを展開
exectop extract SOURCE [--target DIR] [--mode M]          archive extract の短縮形
exectop history <ID> [--limit N] [--since D]   実行履歴を表示
exectop daemon  status|stop|restart|run [--root DIR]   daemon の操作(run は通常クライアントが暗黙に呼ぶ)
exectop db      vacuum|prune|path [--root DIR]         データベース保守
exectop bench   gen|run|compare|all [flags]             ベンチマーク(§18)
```

`archive create` はローカルの `--root` 配下を gzip 圧縮 tar にまとめる。`archive extract` はローカルパスまたは `file://`、`http://`、`https://`、`ftp://` の SOURCE を受け付け、展開先は `--target`（省略時 `./exectop.d`）とする。アーカイブの内容は相対パスの通常ファイルとディレクトリだけで、絶対パス、`..` による逸脱、シンボリックリンク、その他の特殊ファイルは拒否する。

アーカイブには `exectop.d/.archive.data` 相当のメタデータを含める。形式は `exectop.d/v1` とし、ファイルごとの相対パス、モード、SHA-256、アーカイブID、作成時刻を記録する。展開後は展開先直下の `.archive.data` として保存する。

展開更新モードは `--mode refuse`（既存展開先があれば拒否、既定）、`--mode replace`（展開先を完全置換）、`--mode merge`（前回メタデータと現在のローカルSHA-256を比較し、前回のSHA-256と一致する未変更ファイルだけ更新。変更済みファイルは保持）とする。前回メタデータがない、壊れている、または対象ファイルが記録されていない場合、既存ファイルはローカル変更として保持し、新規ファイルだけを追加する。

共通フラグ:

| フラグ | 既定 | 意味 |
|---|---|---|
| `--root DIR` | `./exectop.d`(存在時)、それ以外は `./checks` | 起点ディレクトリ |
| `--tree` | false | watch TUI でディレクトリ行を使った階層表示 |
| `--config FILE` | `<root>/exectop.toml` | グローバル設定 |
| `-j, --concurrency N` | `min(32, 4*NumCPU)` | 同時実行数上限(daemon 起動時に確定。変更は `daemon restart`) |
| `--timeout D` | 設定値 | 全チェックのタイムアウト上書き |
| `--interval D` | 設定値 | 全チェックの間隔上書き(watch) |
| `--tag T`, `--group G`, `--match REGEX` | | 実行対象のフィルタ |
| `-o, --output table\|json\|jsonl` | TTY: table / 非TTY: jsonl(watch) | 出力形式 |
| `--fail-on ok\|warn\|fail` | `fail` | 集約終了コードを非 0 にする閾値 |
| `--force` | false | `min_interval` を無視して実行する(実行中なら join) |
| `--max-age D` | なし | cached 結果の age がこれを超えていたら UNKNOWN として集約する |
| `--inherit-env` | false | 環境変数を全継承 |
| `--follow-symlinks` | false | root 外へのシンボリックリンクを許可 |
| `--allow-unsafe` | false | §14.1 の権限検査を警告のみにする |
| `--splay D` | watch: `interval` の 10% | 初回投入をランダムに遅延させ集中を避ける |
| `--rescan D` | 0 | 再走査間隔 |
| `--history N` | 60 | メモリ上の履歴リング件数/チェック |
| `--no-daemon` | false | 単独モード(§8.6) |
| `--db PATH` | `$XDG_STATE_HOME/exectop/<root-hash>/exectop.db` | DB パス(単独モードでは `--db` を付けたときだけ永続化) |
| `-v, --verbose` | | run で詳細出力も表示 |

### 5.2 グローバル設定 `exectop.toml`

```toml
[global]
concurrency   = 16
timeout       = "10s"
interval      = "10s"
output_limit  = "64KiB"
history       = 60
inherit_env   = false

[defaults]        # §4.2 の [defaults] と同じキー。root 直下の .exectop.toml より弱い
grace = "2s"

[daemon]
background = false      # true: 購読者がいなくても Scheduler を常時稼働
idle_exit  = "0"        # "0" = 無期限。"24h" 等で無活動時に自ら終了

[persist]
queue_size      = 4096
batch_size      = 200
batch_interval  = "200ms"
block           = false  # true: キュー満杯時にドロップせずブロック(監査用途)
compress_over   = "4KiB" # 出力 BLOB をこのサイズ超で gzip
prune_interval  = "1h"

[persist.retention]
output      = "24h"      # run_output
output_min  = 20         # ただし各チェック最新 N 件は残す
runs        = "168h"     # 7 日
runs_max    = 1000       # 各チェック最大件数
transitions = "2160h"    # 90 日
events      = "720h"     # 30 日
```

---

## 6. 実行エンジン(daemon 内。単独モードではクライアント内)

### 6.1 Discover

```go
type Found struct {
    ID    string      // root からの相対パス
    Path  string      // 絶対パス
    Group string      // 最初のパス要素(root 直下なら "")
    Info  fs.FileInfo
    Owner uint32      // uid
    Mode  fs.FileMode
}

func Discover(root string, opt DiscoverOptions) ([]Found, []Warning, error)
```

- `filepath.WalkDir` を使う。ディレクトリ内は名前順に訪問される。
- 除外規則(§4.1)と権限検査(§14.1)をここで適用し、除外理由は `Warning` として返す(`list` で確認できる)。
- 1000 ファイル程度で 50ms 以内を目標。ヘッダ読み取りは Spec 段階で行う(Discover は stat のみ)。

### 6.2 Spec

```go
type Spec struct {
    ID, Name, Description         string
    Path, Cwd                     string
    Args, Env                     []string
    Interval, MinInterval         time.Duration
    MinIntervalFail               time.Duration
    Timeout, Grace                time.Duration
    OK, Warn, Unknown             []int
    Tags                          []string
    RunOnce                       bool
    Nice                          int
    OutputLimit                   int64
    Hash                          string   // 正規化 JSON の SHA-256。変更検知用
}

func Resolve(f Found, chain []DirConfig, global GlobalConfig, cli Overrides) (Spec, error)
```

- ヘッダ解析は先頭 4KiB を読んで行う。NUL を含めばバイナリとみなしスキップ。
- 解析エラー(不正な duration 等)は **そのチェックを ERROR 状態で登録**し、他のチェックの実行は続ける。
- `Interval < MinInterval` なら `Interval = MinInterval` に切り上げ、Warning を出す。

### 6.3 Runner(1 実行のプロセス制御)

```go
type Result struct {
    ID         string
    Iter       int
    Trigger    Trigger    // schedule | request | manual | startup
    Status     Status
    ExitCode   int        // -1 = 取得不能
    Signal     string     // シグナル終了時
    Reason     string     // ERROR/TIMEOUT の理由
    Start, End time.Time
    Duration   time.Duration
    Summary    string     // stdout 1 行目(サニタイズ済み)
    Stdout     []byte     // 上限まで
    Stderr     []byte
    Truncated  bool
    Metrics    map[string]float64
    SpecHash   string
}

type Runner interface {
    Run(ctx context.Context, s Spec, meta RunMeta) Result
}
```

実装要件:

1. `exec.CommandContext(ctx, s.Path, s.Args...)` を使う。**`sh -c` は使わない**。
2. `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` で独立プロセスグループにする。
3. `cmd.Cancel` を設定し、`syscall.Kill(-pid, SIGTERM)` を送る。`cmd.WaitDelay = s.Grace` を設定し、猶予後に Go が SIGKILL を送る。ただし `WaitDelay` の SIGKILL は直接の子だけなので、`Wait` 後に **必ず** `syscall.Kill(-pid, SIGKILL)` を再度送り(ESRCH は無視)、孫プロセスを掃除する。
4. タイムアウトは `context.WithTimeout(ctx, s.Timeout)` で表現し、`ctx.Err() == context.DeadlineExceeded` なら Status を TIMEOUT にする。親コンテキスト(全体停止)のキャンセルは `Status = ERROR, Reason = "cancelled"`。
5. stdout/stderr は **上限付きバッファ**に接続する(`Write` は上限を超えた分を捨てて `n` は全量を返し、`Truncated` を立てる)。パイプを閉じずに残る孫プロセスに対しては `WaitDelay` がパイプの読み取りを打ち切る。
6. stdin は `nil`(Go は `/dev/null` を割り当てる)。
7. 環境変数は §4.5 の通り組み立てる。`os.Environ()` を無条件で渡さない。
8. `Nice != 0` のときは起動後に `syscall.Setpriority(PRIO_PGRP, pid, nice)` を呼ぶ(失敗は警告のみ)。
9. 起動エラー(`cmd.Start` の失敗)は Status=ERROR、Reason にエラー文字列。ただし対象パスが ENOENT なら Status=VANISHED とする。実行開始後に対象パスが消えた場合も VANISHED とする(タイムアウト/明示キャンセルを除く)。
10. 実行直前に対象ファイルの所有者・パーミッションを再検査する(§14.1。daemon は長寿命なので TOCTOU の窓が広い)。
11. Runner はステートレスで、複数 goroutine から同時に呼ばれる。

擬似コードは付録 B。

### 6.4 ワーカープール

- 固定サイズの semaphore(`golang.org/x/sync/semaphore`)で同時実行数を `concurrency` に制限する。
- Coordinator/Scheduler が「投入」する単位は `(Spec, RunMeta)`。取得した goroutine が Runner を呼び、結果を `results chan Result` に送る。
- goroutine はチェック数ぶん先に生成しない。投入ごとに `go func()` し、semaphore で待たせる。

### 6.5 MemStore

```go
type Entry struct {
    Spec        Spec
    Latest      *Result             // 出力本体を持つのはここだけ
    Restored    bool                // daemon 起動時に DB から復元した結果か
    History     ring.Ring[HistoryPoint] // {Start, Status, Duration, ExitCode, Summary}
    Running     bool
    RunStart    time.Time
    Waiters     []chan<- Served     // join 待ちの要求(§7.2)
    Iter        int
    NextRun     time.Time
    Overruns    int                 // 前回が終わらず投入をスキップした回数
    Streak      int                 // 現状態が連続している回数
    StreakSince time.Time
}

type Store interface {
    Upsert(spec Spec)
    Remove(id string)
    MarkRunning(id string, at time.Time)
    Apply(r Result) (transition *Transition)
    Snapshot() []EntryView        // UI 向けのコピー。ロックを長く持たない
    Get(id string) (Entry, bool)
    Restore(rows []LatestRow, hist map[string][]HistoryPoint)
}
```

- **履歴リングには出力本体を入れない**。出力(最大 64KiB×2)を持つのは `Latest` だけ。
- `Snapshot()` は UI の描画周期(100ms 程度)で呼ばれる。ロック中はコピーのみ行う。
- **鮮度判定(§7)の正はこの MemStore**。SQLite の書き込み遅延は判定に影響しない。

---

## 7. 鮮度ポリシー(最低実行周期)

### 7.1 要件の再定式化

「最低実行周期を下回るタイミングで起動された場合は前回結果を返す」を、複数の独立した exectop 呼び出し(cron・複数端末)の間で一貫させるには、(1) 各チェックの最終開始時刻と最新結果、(2) 実行中フラグ、が単一の判断主体に集まっている必要がある。これが daemon が実行を所有する理由である。

### 7.2 判定規則

要求時刻 `t`、前回の開始時刻 `s`(**開始基準**)、前回状態 `st`、実行中フラグ `running` に対して:

```
effective(st) = MinIntervalFail  if st ∈ {FAIL, TIMEOUT, ERROR, UNKNOWN}
              = MinInterval      otherwise

if running:                        → join   (完了を待ち、その結果を全待機者に返す)
elif force:                        → 実行
elif no previous result:           → 実行
elif t - s < effective(st):        → cached (前回結果に age を付けて返す)
else:                              → 実行
```

- 開始基準にする理由: 終了基準だと実行時間の長いチェックほど実効周期が伸び、周期の予測が立たない。
- `join` は同一チェックへの同時要求を 1 回の実行に**合流**させる。cron と手動実行が重なっても二重実行しない。
- `cached` / `joined` で返す結果には `cached`, `joined`, `restored`, `age_ms`, `next_eligible_at` を付ける。表示では `LAST` 列に `12s ago (cached)` のように出す。
- watch の Scheduler(§8.2)も同じ関数を通す。Scheduler の投入は `force=false` の要求と等価。
- `MinInterval = 0` のチェックは常に「実行」または「join」になる。

### 7.3 daemon 再起動をまたぐ「前回結果」

daemon 起動時に `latest` テーブル(§9.2)から MemStore を復元する(§9.5)。これにより daemon が落ちて再起動した直後に長時間チェックが一斉に走ることを防ぐ。復元した結果には `restored: true` を付け、`min_interval` 判定にはそのまま使う。

---

## 8. モード

### 8.1 `run`

1. クライアントは daemon に接続(なければ自動起動、§11.2)し、`run{filter, force, max_age, wait:true}` を送る。
2. daemon は対象チェックごとに §7.2 の判定を行い、実行・cached・joined の結果を確定順にストリームで返す(投入順 = 発見順)。
3. クライアントは全件受信後に Renderer へ渡す。cached の結果も表示・集約に含める(古いが有効な判定として扱う)。`--max-age` を超えた cached は UNKNOWN として集約する。
4. 集約終了コード:

| 条件 | 終了コード |
|---|---|
| 全て OK(SKIP は無視) | 0 |
| FAIL / TIMEOUT / ERROR / VANISHED が 1 件以上 | 1 |
| 上記なし、WARN が 1 件以上 | 2 |
| 上記なし、UNKNOWN が 1 件以上 | 3 |
| exectop 自体の異常(root が無い、daemon を起動できない等) | 64 以上(`sysexits` 準拠。daemon 起動失敗は 70) |

`--fail-on warn` なら WARN でも 1、`--fail-on ok` なら常に 0(表示専用)。

5. クライアントが SIGINT/SIGTERM を受けたら要求をキャンセルし、受信済みだけを出力して 130 で終了する。**daemon 側の実行は止めない**(他の待機者がいる可能性があるため。待機者がいなければ完了後に結果を保存して捨てる)。

### 8.2 `watch`

daemon 内 Scheduler の動作:

- 各チェックの `NextRun` をキーにした min-heap を持つ。初回 `NextRun = now + rand(0, splay)`。
- ループ: heap 先頭が `now` 以下なら取り出し、§7.2 の判定に通す:
  - `running` なら投入せず `Overruns++`、`NextRun += Interval` して戻す(**同一チェックの多重起動をしない**)。
  - `cached` になる場合(手動実行の直後など)も投入せず `NextRun = s + Interval`。
  - でなければ `MarkRunning` してプールへ投入し、`NextRun = Start + Interval` で戻す(開始時刻基準)。
  - `Interval < Duration` が続く場合に投入が無限に詰まらないよう、完了時に `NextRun = max(NextRun, End + 100ms)` に補正する。
- `RunOnce` のチェックは初回投入後 heap から外す。
- semaphore が満杯のときは投入 goroutine が待つだけで、Scheduler ループは止めない。
- Scheduler の稼働条件: 既定は **購読者(watch クライアント)が 1 つ以上いる間だけ**。`daemon.background = true` で常時稼働。
- `--rescan` が有効なら、間隔ごとに Discover を再実行し、MemStore と `checks` テーブルに反映する。実行中のチェックが消えた場合は完了後に削除する。

クライアント側:

- `subscribe{filter, snapshot:true}` を送り、初回スナップショットと以後のイベントを受け取る。
- TTY: TUI(§12)。
- 非 TTY(`-o jsonl` 既定): 結果イベントが届くたびに 1 行 JSON を出力する。`-o table` なら `--refresh D`(既定 2s)ごとに表全体を出力する。

### 8.3 `list`

daemon に `list` を送り、発見結果・解決済み Spec・除外理由(Warning)を表示する。

### 8.4 `check <ID>...`

`run` の対象を指定 ID に限定し `force=true` で実行する。詳細出力を既定で表示する。

### 8.5 `history <ID>`

daemon に `history` を送り、直近の実行(状態・所要・終了コード・サマリ)と状態遷移を表示する。

### 8.6 単独モード `--no-daemon`

- daemon を使わず、クライアントプロセス内で Discover〜Runner を動かす。Phase 1 の完成形であり、daemon を置けない環境・CI・デバッグ用の退避経路。
- `--db PATH` を付けると、同じ Persist writer をプロセス内で使って書き込む。鮮度判定は付録 C の lease 方式で行う。
- 同じ DB を daemon と単独モードから同時に使うことは禁止し、ロックファイル(§11.1)で検出して拒否する。

---

## 9. データストア(SQLite)

### 9.1 配置と接続設定

- パス: `$XDG_STATE_HOME/exectop/<root-hash>/exectop.db`(`$XDG_STATE_HOME` 未設定なら `~/.local/state`)
- ドライバ: `modernc.org/sqlite`(cgo 不要、単一バイナリを維持)。`database/sql` 経由で使い、`mattn/go-sqlite3` に差し替え可能にしておく。
- 接続時 PRAGMA:

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;      -- WAL では電源断でも DB は壊れない。直近コミットの欠落のみ許容
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA auto_vacuum  = INCREMENTAL;
PRAGMA temp_store   = MEMORY;
```

- writer 用と reader 用に `*sql.DB` を分ける。writer は `SetMaxOpenConns(1)`。reader は複数可(WAL は読者を止めない)。
- DB ファイルとディレクトリは `0600` / `0700`。
- 起動時に `PRAGMA quick_check` を実行し、失敗したら DB を `exectop.db.corrupt-<ts>` に退避して新規作成する(ログと `daemon_events` に記録)。

### 9.2 スキーマ

```sql
-- スキーマ版。マイグレーションは version の単調増加で管理する
CREATE TABLE schema_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);  -- ('schema_version','1'), ('root','/abs/path'), ('created_at', ns)

-- チェックの台帳。ID は root からの相対パス。削除は deleted_at で論理削除
CREATE TABLE checks (
  check_id    TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  grp         TEXT NOT NULL DEFAULT '',
  path        TEXT NOT NULL,
  spec_json   TEXT NOT NULL,          -- 解決済み Spec のスナップショット
  spec_hash   TEXT NOT NULL,          -- spec_json の SHA-256。変更検知用
  first_seen  INTEGER NOT NULL,       -- unix ns (UTC)
  last_seen   INTEGER NOT NULL,
  deleted_at  INTEGER                 -- NULL = 現存
);

-- 実行履歴。1 実行 = 1 行。cached/joined の応答は行を作らない
CREATE TABLE runs (
  run_id       INTEGER PRIMARY KEY,   -- rowid。時系列に単調
  check_id     TEXT NOT NULL REFERENCES checks(check_id),
  iter         INTEGER NOT NULL,
  trigger      TEXT NOT NULL,         -- 'schedule' | 'request' | 'manual' | 'startup'
  status       TEXT NOT NULL,         -- OK|WARN|UNKNOWN|FAIL|TIMEOUT|ERROR|VANISHED
  exit_code    INTEGER,               -- NULL = 取得不能
  signal       TEXT,
  reason       TEXT,
  started_at   INTEGER NOT NULL,      -- unix ns
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL,
  summary      TEXT NOT NULL DEFAULT '',
  truncated    INTEGER NOT NULL DEFAULT 0,
  spec_hash    TEXT NOT NULL          -- 実行時点の Spec
);
CREATE INDEX runs_check_started ON runs(check_id, started_at DESC);
CREATE INDEX runs_started       ON runs(started_at);
CREATE INDEX runs_status        ON runs(status, started_at DESC);

-- 出力本体。保持期間を runs より短くできるよう分離
CREATE TABLE run_output (
  run_id   INTEGER PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,
  stdout   BLOB,
  stderr   BLOB,
  encoding TEXT NOT NULL DEFAULT 'raw' -- 'raw' | 'gzip'
);

-- 構造化出力の数値
CREATE TABLE run_metrics (
  run_id  INTEGER NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  key     TEXT    NOT NULL,
  value   REAL    NOT NULL,
  PRIMARY KEY (run_id, key)
);

-- 最新結果のホットテーブル。daemon 起動時の復元と単独モードの lease に使う
CREATE TABLE latest (
  check_id     TEXT PRIMARY KEY REFERENCES checks(check_id),
  run_id       INTEGER NOT NULL REFERENCES runs(run_id),
  status       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER NOT NULL,
  summary      TEXT NOT NULL DEFAULT '',
  streak       INTEGER NOT NULL DEFAULT 1,   -- 現状態の連続回数
  streak_since INTEGER NOT NULL,             -- 現状態が始まった run の started_at
  iter         INTEGER NOT NULL,
  lease_until  INTEGER                       -- 単独モードのみ使用(付録 C)。daemon 使用時は NULL
);

-- 状態遷移のみを抜いた表。「いつ FAIL になったか」を履歴全走査なしで答える
CREATE TABLE transitions (
  id           INTEGER PRIMARY KEY,
  check_id     TEXT NOT NULL REFERENCES checks(check_id),
  run_id       INTEGER NOT NULL REFERENCES runs(run_id),
  from_status  TEXT,                 -- NULL = 初回
  to_status    TEXT NOT NULL,
  at           INTEGER NOT NULL
);
CREATE INDEX transitions_check_at ON transitions(check_id, at DESC);

-- daemon 自身の出来事。障害調査用
CREATE TABLE daemon_events (
  id      INTEGER PRIMARY KEY,
  at      INTEGER NOT NULL,
  kind    TEXT NOT NULL,             -- start|stop|rescan|prune|queue_drop|db_error|corrupt_recovered
  detail  TEXT
);
```

設計上の判断:

| 論点 | 判断 |
|---|---|
| 時刻の型 | unix ナノ秒 INTEGER(UTC)。文字列より小さく、比較が速く、Go の `time.Time.UnixNano()` と往復が単純 |
| 出力の分離 | `run_output` を別テーブルにし、保持期間を短くできるようにする。`runs` を細く保つと履歴クエリが速い |
| cached/joined の記録 | 行を作らない。監査が要る場合は `daemon_events` に `kind='served_cached'` を任意で記録(既定 OFF) |
| `latest` の冗長性 | `runs` から導出可能だが、復元と鮮度判定を 1 クエリで済ませるため保持する。writer が同一 tx で更新するので不整合は生じない |
| status の型 | TEXT。数値化して得るサイズ差は小さく、`sqlite3` CLI で読める利点が勝る |

### 9.3 Writer(キュー → SQLite)

SQLite の書き込みは想定規模(500 チェック × 10s → 50 行/s)ではスループット上のボトルネックにならない(WAL + バッチで毎秒数千〜数万行が公知の水準)。キューの目的は **fsync のコミット遅延(仮想ディスク・NFS・低速 SD で数十〜数百 ms)を実行経路から外すこと**であり、結果は DB 書き込みを待たずに MemStore とクライアントに届く。

```go
type persistItem struct {
    kind   itemKind          // resultItem | checkUpsert | checkDelete | daemonEvent
    result *Result
    ...
}

type Writer struct {
    q        chan persistItem   // 容量 = persist.queue_size
    db       *sql.DB
    batchN   int                // persist.batch_size
    batchT   time.Duration      // persist.batch_interval
    dropped  atomic.Int64
}
```

動作:

1. Coordinator は結果を **まず MemStore に反映し、購読者と join 待機者に配信し、その後** `q` に非ブロッキング送信する(`select { case q <- it: default: dropped++ }`)。`persist.block=true` ならブロッキング送信。
2. writer goroutine は `q` から取り出し、`batchN` 件溜まるか `batchT` 経過で 1 トランザクションにまとめて書く:
   - `INSERT INTO runs` → `last_insert_rowid()` → `INSERT INTO run_output / run_metrics`(出力は `compress_over` 超で gzip)
   - `latest` を `INSERT ... ON CONFLICT(check_id) DO UPDATE`(streak は前値と比較して更新)
   - 状態が変わっていれば `INSERT INTO transitions`
3. コミット失敗(ディスクフル等)はリトライ 3 回、以後はそのバッチを捨てて `daemon_events` に記録を試み、ログに出す。**実行は止めない**。
4. 停止時(SIGTERM/`shutdown` RPC)は `q` を drain してから DB を閉じる(上限 5s)。
5. `dropped > 0` になったら watch 画面のヘッダに `persist: N dropped` を出す。

背圧の方針: **実行結果を捨ててでも実行は止めない**。監視ツールとして、履歴の欠落より監視停止の方が害が大きい。監査記録が必要な用途は `persist.block = true`。

### 9.4 保持と整理(prune)

writer goroutine が `persist.prune_interval` ごとに実行する。既定値は §5.2 `[persist.retention]`。

- `run_output`: 期間経過分を削除。ただし各チェック最新 `output_min` 件は残す。
- `runs`, `run_metrics`: 期間経過分、または各チェック `runs_max` 件超過分を削除。
- `transitions`, `daemon_events`: 期間経過分を削除。
- `checks` の論理削除行: 30 日後に物理削除(`runs` が残っていれば残す)。
- 削除は `DELETE ... WHERE rowid IN (SELECT ... LIMIT 1000)` を繰り返し、1 tx を短く保つ。
- 削除後に `PRAGMA incremental_vacuum(256)`。フル VACUUM は `exectop db vacuum` で明示的に。
- 想定サイズ: `runs_max=1000` が効いて 500 チェックで 50 万行、概ね 100〜150MB。

### 9.5 復元(daemon 起動時)

```sql
SELECT l.check_id, l.run_id, l.status, l.started_at, l.ended_at, l.summary,
       l.streak, l.streak_since, l.iter, r.exit_code, r.reason, r.trigger,
       o.stdout, o.stderr, o.encoding
FROM latest l JOIN runs r USING(run_id) LEFT JOIN run_output o USING(run_id)
WHERE l.check_id IN (<現在の Discover 結果>);
```

- Discover で消えたチェックは復元しない(`checks.deleted_at` を立てる)。
- Spec のハッシュが変わっていても最新結果は復元する(結果は有効、Spec は更新)。`min_interval` が変わっていれば新しい値で判定する。
- 履歴リング(メモリ)も直近 N 件を `runs` から復元する(status/duration/summary のみ)。
- `lease_until` は daemon 起動時に全行 NULL に戻す。

### 9.6 履歴クエリ(reader)

```sql
-- あるチェックの直近 N 件
SELECT run_id, status, exit_code, started_at, duration_ms, summary
FROM runs WHERE check_id = ? ORDER BY started_at DESC LIMIT ?;

-- 期間内の状態遷移
SELECT check_id, from_status, to_status, at
FROM transitions WHERE at BETWEEN ? AND ? ORDER BY at;

-- 期間内の失敗率
SELECT check_id,
       SUM(status IN ('FAIL','TIMEOUT','ERROR')) * 1.0 / COUNT(*) AS fail_ratio,
       AVG(duration_ms), MAX(duration_ms)
FROM runs WHERE started_at >= ? GROUP BY check_id;
```

---

## 10. RPC プロトコル

### 10.1 トランスポート

- UNIX ドメインソケット、`SOCK_STREAM`。
- パス: `$XDG_RUNTIME_DIR/exectop/<root-hash>.sock`。`$XDG_RUNTIME_DIR` が無ければ `/tmp/exectop-<uid>/<root-hash>.sock`(ディレクトリ `0700`)。
- メッセージ: 改行区切り JSON(NDJSON)。1 行 = 1 メッセージ。1 行の上限 16MiB。
- 要求と応答は `id` で対応付ける。ストリーム応答(`run`, `subscribe`)は同じ `id` で複数行を返し、`final: true` で終わる。
- 選定理由: 依存なし、`socat`/`nc -U` で手動デバッグ可能、Go の `encoding/json` + `bufio.Scanner` で足りる。gRPC は依存と生成物が増える割に利点が薄い。

### 10.2 メッセージ形式

```json
{"id": 1, "method": "run", "params": {...}}
{"id": 1, "result": {...}, "final": false}
{"id": 1, "result": {...}, "final": true}
{"id": 1, "error": {"code": "unknown_check", "message": "..."}}
```

### 10.3 メソッド

| method | params | result | 備考 |
|---|---|---|---|
| `hello` | `{client_version, proto_version, exe_mtime}` | `{daemon_version, proto_version, root, pid, started_at, db_path, exe_mtime}` | 接続直後に必須。不一致は §11.4 |
| `status` | `{metrics: bool}` | 集計、実行中数、キュー深さ、dropped、uptime、scheduler 稼働有無。`metrics=true` で §18.5 のヒストグラムを含む | ヘッダ表示用 / bench 用 |
| `list` | `{filter}` | チェック一覧、解決済み Spec、Warning | |
| `run` | `{filter, force, max_age, wait: bool}` | `wait=true`: 結果(Served)を確定順にストリーム。`wait=false`: 投入した ID 一覧のみ | §7.2 の判定を適用 |
| `subscribe` | `{filter, snapshot: bool}` | 初回に全件スナップショット、以後 `result` / `state`(running 開始) / `rescan` イベントをストリーム | watch の実体。購読中は Scheduler 稼働条件を満たす |
| `history` | `{check_id, limit, since, until}` | §9.6 | |
| `transitions` | `{check_id?, since, until}` | | |
| `rescan` | | 追加/削除/更新の ID | |
| `pause` / `resume` | | | Scheduler の停止/再開 |
| `reload` | | | グローバル設定と Spec の再読込 |
| `shutdown` | `{drain: bool}` | | 実行中を待つ/kill してから終了。**全バージョンで必ず実装する** |

`filter` は §5.1 の `--tag/--group/--match` と同じ構造。RPC は **チェック ID とフィルタしか受け取らない**。任意コマンドの実行や Spec の上書きは受け付けない(§14.5)。

Served の形:

```go
type Served struct {
    Result       Result
    Cached       bool
    Joined       bool
    Restored     bool
    Age          time.Duration
    NextEligible time.Time
}
```

---

## 11. daemon のライフサイクル

### 11.1 単一性

- ロックファイル `$XDG_STATE_HOME/exectop/<root-hash>/daemon.lock` に `flock(LOCK_EX|LOCK_NB)`。取れなければ他 daemon(または単独モード `--db` のプロセス)が生きている。
- ロックが取れたのにソケットファイルが残っていれば stale として削除してから listen する。
- pid、バージョン、開始時刻、モード(daemon / standalone)をロックファイルに書く(診断用。判定には flock のみを使う)。
- 加えて実行中チェックの pgid 一覧を `daemon.running`(同ディレクトリ)に都度書く。daemon が `kill -9` で死ぬと子プロセスグループは残るため、次に起動した daemon は起動時にこのファイルを読み、残っている pgid に SIGKILL を送ってから空にする(§19.6 F1)。

### 11.2 自動起動(クライアント側 Launcher)

```
1. ソケットに connect
2. ENOENT / ECONNREFUSED → `os.Executable()` を `exectop daemon run --root <正規パス>` で起動
      - setsid で新セッション、stdin/stdout/stderr は /dev/null とログファイルへ
      - 環境変数は最小継承(§4.5 と同じ規則)
      - `os.Executable()` の実体が他者書込可能なら起動を拒否(§14.5)
3. ソケットが現れるまで 20ms 間隔でポーリング(上限 3s)
4. 再 connect。失敗したら「daemon を起動できない」旨とログパスを出して exit 70 (EX_SOFTWARE)
5. `--no-daemon` 指定時は 1〜4 を飛ばして単独実行
```

同時に複数クライアントが起動しようとしても、flock で 1 つだけが生き残り、残りは終了する。

### 11.3 寿命

- 既定では **クライアントがいなくても終了しない**(OS 起動後の初回起動以降、暗黙に保持)。
- `daemon.idle_exit` を設定すると、購読者ゼロかつ要求なしがその期間続いたら自ら終了する(次回要求で再起動される。DB があるので前回結果は失わない)。
- root ディレクトリが消えた(Discover が ENOENT)場合は 1 分後に終了する。
- 明示操作: `exectop daemon stop|status|restart [--root]`。
- Scheduler の稼働条件は §8.2。
- OS 再起動後の自動起動は行わない。

### 11.4 バージョン整合

- `hello` で `proto_version` と `exe_mtime` を比較する。
- クライアントの方が新しい(proto 非互換、または `exe_mtime` が新しい)場合:
  1. daemon に `shutdown{drain:true}` を送る
  2. 新しいバイナリで再起動(§11.2)
- daemon の方が新しい場合はクライアントがエラーで止まる(古いクライアントを更新するよう案内)。

### 11.5 シグナル

- `SIGTERM` / `SIGINT`: 実行中を `grace` まで待ち、writer を drain し、ソケットとロックを片付けて終了。
- `SIGHUP`: `reload` と同じ。

### 11.6 ログ

- `$XDG_STATE_HOME/exectop/<root-hash>/daemon.log`。サイズ 10MiB × 3 世代でローテート。
- 形式は 1 行 JSON(`log/slog` の JSON ハンドラ)。

---

## 12. TUI

- ライブラリ: `github.com/charmbracelet/bubbletea` + `lipgloss`。
- クライアントは `subscribe` で受けたスナップショットとイベントをローカルの表示用モデルに反映し、100ms 周期(またはイベント到着時)に描画する。描画は実行と完全に非同期。

### 12.1 一覧画面

root では、root 直下のチェックは個別行、サブディレクトリはディレクトリ行として表示する。
ディレクトリ行の状態は配下を再帰的に集約し、FAIL/TIMEOUT/ERROR/UNKNOWN が1件でもあれば
`FAIL`、それ以外で WARN があれば `WARN`、それ以外は実行中・未実行・SKIP・OK の順に決める。
集約行には総数と状態別件数を表示する。ディレクトリ内では直下のチェックと子ディレクトリを
表示する。root 以外ではヘッダ直下に仮想行 `..` を表示し、Enter で親ディレクトリへ移動する。
既定では root 配下の全チェックを個別行でフラットに表示する。`--tree` または TUI の `T`
キーを使うと、ディレクトリ行による集約と移動を有効にできる。この切り替えは表示だけに
影響し、発見・実行対象は変えない。

```
exectop  root=./checks  checks=42  running=3  OK 36  WARN 2  FAIL 3  TIMEOUT 1   conc 5/16   persist q=3   uptime 00:12:31
 ST    NAME                   GROUP   LAST              DUR    INT   HIST(20)              SUMMARY
 FAIL  Disk / usage           disk    3s ago            12ms   30s   ....xxxx.......xxxxx  / used 96%
 WARN  TCP 443 reachable      net     1s ago            210ms  10s   ....................  443 open (slow)
 OK    Nightly backup age     ops     4m ago (cached)   3.1s   10m   ....................  backup age 41000s
 RUN*  slow probe             ops     -                 (4.2s) 60s   ...........x........  (running)
 OK    examples/              directory -              -      -     -                     9 checks: OK=9
```

- 既定ソート: 状態の重篤度(FAIL/TIMEOUT/ERROR > WARN > UNKNOWN > RUNNING > OK > SKIP)→ 名前。
- HIST は履歴の状態を 1 文字ずつ表示(`.`=OK `w`=WARN `x`=FAIL/TIMEOUT/ERROR `?`=UNKNOWN)。
- SUMMARY は端末幅に合わせて切り詰め。
- 各チェック行の SUMMARY には次回実行(`next=`)、連続状態回数(`streak=`)、履歴の平均実行時間(`avg=`)を含める。ディレクトリ行には状態別の内訳を表示する。
- 状態変化した行は数秒間ハイライトする。
- `--rescan` でチェック本体または設定の変更を検出した場合、直近結果が変更前のものなら状態末尾に `*` を付ける(`FAIL*` など)。詳細画面には変更時刻と stale 表示を出し、次回実行完了時にマーカーを消す。変更検出だけでは自動再実行しない。
- 行選択中は行頭に `>` を表示し、行全体の反転表示は行わない。
- ソート対象として選択中のカラムはヘッダとセルだけを反転表示する。
- daemon との接続が切れたらヘッダに `DISCONNECTED` を出し、再接続(自動起動含む)を試みる。
- TTY での起動時および更新時は画面をクリアして再描画する。非TTY出力では画面制御文字を出さない。

### 12.2 キー操作

| キー | 動作 |
|---|---|
| `q` / `Ctrl-C` | `q` は確認後にクライアント終了、`Ctrl-C` は即時終了(daemon と実行は継続) |
| `↑↓` / `j k` | 行選択 |
| `←→` | ソート対象カラムを選択。`↑↓` を押すとカラム選択を解除して行選択に戻る |
| `s` / `n` / `g` / `l` / `d` / `i` / `t` / `m` | ST / NAME / GROUP / LAST / DUR / INT / HIST / SUMMARY を選択。ヘッダにも表示 |
| `Enter` | ディレクトリ行ならそのディレクトリへ移動、チェック行でカラム選択中ならソート、選択なしなら詳細画面 |
| `Esc` | ディレクトリ内では親へ移動、root ではカラム選択を解除 |
| `r` | 選択行を `force` で即時再実行(実行中なら join) |
| `R` | 全件を `force` で即時再実行 |
| `p` | 詳細画面ではスクリプトを `PAGER` で表示。それ以外では Scheduler 一時停止/再開(`pause`/`resume`) |
| `v` | 詳細画面 |
| `/` | 名前・サマリのインクリメンタル絞り込み |
| `f` | 状態フィルタ(異常のみ ⇄ 全件) |
| `h` | 履歴画面(`history` RPC) |
| `?` | ヘルプ |

### 12.3 詳細画面

Spec の解決結果(どの層から値が来たか)、最新結果(cached/joined/restored/age を含む)、stdout / stderr(スクロール可)、履歴表(開始時刻・状態・所要・終了コード・サマリ)、状態遷移。

---

## 13. 出力フォーマット

### 13.1 table(`run`)

```
ST(s)   NAME(n)              LAST(l)            DUR(d)  CODE  SUMMARY(m)  PATH
OK      memory               now                5ms     0     free 61%     memory
FAIL    Disk / usage         now                12ms    1     / used 96%   disk/usage.sh
OK      Nightly backup age   4m ago (cached)    3.1s    0     backup age   backup/age.sh
TIMEOUT slow probe           now                10.00s  -     killed       probe/slow.sh
----
42 checks: 36 ok, 2 warn, 3 fail, 1 timeout  (30 executed, 11 cached, 1 joined; wall 2.31s, sum 18.4s)
```

### 13.2 json / jsonl

1 結果のスキーマ(jsonl は 1 行 1 結果、json は `{"run": {...}, "results": [...]}`):

```json
{
  "id": "disk/usage.sh",
  "name": "Disk / usage",
  "group": "disk",
  "iter": 12,
  "trigger": "request",
  "status": "FAIL",
  "exit_code": 1,
  "signal": "",
  "reason": "",
  "start": "2026-09-02T10:00:00.123+09:00",
  "end":   "2026-09-02T10:00:00.135+09:00",
  "duration_ms": 12,
  "summary": "/ used 96%",
  "stdout": "/ used 96%\n",
  "stderr": "",
  "truncated": false,
  "metrics": {},
  "variables": {"owner": "platform", "latency_ms": 12},
  "tags": ["disk"],
  "cached": false,
  "joined": false,
  "restored": false,
  "age_ms": 0,
  "next_eligible_at": null
}
```

`json` の `run` にはツールバージョン、root、daemon pid、開始/終了時刻、集約カウント(executed/cached/joined)、集約終了コードを含める。

---

## 14. セキュリティと安全性

「ディレクトリ配下の実行ファイルを全部走らせる」設計は、そのディレクトリを**実行面**にする。加えて daemon は「ソケット経由の要求でコードを実行するプロセス」である。以下は必須。

### 14.1 権限検査(既定で拒否、`--allow-unsafe` で警告に緩和)

`run-parts` / `sudo` の sudoers 検査に倣う。Discover 時と **実行直前**の両方で行う。

- root ディレクトリおよび各中間ディレクトリが、他者(o+w)またはグループ(g+w)に書き込み可能なら拒否。
- チェックファイルが o+w / g+w なら拒否。
- チェックファイルの所有者が実行ユーザーでも root でもなければ拒否。
- root 外へのシンボリックリンクは既定で無視。
- exectop を root で実行しようとしたら警告(禁止はしない)。

### 14.2 起動方法

- シェル経由の起動をしない(引数のシェル解釈を排除)。
- 環境変数は最小継承(§4.5)。`LD_PRELOAD`, `LD_LIBRARY_PATH`, `BASH_ENV`, `ENV` 等の危険変数は `--inherit-env` でも除外する。

### 14.3 資源制御

- タイムアウト、出力上限、同時実行上限、プロセスグループ kill(いずれも必須)。
- v1 では rlimit を設定しない(Go の `SysProcAttr` に無く、`prlimit(2)` を起動直後に呼ぶ方法は競合がある)。
- Overruns の累積、TIMEOUT 連続は画面上で目立たせるが、自動で間隔を伸ばさない。

### 14.4 出力のサニタイズ

- 一覧・詳細に出す前に ESC (0x1B) と、`\t` `\n` 以外の C0 制御文字を除去する(端末エスケープ注入対策)。
- JSON 出力と DB 保存はエスケープのみ行い、内容は改変しない。

### 14.5 daemon とソケット

| 項目 | 内容 |
|---|---|
| ソケットの権限 | ディレクトリ `0700`、ソケット `0600` |
| peer credential | 接続受理時に `SO_PEERCRED` で uid を取り、daemon の uid と一致しなければ切断してログ |
| 要求内容 | RPC はチェック ID とフィルタしか受け取らない。任意コマンドの実行や Spec の上書きを受け付けない。Spec はファイルシステムからのみ読む |
| 自動起動するバイナリ | `os.Executable()` の実体。symlink 先が他者書込可能なら起動を拒否する |
| DB / ログ / 状態ディレクトリ | `0600` / `0700` |

---

## 15. 性能

### 15.1 目標

| 指標 | 目標 |
|---|---|
| 1000 ファイルの Discover + Spec 解決 | 100ms 以内 |
| チェック 1 本あたりの exectop 側オーバーヘッド(fork/exec/wait/収集) | 2ms 以内 |
| `run` の壁時計時間 | ≈ max(最長チェック, Σduration / concurrency) + 100ms(cached 分は 0) |
| RPC 往復(hello + run 投入) | 5ms 以内 |
| TUI の描画周期 | 100ms、500 チェックで描画 10ms 以内 |
| Persist writer のコミット | p99 50ms 以内(SSD)。超えてもキューで吸収し実行に影響しない |
| daemon のメモリ | 500 チェック・履歴 60 件で 100MB 以内 |

### 15.2 根拠と注意

- Go の `os/exec` は Linux で `clone(CLONE_VFORK|CLONE_VM)` を使うため、daemon のヒープが大きくても fork コストは小さい。
- **並列度は律速要因になりにくい**。壁時計時間はほぼ最長のチェックとタイムアウト設定で決まる。`run` のフッタに `wall` と `sum` を出すのはこの診断のため。
- 同時実行数の既定を `4*NumCPU`(上限 32)にしたのは、監視スクリプトが I/O 待ち中心という前提(P2)。CPU バウンドなチェックが多いなら `-j NumCPU` に下げる。
- SQLite については §9.3。キューは尾部遅延対策であり、スループット対策ではない。

---

## 16. Go パッケージ構成と主要インターフェース

```
exectop/
├── cmd/exectop/main.go          # サブコマンド分岐(stdlib flag)
├── internal/
│   ├── discover/               # §6.1
│   ├── spec/                   # §6.2  ヘッダ解析, toml, 優先順解決
│   ├── runner/                 # §6.3  プロセス制御(Linux 固有コードは runner_linux.go)
│   ├── pool/                   # §6.4
│   ├── memstore/               # §6.5
│   ├── coordinator/            # §7    鮮度判定, join, fan-out。Scheduler と pool を所有
│   ├── sched/                  # §8.2
│   ├── persist/                # §9    schema.sql を embed, migrate, Writer, Reader, prune
│   ├── rpc/                    # §10   NDJSON コーデック, ディスパッチ, client
│   ├── daemon/                 # §11   flock, listen, シグナル, idle_exit
│   ├── launcher/               # §11.2 クライアント側の自動起動と接続リトライ
│   ├── status/                 # Status 型, 重篤度, 集約
│   ├── render/                 # §13   table / json / jsonl
│   ├── tui/                    # §12   bubbletea モデル
│   ├── sanitize/               # §14.4
│   ├── safety/                 # §14.1, §14.5
│   ├── metrics/                # §18.5 カウンタ・ヒストグラム(常時有効、低コスト)
│   ├── bench/                  # §18   合成チェック生成、シナリオ実行、比較
│   └── fault/                  # §19.6 障害注入(build tag `fault` でのみ有効)
├── check-spec.md          # §4 をチェック作者向けに抜粋
├── bench/baseline/             # §18.7 ホスト別ベースライン JSON
├── scripts/soak.sh             # §19.5 耐久テスト起動スクリプト
└── testdata/checks/            # テスト用チェック群
```

依存ライブラリ(最小に保つ):

- `modernc.org/sqlite`
- `github.com/pelletier/go-toml/v2`
- `github.com/charmbracelet/bubbletea`, `lipgloss`
- `golang.org/x/sync`(semaphore)、`golang.org/x/term`(TTY 判定)
- CLI は標準 `flag` + 手書きサブコマンド。cobra は不要。

主要インターフェース:

```go
// status
type Status uint8
const (
    Pending Status = iota; Running; OK; Warn; Unknown; Fail; Timeout; Error; Skip
)
func (s Status) Severity() int
func Aggregate(rs []Served, failOn Status, maxAge time.Duration) (exitCode int)

// coordinator
type Freshness struct{ Force bool; MaxAge time.Duration }
func (c *Coordinator) Request(ctx context.Context, ids []string, f Freshness, trig Trigger) (<-chan Served, error)
func (c *Coordinator) Subscribe(ctx context.Context, filter Filter) (<-chan Event, Snapshot, error)

// persist
type Writer interface {
    Enqueue(item Item) (accepted bool)   // 非ブロッキング(persist.block=false 時)
    Close(ctx context.Context) error     // drain
    Stats() WriterStats                  // depth, dropped, last_commit_ms
}
type Reader interface {
    Latest(ctx context.Context, ids []string) (map[string]LatestRow, error)
    RecentHistory(ctx context.Context, ids []string, n int) (map[string][]HistoryPoint, error)
    History(ctx context.Context, id string, q HistoryQuery) ([]RunRow, error)
    Transitions(ctx context.Context, q TransitionQuery) ([]TransitionRow, error)
}
```

---

## 17. テスト戦略

| 対象 | 方法 |
|---|---|
| Discover | `testdata/` に `_lib/`, ドットファイル, 非実行ファイル, root 外シンボリックリンクを置き、除外と順序を検証 |
| Spec | ヘッダ解析のテーブルテスト(コメント記号の種類、クォート、複数行、不正値)。優先順解決。`interval < min_interval` の切り上げ |
| Runner: 終了コード | `exit N` するスクリプトで全マッピングを検証 |
| Runner: タイムアウト | `sh -c 'sleep 100 & sleep 100'` を timeout=200ms で実行し、**孫を含めて全滅**していることを `/proc` 走査または `kill -0` で確認。`trap '' TERM` する子で SIGKILL 経路も検証 |
| Runner: 出力上限 | 1MiB 出力で `Truncated`。stdout を継承した `sleep 100 &` で `WaitDelay` の効果 |
| Runner: 環境 | `env` を出力するチェックで最小継承と `EXECTOP_*`、危険変数の除外 |
| 鮮度判定 | 擬似クロックで §7.2 の全分岐。`min_interval_fail` の切替。開始基準であること |
| join | 同一チェックへ同時に 10 要求 → 実行回数 1、全要求が同じ `run_id` を受け取る |
| Scheduler | 擬似クロックで Overrun スキップ、開始時刻基準、RunOnce、splay、cached 時の非投入、購読者ゼロ時の停止 |
| MemStore | `-race` で並行 Apply/Snapshot |
| Writer | 1 万件を一気に投入して全件が DB に載る。キュー容量 8 で溢れさせ `dropped` が増え、実行が止まらない。`block=true` でブロックする |
| クラッシュ耐性 | writer のコミット中に `SIGKILL` → 再起動後 `quick_check` が ok、`latest` と `runs` が整合 |
| 復元 | 結果を書いて daemon を kill → 再起動 → `run` が cached を返し `restored:true` |
| 単一性 | 同時に 20 クライアントが起動 → daemon プロセスは 1 つ、全クライアントが接続成功 |
| stale ソケット | ソケットファイルだけ残して flock なし → 起動が成功する |
| peer 検査 | 別 uid からの接続が拒否される(CI では `unshare -U` またはスキップ) |
| バージョン不一致 | proto_version を偽った hello → 再起動経路が動く |
| prune | 保持期間を 1s にして prune 後の件数と `incremental_vacuum` を確認 |
| 単独モード | `--no-daemon --db` の lease 方式で 2 プロセス同時でも二重実行しない。daemon 稼働中の同一 DB 利用が拒否される |
| Render | golden ファイル(table/json)。cached/joined 表記 |
| 安全性 | o+w のファイル/ディレクトリを作って拒否を検証。実行直前の再検査 |
| 統合 | `exectop run --root testdata/checks -o json` の集約終了コードと件数。daemon あり/なし両方 |

CI では常に `go test -race ./...` と `go vet`、`staticcheck` を通す。

---

## 18. ベンチマーク機能

### 18.1 目的

- §15 の性能目標を、実装後に **同じ手順で再現可能に** 検証する。
- リリースごとに回帰を検知する(ベースライン比較)。
- 運用者が自分の環境(ディスク、チェック数、チェックの重さ)で目標が成立するかを確かめられるようにする。

ベンチマークは本体バイナリに同梱する(`exectop bench`)。別ツールにすると本体と乖離するため。

### 18.2 サブコマンド

```
exectop bench gen     --out DIR [--checks N] [--dist KIND] [--mean D] [--fail-ratio R] [--output-bytes B]
                       [--min-interval-ratio R] [--kind sh|bin]
exectop bench run     --root DIR --scenario NAME[,NAME...] [--repeat R] [--concurrency N]
                       [--no-daemon] [--json FILE] [--warmup R]
exectop bench compare --baseline FILE --current FILE [--tolerance PCT] [--fail-on-regression]
exectop bench all     [--out DIR] [--json FILE] [--baseline FILE]     # gen → 全シナリオ run → compare
```

`bench all` は一時 root を生成し、全シナリオを既定パラメータで実行し、JSON と表を出力する。CI ではこれを使う。

### 18.3 合成チェックの生成(`bench gen`)

生成するチェックは実運用のチェックを模した、パラメータ化された小さなスクリプトまたはバイナリ。

| パラメータ | 既定 | 意味 |
|---|---|---|
| `--checks` | 500 | チェック数。グループは `sqrt(N)` 個のサブディレクトリに均等配置 |
| `--dist` | `lognormal` | 所要時間の分布。`fixed` / `uniform` / `lognormal` / `bimodal`(軽い 90% + 重い 10%) |
| `--mean` | `200ms` | 平均所要時間 |
| `--fail-ratio` | 0.1 | FAIL になる割合。WARN は同量、残りは OK |
| `--output-bytes` | 256 | stdout のサイズ。`--output-bytes 1MiB` で上限超過の経路を試せる |
| `--min-interval-ratio` | 0.2 | `min_interval` を持つチェックの割合(値は `mean × 20`) |
| `--kind` | `sh` | `sh`: `sleep` を使う POSIX sh スクリプト。`bin`: 同梱の小さな Go ヘルパ(`exectop bench-check`、隠しサブコマンド)を exec する。`bin` は sh 自体の起動コストを除いた exectop 側オーバーヘッドを測るのに使う |
| `--seed` | 1 | 乱数シード。同じシードで同じ root を再生成できる |

生成される各チェックはヘッダに `exectop: interval=... timeout=... min_interval=...` を持ち、所要時間・終了コード・出力サイズはファイル名から決定的に導く(`bench compare` で同一条件を保証するため)。

### 18.4 シナリオ

| シナリオ | 内容 | 主な計測項目 |
|---|---|---|
| `discover` | Discover + Spec 解決のみ(実行しない) | `discover_ms`, `spec_ms`, ファイル数 |
| `run-cold` | daemon 起動直後、DB 空、全件実行 | `wall_ms`, `sum_ms`, `overhead_us`, `parallel_efficiency` |
| `run-cached` | `run-cold` 直後にもう一度 `run`。`min_interval` 持ちは cached になる | `wall_ms`, `cached_count`, `rpc_run_us` |
| `run-standalone` | `--no-daemon` で `run-cold` と同じ | daemon 経由との差分 |
| `overhead` | `--kind bin --dist fixed --mean 0` で N 件。所要 0 のチェックの wall から exectop 側オーバーヘッドを出す | `overhead_us`(= wall × concurrency / N) |
| `join-storm` | 同一チェック(所要 2s)へ 50 クライアントが同時に `run` | 実行回数 = 1、`join_latency_ms` |
| `watch-steady` | `watch` を 60s 稼働(購読者 1) | `overruns`, `sched_lag_ms`(NextRun と実投入の差)p99, `rss_mb`, `goroutines` |
| `rpc-latency` | `hello`, `status`, `list`, `run{wait:false}` を各 1000 回 | 各 p50/p99 µs |
| `persist-burst` | 1 万件の結果を writer に一気に投入 | `commit_ms` p50/p95/p99, `queue_depth_max`, `dropped`, 全件到達までの時間 |
| `persist-slowdisk` | writer のコミットに人工遅延(50〜500ms)を注入して `watch-steady` | `sched_lag_ms` が遅延の影響を受けないこと、`dropped` |
| `restore` | 500 件の `latest` がある DB から daemon を起動 | `restore_ms`, 起動直後の `run` が全件 cached |
| `prune` | 100 万行の `runs` を保持上限まで prune | `prune_ms`, tx あたりの最大時間 |
| `tui-render` | 500 行のスナップショットを 1000 回描画(端末なし、文字列化まで) | `render_ms` p99 |

`bench run --scenario all` は上の順に実行する。`persist-slowdisk` の遅延注入は `EXECTOP_BENCH_COMMIT_DELAY=<duration>` 環境変数を daemon が読むことで行う(bench 以外では無視されるよう、`bench run` が起動する daemon にのみ渡す)。

### 18.5 計測の仕組み(`internal/metrics`)

- 常時有効で低コストなカウンタとヒストグラム。ヒストグラムは固定境界(1µs〜60s を対数 16 分割)で、加算はロックフリーの `atomic` 配列。
- 計測点:
  - `runner.exec_overhead`: `cmd.Start` 呼び出し〜子の開始、`Wait` 戻り〜Result 確定までの合計
  - `runner.duration`: 子プロセスの実所要
  - `sched.lag`: NextRun と実際の投入時刻の差
  - `coord.request`: `run` 要求の受理〜最初の Served 送出
  - `persist.commit`, `persist.queue_depth`(現在値と最大)、`persist.dropped`
  - `rpc.<method>`: 受信〜最終応答
  - `tui.render`
- `status{metrics:true}` RPC でヒストグラムのパーセンタイルと件数を返す。`bench run` はこれを読む。
- プロセスの RSS と goroutine 数は bench が `/proc/<pid>/status` と `runtime.NumGoroutine`(RPC 経由)で 1s ごとに採取する。

### 18.6 Go ベンチマーク(`go test -bench`)

パッケージ単位のマイクロベンチ。PR の CI で実行し、`benchstat` で前回と比較する。

| ベンチ | 対象 |
|---|---|
| `BenchmarkDiscover1000` | 1000 ファイルの Discover |
| `BenchmarkHeaderParse` | ヘッダ 3 行の解析 |
| `BenchmarkResolveChain` | 深さ 4 のディレクトリ設定チェーン解決 |
| `BenchmarkRunnerTrue` | `/bin/true` の 1 実行(オーバーヘッドの下限) |
| `BenchmarkBoundedBufferWrite` | 64KiB 上限のバッファに 1MiB 書き込み |
| `BenchmarkFreshnessDecide` | §7.2 の判定関数 |
| `BenchmarkSnapshot500` | 500 エントリの Snapshot |
| `BenchmarkWriterBatch200` | 200 件バッチのコミット(tmpfs) |
| `BenchmarkNDJSONRoundtrip` | `run` 応答 1 件のエンコード/デコード |
| `BenchmarkRender500` | table 500 行の文字列化 |

### 18.7 ベースラインと回帰検知

- `bench run --json` の出力(§18.9)を `bench/baseline/<hostname>-<cpu>-<disk>.json` として保存する。ホスト情報は `bench` が `/proc/cpuinfo` とマウント種別から自動で付ける。
- `bench compare` は同一シナリオ・同一パラメータの結果だけを比較し、`--tolerance`(既定 10%)を超えて悪化した指標を列挙する。`--fail-on-regression` で終了コード 1。
- CI(GitHub Actions 等の共有ランナー)はばらつきが大きいため、CI では **`overhead`, `discover`, `persist-burst`, `tui-render`** のみを `--tolerance 25%` で比較し、他は結果を保存するだけにする。厳密な比較は固定ホストで行う。
- 各シナリオは `--repeat`(既定 5)回実行し、中央値を採用する。`--warmup`(既定 1)回は捨てる。

### 18.8 合格基準(§15 の目標との対応)

| 指標 | 目標 | シナリオ |
|---|---|---|
| `discover_ms + spec_ms`(1000 件) | ≤ 100 | `discover` |
| `overhead_us`(1 件あたり、`--kind bin`) | ≤ 2000 | `overhead` |
| `parallel_efficiency`(= sum / (wall × concurrency)) | ≥ 0.7(`lognormal`, N=500, conc=16) | `run-cold` |
| `wall_ms`(`run-cached`, 全件 cached) | ≤ 50 + N × 0.05 | `run-cached` |
| `rpc.hello` / `rpc.run(wait:false)` p99 | ≤ 5000 µs | `rpc-latency` |
| `join-storm` 実行回数 | = 1 | `join-storm` |
| `sched.lag` p99(`persist-slowdisk`, 遅延 500ms) | ≤ 50 ms | `persist-slowdisk` |
| `persist.commit` p99(SSD, tmpfs 以外) | ≤ 50 ms | `persist-burst` |
| `persist.dropped`(既定キュー、`persist-burst`) | 0 | `persist-burst` |
| `restore_ms`(500 件) | ≤ 200 | `restore` |
| `prune` の 1 tx 最大時間 | ≤ 100 ms | `prune` |
| `tui.render` p99(500 行) | ≤ 10 ms | `tui-render` |
| `rss_mb`(500 件・履歴 60、60s watch 後) | ≤ 100 | `watch-steady` |
| `watch-steady` 中の goroutine 数の増加 | 0(開始 10s 後と 60s 後で同じ ±5) | `watch-steady` |

### 18.9 出力形式

```json
{
  "exectop_version": "1.0.0",
  "host": {"hostname": "...", "cpu": "...", "ncpu": 8, "kernel": "...", "fs": "ext4", "go": "go1.22.5"},
  "params": {"checks": 500, "dist": "lognormal", "mean_ms": 200, "concurrency": 16, "kind": "sh"},
  "scenarios": {
    "run-cold": {
      "repeat": 5,
      "median": {"wall_ms": 4120, "sum_ms": 98400, "parallel_efficiency": 0.75, "overhead_us": 1130},
      "runs": [ {...}, {...} ]
    },
    "persist-burst": {"median": {"commit_ms_p50": 4.2, "commit_ms_p99": 31.0, "queue_depth_max": 812, "dropped": 0}}
  },
  "pass": true,
  "failures": []
}
```

表形式(`-o table`)は各シナリオの中央値と合格基準、合否を 1 行ずつ出す。

---

## 19. 実装後テスト計画

### 19.1 位置づけ

§17 は実装中に書く単体・コンポーネントテスト(CI で常時実行)。本節は **Phase 5 完了後、v1.0 を宣言する前に一度通す計画**であり、以後はリリースごとに再実行する。各テストには要件 ID(§19.2)を紐づけ、トレーサビリティを保つ。

### 19.2 要件一覧(トレース用 ID)

| ID | 要件(依頼原文から) | 主な検証節 |
|---|---|---|
| R1 | 特定ディレクトリ以下の実行ファイルを再帰的に実行する | §19.4 A1〜A3 |
| R2 | 返り値に応じて成功失敗を判断する | A4 |
| R3 | 並列実行により十分な速度で実行される | §18 `run-cold`, `overhead` |
| R4 | top のように継続実行し結果を一覧する | A5〜A7 |
| R5 | 実行に時間がかかるコマンドの実行周期を管理する(最低実行周期) | A8 |
| R6 | 最低周期を下回る起動では前回結果を返す | A8, A9 |
| R7 | SQLite に実行履歴を保存する | A10, A11 |
| R8 | SQLite への保存がボトルネックにならない(キュー経由) | §18 `persist-slowdisk`, A12 |
| R9 | 常駐プロセスは初回起動以降暗黙に保持され、以降はクエリで処理される | A13〜A15 |
| R10 | 常駐プロセスは root ディレクトリ単位で保持される | A16 |
| R11 | チェック配置の仕様が定義されている | A17(`check-spec.md` の全項目) |
| S1〜S6 | セキュリティ要件(§14) | §19.7 |

### 19.3 テストレベルと実施順序

| レベル | 内容 | 環境 | 合格条件 |
|---|---|---|---|
| L1 単体 | §17 の全テスト、`-race` | CI | 全件 pass、カバレッジ: `runner`, `coordinator`, `persist`, `spec` は行 85% 以上 |
| L2 コンポーネント | daemon を起動せず、Coordinator+MemStore+Writer をプロセス内で結合 | CI | §17 の鮮度判定/join/Writer テスト |
| L3 結合 | 実バイナリで daemon + クライアントを起動し RPC 経由で操作 | CI(Linux ランナー) | §19.4 のうち `[CI]` 印の項目 |
| L4 受け入れ | §19.4 全項目を手順どおり実施 | 固定ホスト | 全項目 pass |
| L5 性能 | §18 `bench all` | 固定ホスト(SSD)+ 低速ディスク 1 種 | §18.8 全項目 pass |
| L6 耐久 | §19.5 | 固定ホスト | §19.5 の判定 |
| L7 障害注入 | §19.6 | 固定ホスト | 各項目の期待動作 |
| L8 セキュリティ | §19.7 | 固定ホスト(複数 uid) | 全項目 pass |
| L9 互換・更新 | §19.8 | 固定ホスト | 全項目 pass |
| L10 環境マトリクス | §19.9 | 複数 VM | L3 と `bench all` が各環境で pass |

L1〜L3 は PR ごと。L4〜L10 はリリース前。L6 は 72 時間かかるため、リリース候補が確定した時点で開始する。

### 19.4 受け入れテスト(要件トレース)

各項目: 前提 → 手順 → 期待。`[CI]` は L3 で自動化する。

**A1 再帰発見 [CI]**(R1, R11)
前提: `checks/{a,b/c}/` に実行ファイル 3 本、`_lib/x.sh`(実行可)、`.hidden.sh`、`readme.md`、非実行ファイル、root 外への symlink。
手順: `exectop list`。
期待: 3 本だけが列挙され、除外 4 件が理由付きで Warning に出る。順序は `a/...`, `b/c/...` の辞書順。

**A2 再走査 [CI]**(R1)
手順: `watch --rescan 1s` 中にチェックを追加・削除・内容更新。
期待: 3s 以内に一覧に反映。削除されたチェックが実行中なら完了後に消える。`checks.deleted_at` が立つ。

**A3 ヘッダとディレクトリ設定の優先順 [CI]**(R11)
前提: 同じキーを CLI、`[checks."x"]`、ヘッダ、`[defaults]`(2 階層)、グローバルで異なる値にする。
期待: `list` の解決結果が §4.2 の順序どおり。詳細画面に「どの層から来たか」が表示される。

**A4 終了コード判定 [CI]**(R2)
前提: exit 0/1/2/3/4/127、`kill -9` 自殺、無限ループ(timeout 1s)、shebang 不正、実行ビットなしの各チェック。
期待: OK/FAIL/WARN/UNKNOWN/FAIL/FAIL/FAIL(signal)/TIMEOUT/ERROR/(発見されない)。`ok=0 warn=1` を書いたチェックで exit 1 が WARN になる。集約終了コードが §8.1 の表どおり(`--fail-on` の 3 通り)。

**A5 watch の一覧と更新**(R4)
手順: 42 件の合成チェックで `watch` を TTY で起動し 2 分観察。
期待: 起動時に画面がクリアされる。状態別カウント、HIST、SUMMARY が更新される。状態変化行がハイライトされる。`s`/`/`/`f`/`Enter`/`h`/`r`/`R`/`p`/`q` が §12.2 どおり動く。`q` 後も daemon と実行が続いている(`daemon status` で確認)。

**A5b ディレクトリビュー [CI]**(R4, R11)
前提: `examples/` に OK 2 本、WARN 1 本、FAIL 1 本、`examples/nested/` に OK 1 本を置く。
手順: TUI を root で起動し、ディレクトリ行と `..` 行を選択する。
期待: root では `examples/` が1行に集約され、FAIL が1本でも集約状態は FAIL になる。Enter で `examples/` に移動し、ヘッダ直下に `..` が表示される。`..` の Enter で root に戻る。左右でカラム選択後に上下を押すと行選択へ戻る。

**A6 非 TTY watch [CI]**(R4)
手順: `watch -o jsonl | head -50`、`watch -o table --refresh 1s`。
期待: 結果ごとに 1 行 JSON。table は 1s ごとに全体が出る。パイプが閉じたらクライアントが終了し daemon は残る。

**A7 多重起動抑止と Overrun [CI]**(R4)
前提: 所要 3s、`interval=1s` のチェック。
手順: `watch` を 20s。
期待: 同時実行数は常に 1(`/proc` で確認)。Overruns が増える。`Interval < Duration` でも投入が詰まらない。

**A8 最低実行周期 [CI]**(R5, R6)
前提: `min_interval=30s min_interval_fail=5s`、所要 2s、引数で成否を切り替えられるチェック。
手順: (1) `run` → 実行。(2) 即 `run` → cached、`age_ms` ≈ 数百ms、`next_eligible_at` = 開始 +30s。(3) `--force` → 実行。(4) FAIL させて `run` → 5s 後の `run` で実行される(fail 用周期)。(5) 実行中に別端末から `run` → joined、同じ `run_id`。
期待: 各段階の `cached/joined/force` フラグと時刻が §7.2 どおり。DB の `runs` には実行分だけが記録される。

**A9 前回結果の復元 [CI]**(R6, R9)
手順: A8(1) の後 `daemon stop` → `run`。
期待: daemon が自動起動し、結果は `restored:true, cached:true`。`EXECTOP_ITER` が継続する(次の実行で iter+1)。

**A10 履歴保存 [CI]**(R7)
手順: 100 回実行した後、`sqlite3` で `runs`, `run_output`, `latest`, `transitions` を確認。`history <ID>`。
期待: 行数・内容が一致。`transitions` は状態が変わった回だけ。gzip された出力が復号できる。

**A11 保持と prune**(R7)
手順: 保持を `runs="1m" runs_max=10` にして 5 分 watch。`db prune` と `db vacuum`。
期待: 各チェック 10 件以下、DB サイズが減る(`incremental_vacuum` 後)。watch の履歴クエリが prune 中に 100ms 以上止まらない。

**A12 キューの非ブロッキング [CI]**(R8)
手順: `EXECTOP_BENCH_COMMIT_DELAY=500ms` で daemon を起動し、`interval=1s` × 200 件を watch。
期待: `sched.lag` p99 ≤ 50ms。`persist q=` が増減し、`dropped` は既定キューでは 0。`persist.block=true` では投入がブロックされ `sched.lag` が伸びる(仕様どおり)。

**A13 暗黙起動と保持 [CI]**(R9)
手順: daemon が無い状態で `run` → `daemon status`。クライアント終了後 10 分放置 → `daemon status`。
期待: 初回で daemon が起動し、放置後も生存(`idle_exit` 未設定)。`idle_exit=1m` を設定して再起動すると 1 分後に終了し、次の `run` で再起動する。

**A14 同時起動の単一性 [CI]**(R9)
手順: daemon が無い状態で 20 クライアントを同時に `run`。
期待: daemon は 1 プロセス、全クライアントが正常終了、実行回数はチェックごとに 1。

**A15 stale ソケットと再接続 [CI]**(R9)
手順: `kill -9` で daemon を落とし、ソケットファイルを残したまま `run`。watch 中に daemon を `restart`。
期待: 前者は正常起動。後者は TUI が `DISCONNECTED` を出し、数秒で再接続して更新が続く。

**A16 root 単位の daemon [CI]**(R10)
手順: 2 つの root で `run`。片方を symlink 経由でも指定。
期待: daemon が 2 つ(symlink は同一 root と判定され増えない)。ソケット・DB・ログが root-hash ごとに分かれる。片方の root を削除 → 1 分後にその daemon だけ終了。

**A17 チェック仕様の網羅**(R11)
手順: `check-spec.md` の各項目(環境変数、stdin、cwd、状態ディレクトリ、出力規約、構造化出力)を検証するチェックを 1 本ずつ書き、`run -o json`。
期待: 全項目が記述どおり。仕様書と挙動の差分があれば仕様書側を直すか実装を直すかを決めて記録する。

### 19.5 耐久テスト(soak)

`scripts/soak.sh` で起動。

| 項目 | 内容 |
|---|---|
| 構成 | 500 件(`bench gen --dist bimodal`)、`interval` 5〜60s 混在、`min_interval` 20%、購読者 2(TUI 1 + jsonl 1)、cron から 1 分ごとに `run` |
| 期間 | 72 時間(最低 24 時間) |
| 採取(1 分ごと) | RSS、goroutine 数、fd 数、DB サイズ、WAL サイズ、`persist.dropped`、Overruns、孤児プロセス数(`ps --ppid 1` のうちチェック由来) |
| 判定 | RSS の増加が最初の 1 時間以降で 10% 以内。goroutine と fd が一定(±10)。DB サイズが保持設定の上限で頭打ち。WAL が checkpoint されて 100MB 未満に戻る。dropped = 0。孤児 0。daemon の再起動 0。クライアント `run` の失敗 0 |

### 19.6 障害注入テスト

`internal/fault` は build tag `fault` でのみコンパイルされ、`EXECTOP_FAULT=<point>[:<param>]` で発火する。リリースバイナリには含めない。障害はできる限り外部から(kill、ディスク、権限)与え、注入点は外部から作れないものに限る。

| # | 障害 | 与え方 | 期待 |
|---|---|---|---|
| F1 | 実行中に daemon を `kill -9` | 外部 | 子プロセスが孤児にならない(プロセスグループが親の死で消えないため、**再起動した daemon が起動時に前回の pgid を lock ファイルから読んで kill する**。この挙動を §11.1 に追記済みとして実装する)。次の `run` が正常 |
| F2 | コミット中に `kill -9` | `EXECTOP_FAULT=persist.commit_hang` + 外部 kill | 再起動後 `quick_check` ok。`latest` と `runs` の整合。欠落は最後の 1 バッチのみ |
| F3 | ディスクフル | tmpfs を小さくマウント | writer がエラーをログし実行は継続。空きができたら書き込み再開 |
| F4 | fsync 遅延 500ms | `EXECTOP_BENCH_COMMIT_DELAY` | A12 と同じ |
| F5 | root ディレクトリを削除→復元 | 外部 | 1 分以内に削除なら継続、超えたら終了。復元後の `run` で再起動 |
| F6 | SIGTERM を無視して残る子 | `trap '' TERM; sleep 1000` | grace 後に SIGKILL、孫も消える |
| F7 | 子が自前の daemon を fork(setsid) | `setsid sleep 1000 &` | 別セッションのため pgid kill では消えない → **仕様上の限界として記録**し、`check-spec.md` に「チェックは setsid で子を切り離さないこと」を明記 |
| F8 | 100MB を出力するチェック | 外部 | 64KiB で切り詰め、`truncated`。メモリが増えない |
| F9 | 時刻の後退(NTP 補正) | `date -s` またはコンテナの `faketime` | 鮮度判定と Scheduler が単調時計を使い、誤って cached/未投入にならない |
| F10 | ソケットディレクトリを削除 | 外部 | 既存接続は継続。新規クライアントは daemon を再起動できず exit 70 → `daemon restart` で復旧 |
| F11 | DB を破損(ヘッダを上書き) | 外部 | 起動時に退避して新規作成。`daemon_events` に記録。前回結果は失われるが実行は継続 |
| F12 | DB をロックする外部プロセス | `sqlite3` で `BEGIN EXCLUSIVE` を保持 | `busy_timeout` 後に writer がリトライ、実行は継続。解放後に書き込み再開 |
| F13 | クライアントが受信を止める(`run` 中に SIGSTOP) | 外部 | daemon の送信が詰まらない(送信バッファ上限で切断)。他クライアントに影響なし |
| F14 | `.exectop.toml` に構文エラー | 外部 | そのディレクトリのチェックが ERROR、他は継続。`reload` で修正が反映 |

### 19.7 セキュリティテスト(§14)

| # | 内容 | 期待 |
|---|---|---|
| S1 | root/中間ディレクトリ/チェックを o+w、g+w、他ユーザー所有にする各組み合わせ | 既定で拒否、`--allow-unsafe` で警告。実行直前の再検査でも拒否(Discover 後に chmod) |
| S2 | root 外への symlink、root 内の symlink | 前者は無視、後者は許可。`--follow-symlinks` で前者も許可 |
| S3 | 別 uid からソケットに接続 | 切断、ログに記録。ソケットとディレクトリの権限が 0600/0700 |
| S4 | `LD_PRELOAD`, `BASH_ENV` を設定した環境から `run`(`--inherit-env` あり/なし) | どちらでもチェックに渡らない |
| S5 | チェックが ESC シーケンスと制御文字を出力 | TUI と table でエスケープが無効化。JSON と DB は原文 |
| S6 | `os.Executable()` を o+w にして自動起動 | 起動拒否 |
| S7 | RPC に不正な JSON、巨大行(16MiB 超)、未知メソッド、Spec 上書きを装ったパラメータ | エラー応答のみ。プロセスは落ちない。任意実行に至らない |
| S8 | チェック ID に `../` を含む要求 | 拒否(ID は Discover 結果との照合のみ) |
| S9 | root で daemon を起動し非 root から接続 | S3 と同様に拒否 |

### 19.8 互換性・更新テスト

| # | 内容 | 期待 |
|---|---|---|
| C1 | daemon 稼働中にバイナリを更新して `run` | `exe_mtime` 差で drain → 再起動。実行中の結果は失われない |
| C2 | `proto_version` を意図的に下げた旧クライアントで接続 | エラーと更新案内 |
| C3 | スキーマ v1 の DB(フィクスチャ)で新版を起動 | マイグレーションが走り、`schema_meta` が更新、既存履歴が読める |
| C4 | 新版で作った DB を旧版で開く | 旧版が「新しいスキーマ」として起動を拒否する(ダウングレード非対応を明示) |
| C5 | `exectop.toml` に未知キー | 警告のみで起動 |

### 19.9 環境マトリクス

| 軸 | 値 |
|---|---|
| OS | Ubuntu 22.04 / 24.04、Debian 12、Rocky 9(いずれも x86_64)、Ubuntu 24.04 arm64 |
| ファイルシステム(DB 置き場) | ext4、xfs、tmpfs、NFS(性能のみ、合格基準は緩和) |
| Go | 最小サポート版と最新安定版 |
| 実行ユーザー | 一般ユーザー、root(警告確認) |
| 端末 | 80×24、200×50、`TERM=dumb`、非 TTY |

各環境で L3 と `bench all` を実行し、結果 JSON を `bench/baseline/` に保存する。

### 19.10 合否判定と報告

- 判定: L1〜L10 の全項目 pass。§18.8 の全指標 pass。既知の制限(F7 など)は `limitations.md` に記載されていること。
- 報告書に含めるもの: 実施日、コミットハッシュ、環境、各レベルの結果表、`bench all` の JSON、不合格項目と対処(修正/仕様変更/制限として記録)、耐久テストの採取グラフ。
- 不合格が出た場合: 修正後は当該レベル以降を再実施。`runner`, `coordinator`, `persist` の修正は L6 を再実施する。

### 19.11 リリースチェックリスト

- [ ] L1〜L10 が pass し、報告書が保存されている
- [ ] `bench/baseline/` が更新されている
- [ ] `check-spec.md` が §4 と一致(A17 で検証済み)
- [ ] `limitations.md` に F7 等の既知制限が記載されている
- [ ] スキーマを変更した場合、マイグレーションと C3/C4 が pass
- [ ] `proto_version` を変更した場合、C1/C2 が pass
- [ ] `go vet`, `staticcheck`, `govulncheck` が clean
- [ ] リリースバイナリに build tag `fault` が含まれていない(`go version -m` で確認)

---

## 20. 主要な設計判断とトレードオフ

| 論点 | 選択肢 | 選択と理由 |
|---|---|---|
| 成功失敗の粒度 | (a) 0/非0 の二値 (b) 0/1/2/3 の四値 (c) Nagios 互換 | **(b)**。二値では「注意」を表現できない。(c) は素朴な `exit 1` が WARN になり直感に反する。マッピングはチェック単位で上書き可能 |
| メタデータの置き場 | (a) ファイル名規約 (b) ヘッダコメント (c) サイドカー (d) `--describe` プロトコル | **(b)+ディレクトリ設定**。(a) は表現力不足、(d) は起動コストが倍 |
| 継続実行の周期 | (a) 全体で一つの tick (b) チェックごとの interval | **(b)**。重いチェックと軽いチェックが混在する前提(P2)。(a) は一律指定で再現できる |
| 実行の多重起動 | (a) 許す (b) 実行中なら投入を飛ばす/join | **(b)**。詰まると雪だるま式に悪化する。飛ばした回数は Overruns として可視化 |
| 鮮度判定の基準 | 開始時刻 / 終了時刻 | **開始時刻**。周期の予測が立つ |
| 実行の所有者 | (a) 各クライアント (b) daemon (c) SQLite の lease で調停 | **(b)**。判定が一箇所に集まり、join と購読配信が自然。(c) は単独モードの退避経路として実装(付録 C) |
| キューの位置づけ | スループット対策 / 尾部遅延対策 | **尾部遅延対策**。SQLite は想定規模で律速しない(§9.3)。fsync スパイクを実行経路から外すのが目的 |
| 背圧 | ドロップ / ブロック | **ドロップ(計数)**。監視停止より履歴欠落の方が害が小さい。監査用途は `block=true` |
| 購読者ゼロ時の Scheduler | 稼働 / 停止 | **停止**。誰も見ていないのに走らせ続けない。`background=true` で常時稼働 |
| daemon の寿命 | 無期限 / idle で終了 | **無期限**(要件)。`idle_exit` で切替可 |
| 履歴の保持(メモリ) | 出力込み / 状態・時間のみ | **状態・時間のみ**。出力は最新のみ+DB |
| SQLite ドライバ | modernc / mattn | **modernc**。cgo 不要。`database/sql` で差し替え可能に |
| RPC | NDJSON / gRPC | **NDJSON**。依存なし、手動デバッグ可 |
| TUI ライブラリ | bubbletea / tview / 自前 | **bubbletea**。イベント駆動が「購読→描画」に合う |
| CLI ライブラリ | cobra / stdlib | **stdlib**。サブコマンド数に対し cobra は過剰 |
| シェル経由起動 | する / しない | **しない** |
| rlimit | 設定 / しない | **しない**(v1)。timeout と出力上限で主要リスクは抑えられる |

---

## 21. 段階的実装計画

| Phase | 内容 | 完了条件 |
|---|---|---|
| 1 | discover, spec, runner, pool, memstore, `run`(単独モード)+ table/json, 集約終了コード, 安全性検査 | §17 の Discover/Spec/Runner/安全性テストが通る。`exectop run --no-daemon` が cron/CI から使える |
| 2 | persist(schema, writer, reader, prune, 復元)+ `--no-daemon --db`(lease 方式の鮮度判定) | Writer/クラッシュ耐性/prune/単独モードのテストが通る |
| 3 | daemon + rpc + launcher + coordinator(鮮度判定・join・fan-out)。`run`/`list`/`history` を RPC 化。`exectop daemon *` | 鮮度判定/join/復元/単一性/peer 検査/バージョン不一致のテストが通る。`exectop run` が daemon 経由で動く |
| 4 | Scheduler を daemon に置き `watch` を購読化。TUI(一覧・詳細・履歴) | Scheduler テストが通る。500 チェックで TUI が滑らかに更新される |
| 5 | 状態ディレクトリ, `rescan`, nice, 構造化出力, `check` サブコマンド, `exectop db *`, `exectop bench *`(§18) | 差分監視のサンプルチェックが動く。`bench all` が §18.8 の目標を満たしベースラインを保存できる |
| — | Phase 5 完了後に §19 の実装後テスト計画を実施し、合格をもって v1.0 とする | §19.9 の合否判定 |
| 6(任意) | idle_exit, systemd unit 出力, Prometheus エクスポート, prlimit | 要件が出てから着手 |

Phase 1→2→3 の順にしたのは、単独モードが最後まで有効な退避経路として残るため。

### 21.1 現在までの実装反映

次の追加要件は実装済みであり、今後の受け入れ確認では A5/A5b を対象にする。

- `watch` TUI のディレクトリ単位集約と、再帰的な配下ステータス集計
- ディレクトリ行の Enter 移動、および仮想 `..` 行による親移動
- 左右キーでのカラム選択と、上下キーによる行選択への復帰
- TTY watch 起動時・更新時の画面クリア
- スクリプトの有効・無効をパスと発見規則だけで決定し、`disabled` 設定を禁止

---

## 22. 未決事項(実装者への委譲と決着条件)

| # | 未決 | 暫定 | 決着条件 |
|---|---|---|---|
| U1 | 既定の `interval`(10s)と `timeout`(10s)が同値でよいか | そのまま | 実利用で Overruns が常態化するチェックが 1 割を超えるなら timeout < interval を既定にする |
| U2 | WARN の既定終了コードを 2 とするか | 2 | 既存資産の大半が Nagios 流(1=WARN)ならグローバル既定を `ok=0 warn=1` に切り替える |
| U3 | `watch` の非 TTY 既定を jsonl とするか table とするか | jsonl | パイプ先の主用途がログ収集なら jsonl、人間が `less` で見るなら table |
| U4 | ヘッダ解析を先頭 32 行に限定して十分か | 32 行 | ライセンスヘッダ等で 32 行を超える実例が出れば 64 行に拡大 |
| U5 | `_` プレフィクスを除外規約に採用するか | 採用済み | `.` / `_` 始まりと `.disabled` 拡張子を除外規約として固定 |
| U6 | 鮮度判定の基準を開始時刻とするか終了時刻とするか | 開始 | 「終了から N 秒は再実行しない」が要件として出てきたら `min_gap`(終了基準)を別キーで追加 |
| U7 | 購読者ゼロのとき Scheduler を止めるか | 止める | 「見ていない間の履歴」が必要になった時点で `background=true` を既定にする |
| U8 | daemon の idle_exit 既定 | なし(無期限) | メモリや fd の長期占有が問題視されたら `24h` を既定にする |
| U9 | cached 結果を集約終了コードに含めるか | 含める(`--max-age` で UNKNOWN 化可) | cron 側で「古い成功」を信じたくない運用が主なら既定 `--max-age = 2×min_interval` |
| U10 | SQLite ドライバ(`modernc` vs `mattn`) | modernc | writer のコミット時間が p99 で 50ms を超え、かつ cgo が許容されるなら mattn |
| U11 | 出力 BLOB の圧縮閾値 4KiB | 4KiB | DB サイズの過半が `run_output` なら閾値を 1KiB に下げる、または zstd を検討 |

---

## 付録 A. 状態遷移

```
PENDING --(投入)--> RUNNING --(exit ∈ ok)-------> OK
                            --(exit ∈ warn)-----> WARN
                            --(exit ∈ unknown)--> UNKNOWN
                            --(その他 exit)-----> FAIL
                            --(signal)----------> FAIL
                            --(timeout)---------> TIMEOUT
                            --(start 失敗)------> ERROR
   path/name exclusion --------------------------> not discovered

要求 --(running)---------------------------------> joined  (RUNNING の完了結果を共有)
要求 --(t - s < min_interval)--------------------> cached  (最新結果をそのまま返す。遷移なし)
```

## 付録 B. Runner の擬似コード(Linux)

```go
func (r *linuxRunner) Run(parent context.Context, s Spec, m RunMeta) Result {
    if err := safety.RecheckFile(s.Path, r.opts); err != nil {
        return errorResult(s, m, time.Now(), err)          // §14.1 実行直前の再検査
    }
    ctx, cancel := context.WithTimeout(parent, s.Timeout)
    defer cancel()

    cmd := exec.CommandContext(ctx, s.Path, s.Args...)
    cmd.Dir = s.Cwd
    cmd.Env = buildEnv(s, m)                 // 最小継承 + EXECTOP_*
    cmd.Stdin = nil                          // /dev/null
    out := newBoundedBuffer(s.OutputLimit)
    errb := newBoundedBuffer(s.OutputLimit)
    cmd.Stdout, cmd.Stderr = out, errb
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
    cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
    cmd.WaitDelay = s.Grace

    start := time.Now()
    if err := cmd.Start(); err != nil {
        return errorResult(s, m, start, err)
    }
    if s.Nice != 0 { _ = syscall.Setpriority(syscall.PRIO_PGRP, cmd.Process.Pid, s.Nice) }

    waitErr := cmd.Wait()
    _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // 孫の掃除。ESRCH は無視
    end := time.Now()

    return classify(s, m, start, end, cmd.ProcessState, waitErr, ctx.Err(), out, errb)
}
```

## 付録 C. 単独モードの鮮度判定(SQLite lease 方式)

daemon を使わない `--no-daemon --db` で、複数プロセス間の二重実行を防ぐ方法。

```sql
BEGIN IMMEDIATE;
SELECT status, started_at, lease_until FROM latest WHERE check_id = ?;
-- アプリ側で判定:
--   lease_until > now               → 他プロセスが実行中。cached(最新結果)を返すか、lease 失効までポーリングして再読込
--   now - started_at < min_interval → cached
--   それ以外                        → lease_until = now + timeout + grace を書いて COMMIT し、実行
UPDATE latest SET lease_until = ? WHERE check_id = ?;
COMMIT;
-- 実行後: runs/latest を書き、lease_until = NULL
```

- 利点: 常駐プロセスがなく、ライフサイクル管理と接続元検証が不要。
- 欠点: 実行中の完了を「待って同じ結果を受け取る」にはポーリングが要る。watch の購読配信ができない。lease 失効(実行中のクラッシュ)は timeout 依存で曖昧。
- 位置づけ: 退避経路。利用形態が cron からの `run` のみで watch を使わないなら、この方式だけで運用してもよい。

## 付録 D. ファイル配置まとめ

| 内容 | パス |
|---|---|
| チェック | `<root>/**` |
| グローバル設定 | `<root>/exectop.toml` |
| ディレクトリ設定 | `<root>/**/.exectop.toml` |
| DB | `$XDG_STATE_HOME/exectop/<root-hash>/exectop.db` |
| ロック | `$XDG_STATE_HOME/exectop/<root-hash>/daemon.lock` |
| ログ | `$XDG_STATE_HOME/exectop/<root-hash>/daemon.log` |
| 状態ディレクトリ | `$XDG_STATE_HOME/exectop/<root-hash>/state/<escaped-id>/` |
| ソケット | `$XDG_RUNTIME_DIR/exectop/<root-hash>.sock`(代替 `/tmp/exectop-<uid>/`) |
