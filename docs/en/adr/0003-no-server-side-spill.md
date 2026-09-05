# ADR-0003: No server-side spill — explicit caps and truncation accounting

| Field | Value |
|-------|-------|
| Status | **Accepted** |
| Date | 2026-09-06 |
| Binds | bigquery-mcp |
| Decision makers | nlink-jp maintainers |
| Triggered by | RFP §2 "Input / Output": the first draft reused splunk-mcp's `workspace_root` JSONL spill; the maintainer stated that the fleet is retiring that method — results are returned in the response and the agent runtime files them |

## Context

Seven file-mediated servers in this fleet each grew a `workspace_root`
argument: above a threshold they wrote the result to a directory the caller
named and returned a path. The organisation's knowledge base records the
diagnosis — "too large" is a property of the caller's context window, which
a server cannot observe, so seven servers were re-implementing a guard the
client did not have. gem-agent now has that guard (its ADR-0058 intake
saves oversized tool results to the session work directory and hands the
model a preview and a path), and other clients have their own result
limits.

BigQuery results are unbounded (millions of rows) and wide (a 100 MB row is
legal). Some bound is needed on the server, but it is a *cap*, not a
routing decision.

## Decision

1. **Everything is returned in the response.** No `workspace_root`
   argument, no file written by this server, no path in a result.
2. **Two explicit caps, both accounted for.**
   - `max_rows`: a call argument, defaulting to `[results] default_max_rows`
     (1000) and refused above `[results] hard_max_rows` (50000) with
     `invalid_arguments`.
   - `[results] max_bytes` (1 MiB): a byte budget for the serialised
     response. Rows are appended while the running size stays under it; the
     first row that would cross it ends the result.
   Hitting either sets `truncated: true`, names the cause in
   `truncated_by` (`max_rows` or `max_bytes`), and reports `total_rows`
   from BigQuery's own count. There is no path that drops rows silently.
3. **Paging stops at the cap.** `jobs.getQueryResults` is called with
   `maxResults` bounded by what remains of `max_rows`; a result cut by
   `max_bytes` stops fetching at the page in hand. BigQuery's 20 MB page
   limit is never the binding constraint because `max_bytes` is smaller.
4. **The cap is stated to the model.** `get_usage` and every truncated
   result say what to do: add a `LIMIT` or a tighter `WHERE`, raise
   `max_rows` up to the ceiling, or aggregate in SQL. The agent runtime, not
   this server, decides whether to keep a large result on disk.

## Consequences

- One row-shaping code path (`shape.go`) with unit tests for both caps,
  their interaction, and the accounting fields.
- Wide rows with `max_bytes` at 1 MiB can return few rows; the accounting
  makes that visible, and the operator can raise the budget for a client
  that handles more.
- Phase 2's asynchronous `get_results` pages with an `offset` under the same
  caps, so a model that wants everything walks pages explicitly.
- The fleet's older servers keep `workspace_root` until each is revised;
  this server never had it.

## Alternatives considered

- **`workspace_root` spill (splunk-mcp's contract).** Rejected by the
  maintainer's fleet decision and the knowledge-base diagnosis above.
- **No cap at all** ("the runtime handles it"). Rejected: a client without a
  guard would receive megabytes for `SELECT *`, and the 20 MB page limit
  would surface as an upstream error rather than an accounted truncation.
- **A row cap only.** Rejected: width is unbounded; the fleet has seen a
  162 KB response refused by a client for a `limit: 3` call.
- **Streaming partial results** (multiple content blocks). Rejected: MCP
  tool results are delivered whole; blocks do not change what the client
  must hold.

## References

- RFP §2 Input / Output, §3.8, §7 (20 MB page, client result limits)
- nlink-jp/knowledge `mcp-server-design.md` — the `workspace_root` diagnosis
- gem-agent ADR-0058 (the runtime-side intake)
