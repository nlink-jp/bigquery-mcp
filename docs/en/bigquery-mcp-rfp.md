# RFP: bigquery-mcp

> Generated: 2026-09-06
> Status: Approved (2026-09-06)

## 1. Problem Statement

When an agent (Claude Code, gem-agent, Claude Desktop) analyses an enterprise BigQuery data warehouse agentically, the official remote MCP server (`bigquery.googleapis.com/mcp`) falls short in three ways.

1. **Weak protection.** No dry-run budget preview, no bytes-billed cap, and read-only enforcement that rests on inspecting SQL text.
2. **Errors that lead nowhere.** In session 4d6bb685 (2026-09-05) it transiently answered correct calls with "Required parameter is missing: query" — a string with no cause and no retry hint — and the agent drifted into debugging its own runtime.
3. **A bridge in the path.** stdio clients reach it through mcp-bridge, adding token renewal, a hop, and a failure point.

bigquery-mcp is a **protection-first, BigQuery-only MCP server** that runs locally over stdio with the user's own ADC (Application Default Credentials), fixes the budget gate and the read-only check inside the execution path, and maps BigQuery API error reasons to `{code, message, retryable, details}`. SQL is written by the calling agent; the server calls no LLM.

**Target user**: people who already hold gcloud and BigQuery IAM — the maintainer (nlink-jp's DWH) and enterprise analysts outside the organisation. Setup is three steps: `gcloud auth application-default login`, existing IAM roles, and a billing project id in the config. No per-user OAuth client, no API enablement. Users without gcloud are out of scope.

## 2. Functional Specification

### Commands / API Surface

Single binary with subcommands.

| Subcommand | Behaviour |
|---|---|
| `serve` (default) | Serve MCP over stdio |
| `doctor` | Check, in order: ADC present → billing project reachable → IAM via a `SELECT 1` dry run; name the failing step |
| `version` | Print the version |

MCP tools (v1, six):

| Tool | Arguments | Behaviour |
|---|---|---|
| `list_datasets` | `project?` | Dataset list; the billing project when omitted |
| `list_tables` | `dataset`, `project?` | Tables and views, with kind |
| `describe_table` | `dataset`, `table`, `project?` | Schema (nested), partition column and type, clustering, rows, bytes, partition expiration, created/modified |
| `dry_run` | `query`, `params?` | Bytes, statementType, referenced tables, result schema, budget verdict (`within_budget`), a warning when a partition column has no filter |
| `query` | `query`, `params?`, `max_rows?` | **Always dry-runs internally** → gate (statementType is `SELECT`, within the allowlist, within budget) → `jobs.query` → paging → rows up to the caps |
| `get_usage` | — | Reference and error-recovery table (nlink-jp MCP standard) |

`project` on the three metadata tools is for discovery only and bills nothing; IAM is the boundary.

Phase 2 tools:

| Tool | Behaviour |
|---|---|
| `start_query` / `check_job` / `get_results` / `cancel_job` | Asynchronous path — `jobs.insert` → `jobs.get` polling → `getQueryResults` — for queries longer than three minutes |

### Input / Output

**`query` result** (JSON):

```json
{
  "rows": [ {"email": "...", "cnt": 12} ],
  "schema": [ {"name": "email", "type": "STRING", "mode": "NULLABLE"} ],
  "total_rows": 4813,
  "returned_rows": 1000,
  "truncated": true,
  "truncated_by": "max_rows",
  "bytes_processed": 123456789,
  "bytes_billed": 130023424,
  "cache_hit": false,
  "job_id": "...",
  "location": "US",
  "slot_ms": 1234
}
```

- Rows are **objects keyed by column name**; the official server's `{"f":[{"v":…}]}` shape is not used. Nested records are nested JSON, `REPEATED` is an array, `BYTES` is base64, `TIMESTAMP` is RFC 3339, `NUMERIC` types are strings.
- Everything is returned in the response. The server never spills to a workspace file; turning a result into a file is the agent runtime's job (gem-agent's ADR-0058 intake saves oversized results to its work directory).
- Two caps: `max_rows` (call argument; config holds the default and the ceiling) and `[results] max_bytes` (a byte budget for the whole response). Hitting either cuts rows and always reports `truncated: true`, `truncated_by` and `total_rows`. There is no silent path.

**`dry_run` result**:

