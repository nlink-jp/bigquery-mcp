# AGENTS.md — bigquery-mcp

## Project summary

Protection-first BigQuery MCP server (stdio, Go, single binary). Every
`query` is dry-run first; the gate refuses anything that is not a single
SELECT, anything outside the dataset allowlist, and anything above the byte
budget; the run is a client-named job with `maximumBytesBilled` and
`jobTimeoutMs`; rows come back as column-keyed objects under two accounted
caps; errors are `{code, message, retryable, details}`. Credentials are
Application Default Credentials; the config holds none.

One instance = one billing project (config-path switching, no profiles).
RFP: `docs/ja/bigquery-mcp-rfp.ja.md`. Module path:
`github.com/nlink-jp/bigquery-mcp`.

## Build & test

```bash
make build       # → dist/bigquery-mcp (signed on macOS); never `go build` directly
make test        # go test ./... — unit + fake BigQuery (httptest); no credentials
make vet
make check       # vet + test + build
make live-test   # -tags live against a real project; needs ADC and
                 # BIGQUERY_MCP_TEST_PROJECT (+ BIGQUERY_MCP_TEST_DATASET)
make package     # cross-compile, archive, notarize darwin
make verify-release
```

`--version` and `version` print the same string (the homebrew formula
tests `--version`); the value is injected via
`-ldflags -X github.com/nlink-jp/bigquery-mcp/cmd.Version`.

## Structure

```
main.go                 Entry point — cmd.Execute()
cmd/
  root.go               Cobra root; default action = serve; --config
  serve.go              Config resolution, wiring, stdio serve loop
  doctor.go             ADC → project reach → jobs IAM, names the failing step
  version.go            version subcommand + --version
internal/
  transport/            Newline-delimited JSON-RPC stdio (8 MiB lines)  [ported: data-toolbox-mcp]
  jsonrpc/              JSON-RPC 2.0 types + codes                       [ported]
  mcpserver/            MCP 2024-11-05 routing, RegisterTool, RawResult  [ported]
  toolerr/              {code,message,retryable,details} + Codes list    [ported, codes swapped]
  logging/              slog + startup log rotation                      [ported]
  config/               Sectioned TOML, strict keys, sizes/durations
  bq/                   REST v2 client: ADC, dry run (jobs.insert), named
                        query jobs, getQueryResults paging, jobs.get stats,
                        reason table, single retry, row decoding
  tools/                The 6 tools: gate.go / shape.go / warnings.go /
                        query.go (dry_run + query) / metadata.go / get_usage.go
config.example.toml     Template config (one file per billing project)
docs/{en,ja}/           RFP, ADR 0001–0004, architecture
scripts/                Org release scripts (codesign / notarize / brew) — vendored verbatim
```

## Key decisions & gotchas

- **stdout is the JSON-RPC channel.** Nothing else may write to it;
  diagnostics go through slog (stderr / log file). `doctor` prints to
  stdout because it is not the MCP channel.
- **The gate is inside `query`** (ADR-0002): dry run → statementType ==
  SELECT → allowlist (tables and routines) → billed estimate ≤ budget.
  No argument skips it; `dry_run` runs the same code and renders the
  verdict as data (`allowed`, `denied_by`).
- **The dry run always uses `jobs.insert`** — it is the only call that
  reports `referencedTables`/`referencedRoutines`; `jobs.query`'s dryRun
  is not cheaper (one request either way).
- **Queries run as client-named jobs** (`bqmcp-<32 hex>`, ADR-0004 §3).
  A retried insert answered `409 duplicate` continues with the existing
  job — never two runs, never two bills. Do not switch back to
  `jobs.query`: its `requestId` deduplicates only mutating queries.
- **Statistics come from `jobs.get`**: `getQueryResults` carries no
  `totalBytesBilled` / `totalSlotMs` / `statementType` / `cacheHit`.
- **One reason table** (`internal/bq/errors.go` `reasonRules`) and one
  documented code list (`toolerr.Codes`); `get_usage` renders the list and
  `TestEveryProducedCodeIsDocumented` keeps them in sync. Reason first,
  HTTP status only as fallback (BigQuery answers 403 for quota errors too).
  The daily-quota phrase is the single text rule.
- **No server-side spill** (ADR-0003): everything is in the response;
  `max_rows` and `max_bytes` stop it with `truncated`/`truncated_by`/
  `total_rows`. The runtime files large results, not this server.
- **Warnings never read SQL text**: a partitioned table is flagged when the
  estimate covers its whole size; an imprecise estimate is flagged by
  `totalBytesProcessedAccuracy`.
- **Timestamps** are requested as `ISO8601_STRING` and passed through
  (picosecond precision); int64 microseconds and float seconds are decoded
  only as fallbacks.
- **Budget floor**: config refuses `max_bytes_billed` under 10 MiB
  (BigQuery bills 10 MB per table at minimum); the gate compares the billed
  estimate (MiB rounding + per-table minimum), not the raw bytes.
- **ADC scopes are never narrowed** — the file is shared by every tool on
  the machine; the README says so. Service-account ADC gets the `bigquery`
  scope. `cloud-platform.read-only` cannot use this server (`jobs.insert`).
- **Config**: unknown keys, sizes below the floor, invalid log levels and
  partial wildcards are errors; a named `--config` path must exist; the
  default path may be absent. Domain-scoped project ids (`example.com:p`)
  split at the last dot.
- Transport line limit is 8 MiB so BigQuery's 1 MB SQL limit is reported
  as `invalid_query`, not by the scanner.
- Live tests never carry a project id; they read it from the environment
  and use `bigquery-public-data.samples.wikipedia` for the refusal cases
  (dry runs are free; the gate refuses before anything runs).
- **Every tool's top-level schema is closed** (`additionalProperties:
  false`, organization ADR-021 §10), and both halves of the contract are
  real here: the schema stops a mistyped argument at a validating client,
  and `parseArgs`' `DisallowUnknownFields` stops it at the server
  (`TestUnknownArgumentIsRejected` drives that end to end and checks the
  error names the field). The schemas are six separate JSON literals with no
  shared builder, so a new tool must add the key by hand —
  `TestEveryToolSchemaIsClosed` (`internal/tools/schema_test.go`) catches
  the omission, reading the schemas off a real `tools/list` driven through
  `Register`.
- **`params` is the one object that must stay open.** Its keys are the
  caller's own named query parameters (`@name` in the SQL), so no schema can
  enumerate them and `additionalProperties: true` is what makes them legal.
  Closing it would reject every parameterised query. Only the *top level* of
  each tool schema is closed; `TestParamsObjectStaysOpen` guards the nested
  exception against a future sweep that closes everything it finds.

## Release

Follow the org release process (CONVENTIONS.md). Before release: `make
live-test` against a real project is mandatory (RFP Phase 3).
