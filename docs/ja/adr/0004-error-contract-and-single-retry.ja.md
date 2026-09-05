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
2. **reason 表は 1 つ。** `internal/bq/errors.go` が BigQuery の `reason` から `{code, retryable, hint}` への唯一の写像を持ち、`toolerr.Codes` がコードの唯一の文書化された一覧を持つ。`get_usage` はその一覧を描画し、テストは写像が生成しうる全コードがそこにあることを検査する。判定は reason が先で、HTTP ステータスは補助に過ぎない。BigQuery は IAM 不足と同様に quota やレート制限にも 403 を返すからである。表:

   | code | 由来 | retryable |
   |---|---|---|
   | `invalid_query` | `invalidQuery`、`invalid`、`invalidQueryParameter`; `resourcesExceeded`、`responseTooLarge`、`billingTierLimitExceeded`（「仕事を減らせ」のヒント付き）; 既知の reason を持たない 400。`details.location` と `details.reason` | no |
   | `statement_not_allowed` | ゲート（ADR-0002）; `details.statement_type` | no |
   | `dataset_not_allowed` | ゲート; `details.table` または `details.routine`、`details.allowed` | no |
   | `budget_exceeded` | ゲート（`details.bytes_processed`、`bytes_billed_estimate`、`budget_bytes`、`accuracy`）、または推定が通った後にカーネル上限がジョブを止めた BigQuery の `bytesBilledLimitExceeded`（固有のヒント） | no |
   | `access_denied` | `accessDenied`、`userNotAuthorized`（IAM ヒント）; `billingNotEnabled`（課金ヒント）; `accessNotConfigured`（API 未有効化ヒント）; 既知の reason を持たない 403 | no |
   | `not_found` | `notFound`、`tableNotFound`、`datasetNotFound`、HTTP 404; `details.resource` | no |
   | `rate_limited` | `rateLimitExceeded`、`quotaExceeded`、`concurrentQueryLimitExceeded`、HTTP 429 | yes。ただし message が per day を言えば no |
   | `backend_error` | `backendError`、`internalError`、`jobBackendError`、`jobInternalError`、`unavailable`、501 を除く HTTP 5xx | yes |
   | `timeout` | `timeout`、`jobTimeout`、message が「timed out」を言う `stopped`、HTTP 408/504、クライアント自身の期限; `details.job_id` | no |
   | `cancelled` | それ以外の理由の `stopped`（コンソール、`bq cancel`） | no |
   | `auth_error` | ADC が無い、トークンを取得・更新できない; `authError`、`unauthorized`、HTTP 401 | no |
   | `duplicate` | `duplicate`、HTTP 409 — 再試行時にクライアント内で処理され、ツールに達した場合のみ表面化 | no |
   | `invalid_arguments` | 本サーバーの引数検証 | no |
   | `upstream_error` | `notImplemented`/501、転送失敗、上のどれにも写像できないステータス; `details.http_status` | 転送失敗は yes、他は no |

   日次 quota（message が「per day」か「daily」を言う `quotaExceeded`）が写像の中で唯一の文字列規則である: BigQuery は構造化フィールドを与えず、句は固定で、テストされている。

3. **再試行はサーバー内で 1 回、retryable なコードに限り**、0.5〜1.5 秒のジッタ付き遅延の後に行う。本クライアントが送る全リクエストは送信時点で冪等なので、再試行が仕事を重複させることはない: dry run はジョブを作らず、メタデータ読取と `getQueryResults` は読取であり、**クエリ自体はクライアントが名付けたジョブとして走る**（`jobReference.jobId = bqmcp-<32 hex>` の `jobs.insert`）。最初の insert が BigQuery に届いて応答だけが失われた場合、再試行は `409 duplicate` を受け、クライアントは存在するジョブで続行する — クエリが 2 回走ることも 2 回課金されることもない。2 回目の失敗は `retryable: true` を付けてモデルに返し、モデルが決める。本サーバーは決してループしない。

4. **`details` にはモデルが行動に要るものを常に含める**: ジョブがあれば `job_id` と `location`、BigQuery の `reason` をそのまま、`budget_exceeded` では推定値と予算、`dataset_not_allowed` では問題のテーブルと許可リスト。
5. **ランタイム側の計数は本サーバーの仕事ではない。** 同じエラーを受け取り続けるエージェントはランタイムの関心事（gem-agent ADR-0075）。本サーバーの役割は各エラーを自己説明的にすることである。

## 独立設計レビュー（2026-09-06）— 変えたこと

- **採用 — 再試行が二重課金しうる。** `requestId` 付き `jobs.query` の重複排除は変更系クエリにしか効かず、リクエストが BigQuery に届いた後の読取タイムアウトは SELECT を 2 回走らせた。`jobs.insert` によるクライアント命名ジョブへ置換（§3）。Phase 2 の `cancel_job` にも取っ手を与える。
- **採用 — 統計は Job 上にある。** `getQueryResults` は `totalBytesBilled`・`totalSlotMs`・`statementType`・`cacheHit` を運ばない。完了後に `jobs.get` で読む。
- **採用 — 表は 1 つ。** RFP・本記録・コードが 3 つの表に乖離していた。コードの `reasonRules` と `toolerr.Codes` を正とし、上の表はそこから書いた。
- **採用 — status より reason 先**; `stopped` は `timeout` と `cancelled` に分岐; 501 は再試行不可; `billingNotEnabled` と `accessNotConfigured` は IAM ヒントでなく固有ヒント。
- **採用 — タイムスタンプは ISO 8601 文字列。** `formatOptions.timestampOutputFormat=ISO8601_STRING` は数値表現には無いピコ秒精度を運ぶ。クライアントは文字列をそのまま通す（architecture）。

## 帰結

- `internal/toolerr` に `Retryable bool`（常に直列化）、`AsRetryable()`、文書化された `Codes` 一覧を加える。`internal/bq/errors.go` が reason 写像を持ち、記録した BigQuery エラー本文に対する表駆動テストと、生成しうる全コードが文書化されていることのテストを備える。
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