```json
{
  "bytes_processed": 123456789,
  "statement_type": "SELECT",
  "referenced_tables": ["proj.dataset.table"],
  "schema": [ ... ],
  "within_budget": true,
  "budget_bytes": 10737418240,
  "warnings": ["table proj.dataset.events is partitioned by event_date but the query has no filter on it"]
}
```

**Error contract** (JSON in the text content, `isError: true`):

```json
{"code": "budget_exceeded", "message": "...", "retryable": false,
 "details": {"reason": "...", "location": "...", "job_id": "...", "bytes_processed": 0, "budget_bytes": 0}}
```

| code | Source | retryable |
|---|---|---|
| `invalid_query` | BigQuery `invalidQuery` (syntax, unknown column, SQL over 1 MB; `details.location` carries the position) | no |
| `statement_not_allowed` | dry-run statementType other than `SELECT` | no |
| `dataset_not_allowed` | a referenced table outside `[access] datasets` | no |
| `budget_exceeded` | dry-run bytes above `[budget] max_bytes_billed` (stopped before running) | no |
| `access_denied` | `accessDenied` (missing IAM; the message names the role) | no |
| `not_found` | `notFound` (project, dataset, table) | no |
| `rate_limited` | transient `rateLimitExceeded` / `quotaExceeded` | yes |
| `backend_error` | `backendError` / `internalError` | yes |
| `timeout` | `jobTimeoutMs` exceeded, or the `getQueryResults` wait | no |
| `auth_error` | ADC missing, expired, or not renewable (points to `gcloud auth application-default login`) | no |
| `invalid_arguments` | argument type, missing argument, `max_rows` above the ceiling | no |

A `retryable: true` error is retried once inside the server, with jitter; a second failure goes to the model.

### Configuration

Sectioned TOML at `~/.config/bigquery-mcp/config.toml`. Resolution: `--config` → `$BIGQUERY_MCP_CONFIG` → the default path.

```toml
[project]
id = "billing-project"          # where jobs run and are billed (required)
# location = "US"               # BigQuery infers it from the datasets when omitted

[budget]
max_bytes_billed = "10GiB"       # the dry-run gate's threshold, passed as maximumBytesBilled as well
job_timeout      = "3m"          # jobTimeoutMs

[access]
# datasets = ["proj.dataset", "proj2.*"]   # enforced through jobs.insert(dryRun) referencedTables only when set

[results]
default_max_rows = 1000          # when the call omits max_rows
hard_max_rows    = 50000         # the most a call may ask for
max_bytes        = "1MiB"        # byte budget for the whole response

[logging]
# log_file    = ""               # stderr when omitted
# log_level   = "info"
# log_queries = false            # opt-in: keep SQL text in the log (may contain PII)
```

**One instance = one billing project.** For several billing projects, keep one config each and register the server under several names (`bigquery-prod`, `bigquery-sandbox`). The config holds no secrets (credentials are ADC). Project ids are environment-specific and never committed.

### External Dependencies

- **BigQuery REST API v2**: `jobs.insert` (dryRun), `jobs.query`, `jobs.getQueryResults`, `datasets.list`, `datasets.get`, `tables.list`, `tables.get`; Phase 2 adds `jobs.get`, `jobs.cancel`.
- **Auth**: ADC through `golang.org/x/oauth2/google` (build list 3 modules, 27 packages linked). Tokens renew automatically.
- **Not adopted**: `cloud.google.com/go/bigquery` (build list 236 modules, 330 packages linked, pulls gRPC and protobuf). The needed structs are written by hand from the Discovery document.
- The TOML parser is the one data-toolbox-mcp uses, pinned at scaffold time.

## 3. Design Decisions

