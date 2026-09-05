# bigquery-mcp architecture

This document explains **why** the pieces are shaped as they are. The RFP
(`bigquery-mcp-rfp.md`) records the decisions; the ADRs record the
reasoning behind the non-obvious ones. Read this before changing the
execution path of `query`.

## The one path a query takes

```
model ──tools/call query──▶ tools.query
                              │ parse + validate args (max_rows ≤ hard_max_rows)
                              ▼
                         bq.DryRun ──▶ jobs.insert{dryRun}  (statementType, bytes, referencedTables, schema)
                              │
                              ▼  gate, in order (ADR-0002)
                         statementType == SELECT ?      ──✗ statement_not_allowed
                         referenced tables ⊆ allowlist ? ──✗ dataset_not_allowed
                         bytes ≤ max_bytes_billed ?      ──✗ budget_exceeded
                              │
                              ▼
                         bq.Query ──▶ jobs.insert{jobId=bqmcp-…, maximumBytesBilled, jobTimeoutMs, labels}
                                   ├─▶ jobs.getQueryResults{location, timeoutMs} … until jobComplete
                                   ├─▶ jobs.get{location}  (bytes billed, slot ms, cache hit, statement type)
                                   └─▶ jobs.getQueryResults{location, pageToken, maxResults} …
                              │
                              ▼
                         shape rows (ADR-0003): objects keyed by column, stop at
                         max_rows or max_bytes, account truncation
                              │
                              ▼
                         one JSON text block  ──or── one toolerr JSON (ADR-0004)
```

Nothing in the tool layer can reorder these steps; `dry_run` is the same
`bq.DryRun` plus the gate verdict rendered as data.

## Packages

| Package | Role | Origin |
|---|---|---|
| `cmd/` | cobra: `serve` (default), `doctor`, `version`, `--config` | splunk-mcp shape |
| `internal/transport` | newline-delimited JSON-RPC over stdio, 1 MB lines | ported from data-toolbox-mcp |
| `internal/jsonrpc` | JSON-RPC 2.0 types and codes | ported |
| `internal/mcpserver` | MCP 2024-11-05 routing, `RegisterTool`, `RawResult`, structured tool errors | ported |
| `internal/toolerr` | `{code, message, retryable, details}` (ADR-0004) | ported, codes swapped, `Retryable` added |
| `internal/logging` | slog to stderr and an optional rotated file | ported |
| `internal/config` | sectioned TOML, strict keys, sizes and durations parsed | new |
| `internal/bq` | REST v2 client over `net/http`, ADC token source, error mapping, retry (ADR-0001, ADR-0004) | new |
| `internal/tools` | the six tools, argument parsing, the gate, row shaping (ADR-0002, ADR-0003) | new |

**stdout belongs to JSON-RPC.** No package may print to it; diagnostics
go through slog.

## `internal/bq` — what is read and written

REST base: `https://bigquery.googleapis.com/bigquery/v2`. Every request
carries `Authorization: Bearer` from `google.DefaultTokenSource(ctx,
"https://www.googleapis.com/auth/bigquery")` and a `User-Agent` naming this
server and its version.

| Call | Request fields used | Response fields read |
|---|---|---|
| `POST projects/{p}/jobs` (dry run) | `configuration.dryRun=true`, `configuration.query.{query,useLegacySql=false,parameterMode,queryParameters}`, `configuration.labels`, `jobReference.location?` | `jobReference.location`, `statistics.query.{statementType,totalBytesProcessed,totalBytesProcessedAccuracy,referencedTables,referencedRoutines,undeclaredQueryParameters,schema}`, `status.errorResult` |
| `POST projects/{p}/jobs` (run) | `jobReference.{projectId,jobId=bqmcp-<32 hex>,location?}`, `configuration.{jobTimeoutMs,labels}`, `configuration.query.{query,useLegacySql=false,maximumBytesBilled,useQueryCache=true,priority=INTERACTIVE,parameterMode,queryParameters}` | `jobReference.location`, `status.errorResult`; a `409 duplicate` on the retry means the first insert landed |
| `GET projects/{p}/queries/{jobId}` | `location`, `timeoutMs=30000`, `maxResults`, `pageToken`, `formatOptions.timestampOutputFormat=ISO8601_STRING` | `jobComplete`, `jobReference`, `schema`, `rows`, `totalRows`, `pageToken`, `totalBytesProcessed`, `errors` |
| `GET projects/{p}/jobs/{jobId}` | `location` | `statistics.query.{statementType,totalBytesBilled,totalSlotMs,cacheHit,totalBytesProcessed}`, `statistics.totalSlotMs` |
| `GET projects/{p}/datasets` | `pageToken`, `maxResults` | `datasets[].{datasetReference,location}`, `nextPageToken` |
| `GET projects/{p}/datasets/{d}/tables` | `pageToken`, `maxResults` | `tables[].{tableReference,type,timePartitioning,clustering}`, `nextPageToken` |
| `GET projects/{p}/datasets/{d}/tables/{t}` | — | `schema`, `type`, `numRows`, `numBytes`, `timePartitioning`, `rangePartitioning`, `clustering`, `creationTime`, `lastModifiedTime`, `expirationTime`, `description` |

