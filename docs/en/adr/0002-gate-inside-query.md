# ADR-0002: The gate lives inside `query`; BigQuery's statementType and IAM decide

| Field | Value |
|-------|-------|
| Status | **Accepted** |
| Date | 2026-09-06 |
| Binds | bigquery-mcp |
| Decision makers | nlink-jp maintainers |
| Triggered by | RFP §1 (protection first) and §3.2–3.5; the official remote MCP's `execute_sql_readonly` restricts "to SELECT statements" by inspecting text and offers no budget |

## Context

Three protections were fixed as v1 requirements: a budget gate, read-only
execution, and an error contract (ADR-0004). This record decides where the
first two live and what decides them.

Two facts from the Discovery document shape the design:

- `jobs.query` accepts `dryRun`, `maximumBytesBilled`, `jobTimeoutMs` and
  `labels`, and its response carries `statementType` and
  `totalBytesProcessed`. It does **not** carry `referencedTables`.
- `jobs.insert` with `configuration.dryRun = true` returns the Job at once,
  and `statistics.query` carries `statementType`, `totalBytesProcessed`,
  `referencedTables` and `schema`.

`maximumBytesBilled` fails the job "without incurring a charge" when the
bytes billed would exceed it — under on-demand pricing. Under Editions the
bytes billed are 0 and the cap is inert.

Statement classification by regular expression over SQL is an unbounded
domain (comments, string literals, scripts, `EXECUTE IMMEDIATE`, multi-
statement scripts whose first statement is a `SELECT`). BigQuery classifies
the statement itself and reports it as `statementType`; a multi-statement
script reports `SCRIPT`.

A model that can skip a check will, eventually, skip it — an optional
`dry_run` tool is a capability without a trigger (the organisation's
recorded lesson). The gate therefore cannot be a separate tool the model is
asked to call first.

## Decision

1. **`query` always dry-runs first.** The sequence dry run → gate → run is
   inside the tool; there is no argument that skips it. `dry_run` exists as
   a separate tool for the model's own preview, and runs the same code.
2. **Three gate checks, in this order, each with its own error code:**
   - `statement_not_allowed` unless the dry run's `statementType` is exactly
     `SELECT`. `SCRIPT`, `EXPORT_DATA`, every DML and DDL type, `ASSERT` and
     `CALL` are all refused by this one comparison. No SQL text is inspected.
   - `dataset_not_allowed` when `[access] datasets` is set and a referenced
     table's `project.dataset` matches no entry (`project.dataset` exact or
     `project.*`). This check needs `referencedTables`, so **when an
     allowlist is configured the dry run goes through `jobs.insert`**; with
     no allowlist it goes through `jobs.query`, which is one round trip
     cheaper.
   - `budget_exceeded` when `totalBytesProcessed` is above
     `[budget] max_bytes_billed`. Nothing has run; the error carries the
     estimate and the budget.
3. **The run carries the kernel caps.** `maximumBytesBilled` is set to the
   same `max_bytes_billed`, `jobTimeoutMs` to `[budget] job_timeout`, and
   the label `bigquery-mcp: true` is attached. A query whose true bytes
   billed exceed the estimate is stopped by BigQuery, not by this server.
4. **IAM is the permanent boundary.** The README's setup grants
   `roles/bigquery.jobUser` on the billing project and
   `roles/bigquery.dataViewer` on the data; nothing that can write. The
   server's statement check is the second line, never the only one, and the
   README says so.
5. **Warnings, not refusals, for cost shape.** When `describe_table` or the
   dry run shows a referenced table is partitioned and the SQL mentions no
   filter on the partition column, `dry_run` and `query` add a warning
   string. This is advice; the budget is the rule.

## Consequences

- One billing project per instance (RFP §3.12) is what makes the budget a
  configuration value rather than a model choice.
- Under Editions pricing the gate is the dry-run estimate alone; the README
  states it, and `dry_run` reports `bytes_processed` either way.
- Legitimate non-SELECT reads — `ASSERT`, a `CALL` to a read-only
  procedure, scripts with `DECLARE` — are refused in v1. Phase 2's
  protected write mode is the place to widen this deliberately.
- Two dry-run paths (`jobs.query` and `jobs.insert`) are exercised by the
  fake server tests; the live test runs both against a real project.

## Alternatives considered

- **A regex or parser over the SQL** to detect writes. Rejected: unbounded
  domain, and BigQuery already publishes its own classification.
- **Only IAM**, no server check. Rejected: the operator may hold wider roles
  than the server should exercise (they usually do), and a refused statement
  with a stable code is what makes the model stop rather than probe.
- **`dry_run` as an optional first step the prompt recommends.** Rejected:
  a capability without a trigger does not fire; the budget must not depend
  on the model remembering.
- **Always `jobs.insert` for the dry run.** Rejected for the no-allowlist
  case: one more request per query for a field that is not read.

## References

- RFP §3.2–3.5, §7 (`maximumBytesBilled` on-demand only; `referencedTables`
  absent from `jobs.query`)
- BigQuery Discovery v2: `QueryRequest`, `QueryResponse`, `JobStatistics2.statementType`
- Organisation knowledge: capability-without-trigger; bounded domains over text rules