1. **Go, REST v2 direct, no SDK.** The measured dependency graphs (236 versus 3 modules) decide it; within the organisation's supply-chain policy (stdlib, first-party SDK, or direct REST) the smallest option wins. The skeleton is data-toolbox-mcp's transport / jsonrpc / mcpserver / toolerr / logging; the REST and job layer follows splunk-mcp.
2. **Protection lives inside `query`.** Dry run → gate → run cannot be skipped; the budget check runs whether or not the model called `dry_run`.
3. **Statements are classified by BigQuery's own statementType**, never by a regex over SQL. `SCRIPT`, `EXPORT_DATA`, DML and DDL all fall out as non-`SELECT`.
4. **IAM is the permanent boundary.** A user holding only `jobUser` + `dataViewer` cannot write even past the server's check; the server's check is a second line, not the only one.
5. **Kernel-side caps are `maximumBytesBilled` and `jobTimeoutMs`**; the dry-run verdict is UX. `maximumBytesBilled` only applies under on-demand pricing (bytes billed is 0 under Editions) — the README says so.
6. **Jobs carry the label `bigquery-mcp`** so billing can be attributed.
7. **One retry, retryable errors only.** Beyond that the model gets `retryable: true` and decides; repeated transparent retries would hide the server's state.
8. **No server-side spill.** Everything comes back in the response; the agent runtime turns it into a file. The server keeps only explicit caps and truncation accounting.
9. **Error contract `{code, message, retryable, details}`.** details carry BigQuery's reason, location, job id, dry-run bytes and the budget, so the model can read what would make the call pass.
10. **SQL text in the log is opt-in** (`log_queries = true`), independent of the log level. By default only job id, statementType, bytes, duration and error code are logged.
11. **ADC scopes are left alone.** ADC is one file shared by every tool on the machine; narrowing it breaks gem-agent's Vertex use. Privilege reduction is done with IAM and the server's checks; the README says not to narrow ADC. Only the service-account path requests `https://www.googleapis.com/auth/bigquery`.
12. **One instance = one billing project.** No profile mechanism, no per-call billing switch: a structure in which the model picks the billing project and the budget weakens the gate. Cross-project data access is native to BigQuery, so the `project` argument on metadata tools plus IAM suffices.
13. **Complements.** gem-agent receives results through its ADR-0058 intake; oversized results land in the work directory and can be handed to data-toolbox-mcp's `load_from_work`. The mcp-tactics skill gains a BigQuery path (it must follow fleet changes). gem-query (NL→SQL) is a different tool.
14. **Out of scope.** NL→SQL; all writes (DML, DDL, EXPORT — a protected temp-dataset write mode is reconsidered in Phase 2); proxying the official remote MCP; HTTP/SSE transport; multi-database support; legacy SQL; sessions; BI Engine; reservation management; comparison with community BigQuery MCPs.

## 4. Development Plan

### Phase 0: design documents (right after scaffold, before code)

- ADR-0001: REST v2 direct + oauth2/google, no SDK (with the measurements)
- ADR-0002: the budget gate and statement check live inside `query`; statementType and IAM decide
- ADR-0003: no server-side spill; explicit caps and truncation accounting
- ADR-0004: the error contract and the single retry
- architecture.md
- An independent verification pass (subagent) on the fixed design

### Phase 1: Core + tests

- Go scaffold in `_wip/bigquery-mcp/` (CONVENTIONS templates, `make build` → `dist/`); subcommands `serve` / `doctor` / `version`
- Port transport / jsonrpc / mcpserver / toolerr / logging from data-toolbox-mcp
- `internal/bq`: ADC, REST client, error mapping, carrying `location`
- `internal/tools`: all six tools; dry run → gate → run → paging → caps inside `query`
- `doctor`
- Tests: an `httptest` fake BigQuery pins the gate, caps, error mapping, retry and location handling; live E2E is opt-in (`-tags live`) against the Workspace audit export dataset, with the project id from an environment variable

Review unit: the fake-server suite plus the live E2E result can be judged on their own.

### Phase 2: Features

