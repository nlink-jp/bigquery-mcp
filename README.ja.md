# bigquery-mcp

保護重視の BigQuery MCP サーバー。利用者自身の Application Default Credentials でローカルに stdio 動作し、全クエリを実行前に dry run して、予算内の単一 SELECT 以外を拒否し、行を列名キーのオブジェクトで明示キャップ付きで返し、失敗は `{code, message, retryable, details}` で報告します。エージェントは誰の落ち度で何を変えればよいかを読み取れます。

LLM は呼びません。SQL はエージェントが書きます。

## 公式リモート MCP サーバーではなく本サーバーを使う理由

BigQuery 自身のリモート MCP エンドポイントは汎用です。dry run の予算ゲートが無く、読取専用ツールは SQL 文字列を検査し、行は生の `{f:[{v:…}]}` 形式で、エラーは原因も再試行の手がかりも無い一文で、stdio クライアントはブリッジ経由でしか届きません。bigquery-mcp は企業内 DWH を分析するエージェントのためにあります。操作者はクエリのコスト上限、読める範囲の境界、そしてエージェントが自分のランタイムを調べに行く代わりに行動できるエラーを求めています。

## インストール

リリースページからプラットフォーム別のアーカイブを取得して展開し、`bigquery-mcp` を `PATH` に置きます。macOS ビルドは署名・notarize 済みです。

ソースからのビルド:

```bash
make build      # → dist/bigquery-mcp（`go build` を直接使わない）
```

## セットアップ

三段階です。`bigquery-mcp doctor` が順に検査し、最初に失敗した段を名指しします。

1. **資格情報** — Application Default Credentials:

   ```bash
   gcloud auth application-default login
   ```

   ADC は機体上の全ツール（gem-agent の Vertex AI 呼び出しを含む）が共有する 1 ファイルです。本サーバーのためにそのスコープを狭めないでください。権限の縮小は下の IAM で行います。

2. **IAM** — 資格情報に必要なロール。書けるものは付けません:

   | 対象 | ロール | 理由 |
   |---|---|---|
   | billing プロジェクト | `roles/bigquery.jobUser` | クエリジョブの作成 |
   | データプロジェクト / データセット | `roles/bigquery.dataViewer` | テーブルの読取 |
   | 探索のみ許すプロジェクト | `roles/bigquery.metadataViewer` | 一覧と説明 |

   `dataEditor` 以上を付けないことが恒久的な読取専用の境界です。サーバー自身の SELECT 検査は二重目であって唯一ではありません。

3. **設定** — [config.example.toml](config.example.toml) を `~/.config/bigquery-mcp/config.toml` にコピーし、billing プロジェクトを設定します:

   ```toml
   [project]
   id = "your-billing-project"

   [budget]
   max_bytes_billed = "10GiB"
   job_timeout      = "3m"

   [access]
   # datasets = ["proj.dataset", "proj2.*"]

   [results]
   default_max_rows = 1000
   hard_max_rows    = 50000
   max_bytes        = "1MiB"

   [logging]
   # log_file    = ""
   # log_level   = "info"
   # log_queries = false
   ```

   設定の解決順: `--config` → `$BIGQUERY_MCP_CONFIG` → `~/.config/bigquery-mcp/config.toml`。未知のキーはエラーです。ファイルに資格情報は含まれません。

**1 インスタンス = 1 billing プロジェクト。** 複数使うときは config を分け、別名で登録します:

```json
{
  "mcpServers": {
    "bigquery-prod":    { "command": "bigquery-mcp", "args": ["--config", "/path/to/prod.toml"] },
    "bigquery-sandbox": { "command": "bigquery-mcp", "args": ["--config", "/path/to/sandbox.toml"] }
  }
}
```

このブロックは Claude Desktop、Claude Code（`.mcp.json` または `~/.claude.json`）、gem-agent（`~/.config/gem-agent/mcp.json` またはプロジェクトの `.mcp.json`）でそのまま使えます。

## ツール

