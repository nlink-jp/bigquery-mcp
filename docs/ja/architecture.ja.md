# bigquery-mcp アーキテクチャ

この文書は各部品が**なぜ**その形なのかを説明する。決定は RFP（`bigquery-mcp-rfp.ja.md`）が、非自明な決定の理路は ADR が記録する。`query` の実行経路を変える前に読むこと。

## クエリが辿る唯一の経路

```
model ──tools/call query──▶ tools.query
                              │ 引数の解析と検証（max_rows ≤ hard_max_rows）
                              ▼
                         bq.DryRun ──▶ jobs.insert{dryRun}  （statementType・bytes・referencedTables・schema）
                              │
                              ▼  ゲート、この順で（ADR-0002）
                         statementType == SELECT ?      ──✗ statement_not_allowed
                         参照テーブル ⊆ 許可リスト ?     ──✗ dataset_not_allowed
                         bytes ≤ max_bytes_billed ?      ──✗ budget_exceeded
                              │
                              ▼
                         bq.Query ──▶ jobs.insert{jobId=bqmcp-…, maximumBytesBilled, jobTimeoutMs, labels}
                                   ├─▶ jobs.getQueryResults{location, timeoutMs} … jobComplete まで
                                   ├─▶ jobs.get{location}  （課金バイト・スロット ms・キャッシュ・文種別）
                                   └─▶ jobs.getQueryResults{location, pageToken, maxResults} …
                              │
                              ▼
                         行の整形（ADR-0003）: 列名キーのオブジェクト、max_rows か
                         max_bytes で停止、切り詰めを計上
                              │
                              ▼
                         1 つの JSON text block ──または── 1 つの toolerr JSON（ADR-0004）
```

ツール層のどこもこの順序を並べ替えられない。`dry_run` は同じ `bq.DryRun` にゲートの判定をデータとして描画したものである。

## パッケージ

| パッケージ | 役割 | 出自 |
|---|---|---|
| `cmd/` | cobra: `serve`（既定）、`doctor`、`version`、`--config` | splunk-mcp の形 |
| `internal/transport` | stdio 上の改行区切り JSON-RPC、1 MB 行 | data-toolbox-mcp から移植 |
| `internal/jsonrpc` | JSON-RPC 2.0 の型とコード | 移植 |
| `internal/mcpserver` | MCP 2024-11-05 ルーティング、`RegisterTool`、`RawResult`、構造化ツールエラー | 移植 |
| `internal/toolerr` | `{code, message, retryable, details}`（ADR-0004） | 移植、コード差し替え、`Retryable` 追加 |
| `internal/logging` | stderr と任意のローテーション付きファイルへの slog | 移植 |
| `internal/config` | sectioned TOML、未知キー拒否、サイズと期間の解析 | 新規 |
| `internal/bq` | `net/http` の REST v2 クライアント、ADC トークンソース、エラー写像、再試行（ADR-0001、ADR-0004） | 新規 |
| `internal/tools` | 6 ツール、引数解析、ゲート、行整形（ADR-0002、ADR-0003） | 新規 |

**stdout は JSON-RPC のもの。** どのパッケージもそこへ出力してはならない。診断は slog を通す。

## `internal/bq` — 何を読み書きするか

REST ベース: `https://bigquery.googleapis.com/bigquery/v2`。全リクエストは `google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/bigquery")` からの `Authorization: Bearer` と、本サーバー名と版数を名乗る `User-Agent` を運ぶ。

| 呼び出し | 使うリクエストフィールド | 読むレスポンスフィールド |
|---|---|---|
| `POST projects/{p}/jobs`（dry run） | `configuration.dryRun=true`、`configuration.query.{query,useLegacySql=false,parameterMode,queryParameters}`、`configuration.labels`、`jobReference.location?` | `jobReference.location`、`statistics.query.{statementType,totalBytesProcessed,totalBytesProcessedAccuracy,referencedTables,referencedRoutines,undeclaredQueryParameters,schema}`、`status.errorResult` |
| `POST projects/{p}/jobs`（実行） | `jobReference.{projectId,jobId=bqmcp-<32 hex>,location?}`、`configuration.{jobTimeoutMs,labels}`、`configuration.query.{query,useLegacySql=false,maximumBytesBilled,useQueryCache=true,priority=INTERACTIVE,parameterMode,queryParameters}` | `jobReference.location`、`status.errorResult`; 再試行時の `409 duplicate` は最初の insert が届いていた印 |
| `GET projects/{p}/queries/{jobId}` | `location`、`timeoutMs=30000`、`maxResults`、`pageToken`、`formatOptions.timestampOutputFormat=ISO8601_STRING` | `jobComplete`、`jobReference`、`schema`、`rows`、`totalRows`、`pageToken`、`totalBytesProcessed`、`errors` |
| `GET projects/{p}/jobs/{jobId}` | `location` | `statistics.query.{statementType,totalBytesBilled,totalSlotMs,cacheHit,totalBytesProcessed}`、`statistics.totalSlotMs` |
| `GET projects/{p}/datasets` | `pageToken`、`maxResults` | `datasets[].{datasetReference,location}`、`nextPageToken` |
| `GET projects/{p}/datasets/{d}/tables` | `pageToken`、`maxResults` | `tables[].{tableReference,type,timePartitioning,clustering}`、`nextPageToken` |
| `GET projects/{p}/datasets/{d}/tables/{t}` | — | `schema`、`type`、`numRows`、`numBytes`、`timePartitioning`、`rangePartitioning`、`clustering`、`creationTime`、`lastModifiedTime`、`expirationTime`、`description` |

