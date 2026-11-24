# exectop examples

このディレクトリは `exectop examples` で生成されたサンプルです。

- LLM agent向けの作成契約は [`AGENTS.md`](./AGENTS.md)
- カラム設定は各ディレクトリの `.exectop.column`
- 実際の依存性検査は `host/software/06-runtime-dependencies.sh`
- 新規作成の雛形は [`check-template.txt`](./check-template.txt)
- 構造化出力の例は各チェックのstdout 1行目

新しいチェックを追加したら、実行ビットを付け、`AGENTS.md` の検証手順を実行して
から監視に組み込んでください。

## Cloud health examples

`cloud/` には AWS、Google Cloud、Azure の公開ヘルス情報を確認するチェックが
あります。これらはアカウントや個別リソースの状態ではなく、各社が公開している
広域インシデント情報だけを判定します。`python3` とネットワーク接続が必要です。

- 終了コード `0`: アクティブな公開イベントなし
- 終了コード `1`: 障害（outage）
- 終了コード `2`: 劣化・サービス中断（degraded/disruption）
- 終了コード `3`: 取得失敗、タイムアウト、またはフィード形式変更

AWSチェックはデフォルトでUAE（`me-central-1`）のイベントを除外します。
含める場合は `AWS_HEALTH_IGNORE_UAE=0`（`false`、`no`、`off` も可）を設定します。