| ツール | 用途 |
|---|---|
| `list_datasets` | プロジェクトのデータセット一覧（既定は billing プロジェクト）。`project` は任意で課金されません |
| `list_tables` | データセットのテーブルとビュー、パーティション列付き |
| `describe_table` | スキーマ（ネスト含む）、パーティション列と粒度、クラスタ、行数、サイズ、期限、ビューの SQL |
| `dry_run` | バイト数（生と課金推定）、精度、文種別、参照テーブルとルーチン、未宣言パラメータ、結果スキーマ、ゲートの判定、警告 |
| `query` | dry run → ゲート → 実行。行は列名キーのオブジェクトでキャップ付き |
| `get_usage` | エラー回復表を含む完全なリファレンス |

### `query` が毎回すること

1. **dry run**（`jobs.insert`）— 文種別、バイト推定、参照テーブルとルーチン。
2. **ゲート**。この順で、いずれかが失敗すれば何も走りません:
   - BigQuery が文を `SELECT` と分類しなければ `statement_not_allowed`（スクリプト、DML、DDL、EXPORT、CALL、ASSERT はすべてここで落ちます）
   - `[access] datasets` が設定され、参照テーブルかルーチンがその外にあれば `dataset_not_allowed`
   - 課金推定（MiB 切り上げ、テーブルごと最小 10 MiB）が `[budget] max_bytes_billed` を超えれば `budget_exceeded`
3. **実行**。本サーバーが名付けたジョブとして `maximumBytesBilled`、`jobTimeoutMs`、ラベル `bigquery-mcp: true` を付けて走ります。課金を切り分けられ、止められたジョブは原因付きで報告されます。
4. **整形**。行は列名キーのオブジェクトです。ネストレコードはオブジェクト、繰り返しは配列、TIMESTAMP は ISO 8601 UTC 文字列、NUMERIC と 53 ビットを超える整数は文字列です。

結果は `max_rows`（呼び出しごと。既定と上限は config）か、レスポンスのバイト予算 `max_bytes` で止まります。どちらでも `truncated: true` になり、`truncated_by` にキャップの名前、`total_rows` に BigQuery の件数が入ります。黙って落とすことはありません。サーバーはファイルを書きません。大きな結果をファイルにするのはエージェントランタイムの仕事です。

パラメータ: `"params": {"n": 3, "s": "x", "day": {"type": "DATE", "value": "2026-09-05"}}`。文字列・数値・真偽値は型を推定し、それ以外のスカラー型は明示形式を使います。パーティションフィルタにはパラメータでなくリテラルを使ってください。パラメータ化したフィルタは dry run 時に刈り込まれないことがあります。

### エラー

全エラーは 1 つの JSON オブジェクトで、安定した `code`、何を変えるかを言う `message`、常に存在する `retryable`（true ならサーバーが既に 1 回再試行済み）、`details`（BigQuery の reason と location、ジョブ ID、推定値と予算）を持ちます。完全な表は `get_usage` にあります。

## 注記

- `maximumBytesBilled` はオンデマンド課金でのみ効きます。Editions では、ゲートは dry run の推定値だけに依存します。
- SELECT でもリモート関数や `ML.` / `AI.` 関数を通じてバイト予算の外でお金を使えます。そこでの境界は接続とモデルに対する IAM です。
- `[project] location` は通常未設定のままにします。BigQuery がデータセットから決めますし、固定すると他リージョンへのクエリが `not_found` で失敗します。
- 隠しデータセット（`_` で始まる名前）は一覧に出ません。
- 長いクエリ: ジョブタイムアウトの既定は 3 分です。非同期ジョブは後のリリースで予定しています。

## 開発

```bash
make test        # 単体 + 偽サーバーテスト（資格情報不要）
make vet
make check       # vet + test + build
make live-test   # 実プロジェクトへの -tags live テスト:
                 #   BIGQUERY_MCP_TEST_PROJECT=<billing project> BIGQUERY_MCP_TEST_DATASET=<dataset> make live-test
```

設計記録: [RFP](docs/ja/bigquery-mcp-rfp.ja.md)、[ADR](docs/ja/adr/)、[アーキテクチャ](docs/ja/architecture.ja.md)。

## ライセンス

MIT
