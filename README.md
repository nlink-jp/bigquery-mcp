# bigquery-mcp

Protection-first BigQuery MCP server. It runs locally over stdio with your
own Application Default Credentials, dry-runs every query before it runs,
refuses anything that is not a single SELECT inside the budget, returns
rows as column-keyed objects with explicit caps, and reports every failure
as `{code, message, retryable, details}` so an agent knows whose fault it
is and what to change.

It calls no LLM: the agent writes the SQL.

## Why not the official remote MCP server

BigQuery's own remote MCP endpoint is generic. It has no dry-run budget
gate, its read-only tool inspects SQL text, it returns raw `{f:[{v:…}]}`
rows, its errors are single sentences with no cause or retry hint, and
stdio clients have to reach it through a bridge. bigquery-mcp exists for
agents analysing an enterprise data warehouse, where the operator wants a
cap on what a query may cost, a boundary on what it may read, and errors
an agent can act on instead of investigating its own runtime.

## Install

Download the archive for your platform from the releases page, unpack it,
and put `bigquery-mcp` on your `PATH`. macOS builds are signed and
notarized.

Building from source:

```bash
make build      # → dist/bigquery-mcp (never `go build` directly)
```

## Setup

Three steps; `bigquery-mcp doctor` checks them in order and names the
first one that fails.

1. **Credentials** — Application Default Credentials:

   ```bash
   gcloud auth application-default login
   ```

   ADC is one file shared by every tool on your machine (gem-agent's
   Vertex AI calls included). Do not narrow its scopes for this server;
   privilege reduction is done with IAM below.

2. **IAM** — the roles the credential needs, and nothing that can write:

   | Where | Role | Why |
   |---|---|---|
   | the billing project | `roles/bigquery.jobUser` | create query jobs |
   | the data projects or datasets | `roles/bigquery.dataViewer` | read tables |
   | discovery-only projects | `roles/bigquery.metadataViewer` | list and describe |

   Withholding `dataEditor` and above is the permanent read-only boundary;
   the server's own SELECT check is the second line, not the only one.

3. **Config** — copy [config.example.toml](config.example.toml) to
   `~/.config/bigquery-mcp/config.toml` and set the billing project:

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

   Config resolution: `--config` → `$BIGQUERY_MCP_CONFIG` →
   `~/.config/bigquery-mcp/config.toml`. Unknown keys are an error. The
   file holds no credentials.

**One instance = one billing project.** For several, keep one config each
and register the server under several names:

```json
{
  "mcpServers": {
    "bigquery-prod":    { "command": "bigquery-mcp", "args": ["--config", "/path/to/prod.toml"] },
    "bigquery-sandbox": { "command": "bigquery-mcp", "args": ["--config", "/path/to/sandbox.toml"] }
  }
}
```

That block works as-is for Claude Desktop, Claude Code (`.mcp.json` or
`~/.claude.json`) and gem-agent (`~/.config/gem-agent/mcp.json` or the
project's `.mcp.json`).

## Tools

| Tool | Purpose |
|---|---|
| `list_datasets` | Datasets of a project (the billing project by default); `project` is optional and bills nothing |
| `list_tables` | Tables and views of a dataset, with partition column |
| `describe_table` | Schema (nested), partition column and granularity, clustering, rows, size, expiration, view SQL |
| `dry_run` | Bytes (raw and billed estimate), accuracy, statement type, referenced tables and routines, undeclared parameters, result schema, the gate's verdict, warnings |
| `query` | Dry run → gate → run; rows as column-keyed objects under the caps |
| `get_usage` | The full reference with the error-recovery table |

### What `query` does, every time

1. **Dry run** through `jobs.insert` — statement type, byte estimate,
   referenced tables and routines.
2. **Gate**, in order; nothing runs when a check fails:
   - `statement_not_allowed` unless BigQuery classified the statement as
     `SELECT` (scripts, DML, DDL, EXPORT, CALL and ASSERT all fail this);
   - `dataset_not_allowed` when `[access] datasets` is set and a referenced
     table or routine lies outside it;
   - `budget_exceeded` when the billed estimate (bytes rounded up to MiB,
     10 MiB minimum per table) is above `[budget] max_bytes_billed`.
3. **Run** as a job this server names, with `maximumBytesBilled`,
   `jobTimeoutMs` and the label `bigquery-mcp: true`, so billing can be
   attributed and a stopped job is reported with its cause.
4. **Shape**: rows are objects keyed by column name. Nested records are
   objects, repeated fields arrays, TIMESTAMP an ISO 8601 UTC string,
   NUMERIC and integers beyond 53 bits strings.

Results stop at `max_rows` (per call; default and ceiling from the
config) or at the response byte budget `max_bytes`. Either sets
`truncated: true`, names the cap in `truncated_by`, and reports
`total_rows` from BigQuery — nothing is dropped silently. The server never
writes files; turning a large result into one is the agent runtime's job.

Parameters: `"params": {"n": 3, "s": "x", "day": {"type": "DATE", "value": "2026-09-05"}}`.
Strings, numbers and booleans are typed by inference; other scalar types
use the explicit form. Use literals, not parameters, in partition filters —
a parameterised filter may not prune at dry-run time.

### Errors

Every error is one JSON object with a stable `code`, a `message` that says
what to change, `retryable` (always present; when true the server already
retried once), and `details` (BigQuery's reason and location, the job id,
the estimate and the budget). The full table is in `get_usage`.

## Notes

- `maximumBytesBilled` only applies under on-demand pricing. Under
  Editions the gate rests on the dry-run estimate alone.
- A SELECT can still spend money outside the byte budget through remote
  functions and `ML.` / `AI.` functions; IAM on connections and models is
  the boundary there.
- `[project] location` is normally left unset; BigQuery infers it from the
  datasets, and a fixed location makes queries against other regions fail
  with `not_found`.
- Hidden datasets (names starting with `_`) are not listed.
- Long queries: the default job timeout is 3 minutes. Asynchronous jobs
  are planned for a later release.

## Development

```bash
make test        # unit + fake-server tests (no credentials)
make vet
make check       # vet + test + build
make live-test   # -tags live against a real project:
                 #   BIGQUERY_MCP_TEST_PROJECT=<billing project> BIGQUERY_MCP_TEST_DATASET=<dataset> make live-test
```

Design records: [RFP](docs/en/bigquery-mcp-rfp.md), [ADRs](docs/en/adr/),
[architecture](docs/en/architecture.md).

## License

MIT