- Asynchronous tools `start_query` / `check_job` / `get_results` / `cancel_job`
- `[auth] token_command` (for environments that want a different EDR footprint; the shape of mcp-bridge's tokenCommand)
- Protected write mode limited to a temporary dataset (to be reconsidered)
- Sharper full-scan warnings (partition column × presence of a filter)

### Phase 3: Release

- README en/ja, AGENTS.md, CHANGELOG, `docs/{en,ja}/reference/client-setup.md` (Claude Desktop / Claude Code / gem-agent)
- Signing, notarization, five platform zips, tap formula, util-series submodule, org profile
- A BigQuery path in the mcp-tactics skill
- Feedback to knowledge; check-org all green
- An independent verification pass before release

## 5. Required API Scopes / Permissions

**OAuth scopes** (from the Discovery document):

| Method | Accepted scopes |
|---|---|
| `jobs.query` / `jobs.get` / `jobs.getQueryResults` / `datasets.*` / `tables.*` | `bigquery`, `cloud-platform`, `cloud-platform.read-only` |
| `jobs.insert` | `bigquery`, `cloud-platform` |
| `jobs.cancel` | `bigquery`, `cloud-platform` |

`bigquery.readonly` is listed for none of these methods, so a read-only scope cannot run queries. The default user ADC scope, `cloud-platform`, covers every path. The service-account path requests `https://www.googleapis.com/auth/bigquery`.

**IAM roles**:

| Target | Role | Why |
|---|---|---|
| Billing project | `roles/bigquery.jobUser` | `bigquery.jobs.create`; getting and cancelling one's own jobs is included |
| Data projects / datasets | `roles/bigquery.dataViewer` | `datasets.get`, `tables.list`, `tables.get`, `tables.getData` |
| Discovery-only projects | `roles/bigquery.metadataViewer` | lists and schemas only |

**Not needed**: `dataEditor` or above (withholding it is the permanent read-only boundary), `serviceusage.services.use`, the `x-goog-user-project` header. The BigQuery API enabled on the billing project is sufficient.

## 6. Series Placement

Series: **util-series**
Reason: the same row as splunk-mcp, data-toolbox-mcp and pcap-analyzer-mcp — analysis-infrastructure MCP servers. Not a user-authenticated interactive CLI (cli-series) nor Slack automation (chatops-series).

## 7. External Platform Constraints

| Constraint | Value | Design response |
|---|---|---|
| `jobs.query` response size | 20 MB per page | Page with `maxResults`; loop `getQueryResults` until `max_rows` / `max_bytes` |
| Unresolved SQL length | 1 MB | Reported under `invalid_query` details |
| On-demand daily query bytes | 200 TiB per project by default | README points to the project-level custom quota above the gate |
| Queued interactive queries | 1,000 per project | `rate_limited` (retryable) |
| `jobs.get` | 1,000 req/s per project | Phase 2 polling backs off exponentially |
| `maximumBytesBilled` | on-demand pricing only | README states that under Editions the gate rests on the dry-run estimate |
| Minimum billed unit | 10 MB per table | `bytes_billed` comes in 10 MB steps; noted |
| Job location | decided by the datasets | Keep `jobReference.location` and always pass it to `getQueryResults`; cross-region references fail |
| `referencedTables` | absent from the `jobs.query` response | Use `jobs.insert(dryRun)` only when an allowlist is configured |
| ADC token | 1 hour | oauth2 renews it |
| MCP client tool-result limit | Claude Desktop / Claude Code have rejected responses of a few hundred KB in this fleet | `max_bytes` byte budget beside `max_rows` |

To re-check in Phase 0 and pin in an ADR: concurrent interactive queries (100 per project), query execution time limit (6 hours), per-user API rates (100 req/s, 300 concurrent).

---

## Discussion Log

- **Problem statement**: the user approved ADC and all three protections (budget gate, read-only, error contract) for v1; asynchronous jobs moved to Phase 2. Multi-project handling was raised for examination.
- **Standing of the official server**: the initial "it is beta" premise proved weak against the release notes (Preview 2025-12-10, available by default from 2026-03-17). The value of an in-house server was restated: the failing MCP front-end layer and the mcp-bridge hop disappear, and error reasons can be mapped to a contract.
- **Multi-project**: BigQuery separates the billing project from data projects, and cross-project reads are native under IAM. Options A (one instance = one billing project), B (profiles + a `profile` argument) and C (per-call `project_id`) were compared; A was chosen because the model must not pick the billing project or the budget. The optional `project` argument on metadata tools was approved.
- **Results**: the first proposal reused splunk-mcp's workspace spill. The user stated the direction: that method is being retired; everything is returned in the response and the agent runtime files it automatically. The server keeps only an explicit cap (`max_rows`) with truncation accounting. The rationale is already recorded in knowledge's mcp-server-design.md.
- **Design decisions**: no SDK (measured 236 versus 3 modules); writes entirely out of v1; SQL logging opt-in (user's instruction: an independent switch, not tied to the log level).
- **Development plan**: all six tools in Phase 1, live E2E on the Workspace audit export dataset, four ADRs written first in Phase 0 — approved.
- **Scopes**: the proposal to narrow ADC with `--scopes` was rejected by the user — ADC is shared by the machine and gem-agent's Vertex use would break. The README carries the opposite note.
- **Series and constraints**: util-series, and the `max_bytes` byte budget — approved.
