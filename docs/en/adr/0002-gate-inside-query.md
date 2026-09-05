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
     `project.*`). This check needs `referencedTables`, which only the
     `jobs.insert` dry run reports, so **the dry run always goes through
     `jobs.insert`** — it is one request either way, creates no job, and
     the referenced tables also feed the partition-filter warning (§5).
   - `budget_exceeded` when the **billed estimate** — `totalBytesProcessed`
     rounded up to the next MiB, with BigQuery's 10 MiB minimum per
     referenced table — is above `[budget] max_bytes_billed`. Nothing has
     run; the error carries the raw estimate, the billed estimate, the
     budget and the estimate's accuracy. A budget below 10 MiB is refused
     by the config loader, because it would fail every real query.
3. **The run carries the kernel caps.** The query runs through
   `jobs.insert` as a job this server names (`bqmcp-<32 hex>`), with
   `maximumBytesBilled` set to the same `max_bytes_billed`, `jobTimeoutMs`
   to `[budget] job_timeout`, and the label `bigquery-mcp: true`. A query
   whose true bytes billed exceed the estimate is stopped by BigQuery, not
   by this server, and reported as `budget_exceeded` with a hint that
   distinguishes the kernel cap from the gate. Naming the job is also what
   makes the single retry safe (ADR-0004 §3).
4. **IAM is the permanent boundary.** The README's setup grants
   `roles/bigquery.jobUser` on the billing project and
   `roles/bigquery.dataViewer` on the data; nothing that can write. The
   server's statement check is the second line, never the only one, and the
   README says so.
5. **Warnings, not refusals, for cost shape — from numbers, never from
   SQL text.** `dry_run` and `query` warn when a referenced table is
   partitioned and the dry-run estimate covers its whole size (the
   partition column pruned nothing), and when the estimate's accuracy is
   not `PRECISE` (the kernel cap may still stop the run). The SQL is never
   inspected, consistent with §2. This is advice; the budget is the rule.
6. **Routines are held to the allowlist too.** A table function or UDF
   can read tables of its own dataset; `referencedRoutines` from the dry
   run is checked against `[access] datasets` like tables are.

## Consequences

- One billing project per instance (RFP §3.12) is what makes the budget a
  configuration value rather than a model choice.
- Under Editions pricing the gate is the dry-run estimate alone; the README
  states it, and `dry_run` reports `bytes_processed` either way.
- Legitimate non-SELECT reads — `ASSERT`, a `CALL` to a read-only
  procedure, scripts with `DECLARE` — are refused in v1. Phase 2's
  protected write mode is the place to widen this deliberately.
- One dry-run path (`jobs.insert`), exercised by the fake server tests
  and by the live test against a real project.

## Independent design review (2026-09-06) — what changed

A fresh-context reviewer re-verified the API claims against the Discovery
document and reported 15 findings; the ones touching this record:

- **Adopted — one dry-run path.** The "cheaper" `jobs.query` dry run was a
  false premise (one request either way) and dropped `referencedTables`;
  the dry run always uses `jobs.insert` now (§2).
- **Adopted — billing minimum and rounding.** The gate compares the billed
  estimate, and a budget under 10 MiB is refused (§2).
- **Adopted — routines.** `referencedRoutines` joins the allowlist check (§6).
- **Adopted — text-free warnings.** The "SQL mentions the column" rule
  contradicted §2's own principle; replaced by the byte comparison and the
  accuracy field (§5).
- **Adopted — kernel-cap reason.** `bytesBilledLimitExceeded` maps to
  `budget_exceeded` with its own hint (ADR-0004 table).
- **Recorded, not adopted — expanding TVF bodies.** Whether a table
  function's underlying tables appear in `referencedTables` is not stated
  by the Discovery document; the live test pins the observed behaviour
  rather than the design assuming it.

## Alternatives considered

- **A regex or parser over the SQL** to detect writes. Rejected: unbounded
  domain, and BigQuery already publishes its own classification.
- **Only IAM**, no server check. Rejected: the operator may hold wider roles
  than the server should exercise (they usually do), and a refused statement
  with a stable code is what makes the model stop rather than probe.
- **`dry_run` as an optional first step the prompt recommends.** Rejected:
  a capability without a trigger does not fire; the budget must not depend
  on the model remembering.
- **`jobs.query` with `dryRun` when no allowlist is configured.** Rejected
  on second look: it is not cheaper (one request either way), and it drops
  `referencedTables`, which the partition-filter warning needs whether or
  not an allowlist exists. One path is simpler to test than two.

## References

- RFP §3.2–3.5, §7 (`maximumBytesBilled` on-demand only; `referencedTables`
  absent from `jobs.query`)
- BigQuery Discovery v2: `QueryRequest`, `QueryResponse`, `JobStatistics2.statementType`
- Organisation knowledge: capability-without-trigger; bounded domains over text rules