`location` は最初の応答から同じジョブへの以後の全呼び出しへ引き回す（リージョナルなデータセットでは必須）。

行の復号: BigQuery は `rows[].f[].v` をスキーマ順で返し、ネストレコードはさらに `{f:[...]}`、繰り返しフィールドは `[{v:...}]` になる。復号器はスキーマを再帰的に歩いて列名キーのオブジェクトを作る: `RECORD` → オブジェクト、`REPEATED` → 配列、`BYTES` は base64 のまま、`TIMESTAMP` は ISO 8601 UTC 文字列（`ISO8601_STRING`、ピコ秒精度まで）を要求してそのまま通す — int64 マイクロ秒と秒の浮動小数はフォールバックとしてのみ復号 — `NUMERIC`/`BIGNUMERIC` → 文字列、`INTEGER` → 53 ビットに収まれば数値、さもなくば文字列、`BOOLEAN` → bool、`JSON` → 埋め込み JSON。

エラー写像（`errors.go`）: `error.errors[0].reason` が唯一の `reasonRules` 表を通じてコードを決め、HTTP ステータスは補助に過ぎない（ADR-0004 §2）。`message`・`location`・`reason`・`job_id` が `details` に乗る。再試行: retryable なコードに対して 500〜1500 ms 後に 1 回。全呼び出しは送信時点で冪等 — dry run はジョブを作らず、読取は読取、実行はクライアントが名付けたジョブなので、再試行された insert は `409 duplicate` を受けてそのジョブで続行する。

## `internal/tools`

各ツールは `mcpserver.Tool` の記述子と `(d *deps)` ハンドラで、`tools.go` で登録、`tools_test.go` で偽 BigQuery（`httptest`）に対してカバーし、`get_usage` に載る。

| ツール | 読む | 書く |
|---|---|---|
| `list_datasets` | `datasets.list` | — |
| `list_tables` | `tables.list` | — |
| `describe_table` | `tables.get` | — |
| `dry_run` | `bq.DryRun` + ゲート判定の描画 | — |
| `query` | `bq.DryRun` → ゲート → `bq.Query` | BigQuery のクエリジョブ |
| `get_usage` | config | — |

`shape.go` は復号済みの行を両キャップ（ADR-0003）のもとで応答にし、計上フィールドを作る。`gate.go` は 3 つの検査とそのエラーコード、課金推定（MiB 切り上げ、テーブルごと 10 MiB）を持つ。`warnings.go` は数値だけから助言を作る: 推定がサイズ全体に達したパーティションテーブル（`tables.get` で引く。呼び出しごと最大 5 つ）と、精度が `PRECISE` でない推定。`util.go` は JSON スカラー（型推定）または明示的な `{type, value}` からクエリパラメータを組み立てる。

## `doctor`

3 つの検査を順に行い、それぞれ直し方を名指しする: ADC が取得できるか（`auth_error` と `gcloud auth application-default login` のヒント）、billing プロジェクトに到達できるか（`maxResults=1` の `datasets.list`）、ジョブに十分な IAM か（`SELECT 1` の dry run）。`doctor` は MCP チャネルではないので出力は stdout の平文である。

## テスト

- 単体: config の解析と検証、型行列にわたる行の復号、記録したエラー本文に対するエラー写像、ゲート判定、各キャップ下の整形、再試行の回数。
- 偽サーバー: エンドポイントごとに応答を台本化した `httptest` の BigQuery。ツールテストが `mcpserver` を通して端から端まで使う（JSON-RPC 入力、JSON 出力）。
- live（`-tags live`）: `BIGQUERY_MCP_TEST_PROJECT` が指す実プロジェクトに対する 6 ツール。両 dry run 経路と、意図的に予算超過のクエリを含む。プロジェクト ID をコミットすることはない。