`location` is carried from the first response into every later call for
the same job (regional datasets require it).

Row decoding: BigQuery returns `rows[].f[].v` in schema order, with nested
records as further `{f:[...]}` and repeated fields as `[{v:...}]`. The
decoder walks the schema recursively to produce column-keyed objects:
`RECORD` → object, `REPEATED` → array, `BYTES` stays base64, `TIMESTAMP`
is requested as an ISO 8601 UTC string (`ISO8601_STRING`, up to
picosecond precision) and passed through — int64 microseconds and float
seconds are decoded only as fallbacks — `NUMERIC`/`BIGNUMERIC` → string,
`INTEGER` → number when it fits in 53 bits else string, `BOOLEAN` → bool,
`JSON` → embedded JSON.

Error mapping (`errors.go`): `error.errors[0].reason` decides the code
through the one `reasonRules` table, the HTTP status only as a fallback
(ADR-0004 §2); `message`, `location`, `reason`, `job_id` ride in
`details`. Retry: one attempt after 500–1500 ms for retryable codes.
Every call is idempotent as sent — dry runs create no job, reads are
reads, and the run is a job the client named, so a retried insert is
answered `409 duplicate` and the client continues with that job.

## `internal/tools`

Each tool is a `mcpserver.Tool` descriptor plus a `(d *deps)` handler,
registered in `tools.go`, covered in `tools_test.go` against a fake
BigQuery (`httptest`), and listed in `get_usage`.

| Tool | Reads | Writes |
|---|---|---|
| `list_datasets` | `datasets.list` | — |
| `list_tables` | `tables.list` | — |
| `describe_table` | `tables.get` | — |
| `dry_run` | `bq.DryRun` + gate verdict rendered | — |
| `query` | `bq.DryRun` → gate → `bq.Query` | a BigQuery query job |
| `get_usage` | config | — |

`shape.go` turns decoded rows into the response under both caps (ADR-0003)
and produces the accounting fields. `gate.go` holds the three checks with
their error codes and the billed estimate (MiB rounding, 10 MiB per
table). `warnings.go` produces advice from numbers only: a partitioned
referenced table whose size the estimate covers (looked up through
`tables.get`, at most five per call), and an estimate whose accuracy is
not `PRECISE`. `util.go` builds query parameters from either JSON scalars
(type inferred) or explicit `{type, value}` objects.

## `doctor`

Three checks in order, each naming the fix: ADC obtainable (`auth_error`
with the `gcloud auth application-default login` hint), billing project
reachable (`datasets.list` with `maxResults=1`), IAM sufficient for jobs
(`SELECT 1` dry run). Output is plain text on stdout because `doctor` is
not the MCP channel.

## Tests

- Unit: config parsing and validation; row decoding across the type
  matrix; error mapping over recorded error bodies; gate decisions; shaping
  under each cap; retry counting.
- Fake server: an `httptest` BigQuery that scripts responses per endpoint,
  used by the tool tests end to end through `mcpserver` (JSON-RPC in, JSON
  out).
- Live (`-tags live`): the six tools against a real project named by
  `BIGQUERY_MCP_TEST_PROJECT`, including both dry-run paths and a
  deliberately over-budget query. Never committed with a project id.
