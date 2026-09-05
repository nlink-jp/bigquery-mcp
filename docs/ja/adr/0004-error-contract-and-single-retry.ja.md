# ADR-0004: エラー契約 `{code, message, retryable, details}` と 1 回の再試行

| 項目 | 内容 |
|-------|-------|
| ステータス | **採択** |
| 日付 | 2026-09-06 |
| 拘束対象 | bigquery-mcp |
| 決定者 | nlink-jp maintainers |
| 契機 | セッション 4d6bb685（2026-09-05）: 公式リモート MCP が正しい呼び出しに 3 分間「Required parameter is missing: query」を返した。文字列に原因も再試行の手がかりも無く、エージェントは自分のランタイムを 56 ラウンド探った |

## 背景

ツールエラーを読むエージェントには、公式サーバーが与えなかった 3 つが要る: 誰の落ち度か（引数、権限、サーバー）、もう一度試して助けになるか、何をすれば通るか。BigQuery 自身は第一の問いに `error.errors[].reason` — `invalidQuery`・`accessDenied`・`notFound`・`rateLimitExceeded`・`quotaExceeded`・`backendError`・`internalError`・`resourcesExceeded`・`timeout` 他 — で答え、構文エラーには `location` を添える。MCP フロントエンドはこれを一文に潰していた。

組織の MCP サーバーは既に `{code, message, details}` を返す（data-toolbox-mcp `internal/toolerr`）。本サーバーはそこに再試行の次元を足す。事象で欠けていたのはそれである。

## 決定

1. **全ツールエラーは `isError` 結果の text content に載る 1 つの JSON オブジェクト:** `code`（下表の安定したスラグ）、`message`（何を変えるかを 1〜2 文で）、`retryable`（常に存在、省略しない）、`details`（機械可読な文脈）。
2. **コード表**と BigQuery reason からの写像:

   | code | 由来 | retryable |
   |---|---|---|
   | `invalid_query` | クエリ呼び出しでの `invalidQuery`・`invalid`; `details.location` と `details.reason` | no |
   | `statement_not_allowed` | ゲート（ADR-0002）; `details.statement_type` | no |
   | `dataset_not_allowed` | ゲート; `details.table`、`details.allowed` | no |
   | `budget_exceeded` | ゲート; `details.bytes_processed`、`details.budget_bytes` | no |
   | `access_denied` | `accessDenied`、HTTP 403; message は通常欠けているロールを名指しする | no |
   | `not_found` | `notFound`、HTTP 404; `details.resource` | no |
   | `rate_limited` | `rateLimitExceeded`、分単位または同時実行の `quotaExceeded`、HTTP 429 | yes |
   | `backend_error` | `backendError`、`internalError`、HTTP 5xx | yes |
   | `timeout` | ジョブの `jobTimeoutMs`、または結果待ち; `details.job_id` | no |
   | `auth_error` | ADC が無い、トークンを取得・更新できない、HTTP 401 | no |
   | `invalid_arguments` | 本サーバーの引数検証 | no |
   | `upstream_error` | 上のどれにも写像できない HTTP/転送の失敗; `details.status` | no |

   日次 quota（「per day」を含む `quotaExceeded`）は再試行**不可**。表は `retryable: false` の `rate_limited` に写像し、message はリセット時刻を言う。
3. **再試行はサーバー内で 1 回、retryable なコードに限り**、0.5〜1.5 秒のジッタ付き遅延の後に、送信時点で冪等なリクエスト — dry run、メタデータ読取、ジョブ ID をまだ返していない `jobs.query` — に対してだけ行う。2 回目の失敗は `retryable: true` を付けてモデルに返し、モデルが決める。本サーバーは決してループしない。
4. **`details` にはモデルが行動に要るものを常に含める**: ジョブがあれば `job_id` と `location`、BigQuery の `reason` をそのまま、`budget_exceeded` では推定値と予算、`dataset_not_allowed` では問題のテーブルと許可リスト。
5. **ランタイム側の計数は本サーバーの仕事ではない。** 同じエラーを受け取り続けるエージェントはランタイムの関心事（gem-agent ADR-0075）。本サーバーの役割は各エラーを自己説明的にすることである。

## 帰結

- `internal/toolerr` に `Retryable bool`（常に直列化）と `AsRetryable()` を加える。`internal/bq/errors.go` が単一の写像関数を持ち、記録した BigQuery エラー本文に対する表駆動テストを備える。
- `get_usage` 文書はフリートの慣例どおり同じ表をエラー回復ガイドとして載せる。
- 事象で見たような一過性の障害は 2 秒未満で収まるなら 1 呼び出しの内側で解消し、収まらなければ正直に報告される。

## 検討した代替案

- **BigQuery のエラー本文をそのまま通す。** 棄却: 本文はモデルに不要なフィールドを含む Google API の入れ子エンベロープであり、`reason` だけでは再試行の可否が分からない。
- **サーバー内で無制限または指数的な再試行。** 棄却: サーバーの状態をモデルと操作者から隠す。5 回続けて失敗するクエリは情報である。
- **message 文字列による分類**（例: 「quota」の検索）。BigQuery が構造化フィールドを与えない場合を除き棄却: `quotaExceeded` の日次と分単位の区別が文書化された唯一の例外で、reason と固定句で判定しテストする。

## 参照

- RFP §2 エラー契約、§3.7、§3.9
- data-toolbox-mcp `internal/toolerr`（フリートの構造化エラーの先例）
- BigQuery エラー表: https://cloud.google.com/bigquery/docs/error-messages
- gem-agent ADR-0075（提案）: ランタイム側の障害カウンタ
