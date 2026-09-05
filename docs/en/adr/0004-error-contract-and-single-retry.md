# ADR-0004: The error contract `{code, message, retryable, details}` and a single retry

| Field | Value |
|-------|-------|
| Status | **Accepted** |
| Date | 2026-09-06 |
| Binds | bigquery-mcp |
| Decision makers | nlink-jp maintainers |
| Triggered by | Session 4d6bb685 (2026-09-05): the official remote MCP answered correct calls with "Required parameter is missing: query" for three minutes; the string carried no cause and no retry hint, and the agent spent 56 rounds probing its own runtime |

## Context

An agent reading a tool error needs three things the official server did
not give: whose fault it is (the arguments, the permissions, the server),
whether trying again can help, and what would make the call pass. BigQuery
itself answers the first through `error.errors[].reason` — `invalidQuery`,
`accessDenied`, `notFound`, `rateLimitExceeded`, `quotaExceeded`,
`backendError`, `internalError`, `resourcesExceeded`, `timeout` and others
— with `location` for syntax errors; the MCP front end flattened this to a
sentence.

The organisation's MCP servers already return `{code, message, details}`
(data-toolbox-mcp `internal/toolerr`); this server adds the retry
dimension, which is the one the incident lacked.

## Decision

1. **Every tool error is one JSON object in the text content of an
   `isError` result:** `code` (a stable slug from the table below),
   `message` (what to change, in one or two sentences), `retryable` (always
   present, never omitted), `details` (machine-readable context).
2. **The code table** and its mapping from BigQuery reasons:

   | code | from | retryable |
   |---|---|---|
   | `invalid_query` | `invalidQuery`, `invalid` on a query call; `details.location` and `details.reason` | no |
   | `statement_not_allowed` | the gate (ADR-0002); `details.statement_type` | no |
   | `dataset_not_allowed` | the gate; `details.table`, `details.allowed` | no |
   | `budget_exceeded` | the gate; `details.bytes_processed`, `details.budget_bytes` | no |
   | `access_denied` | `accessDenied`, HTTP 403; the message names the role that is usually missing | no |
   | `not_found` | `notFound`, HTTP 404; `details.resource` | no |
   | `rate_limited` | `rateLimitExceeded`, `quotaExceeded` with a per-minute or concurrency scope, HTTP 429 | yes |
   | `backend_error` | `backendError`, `internalError`, HTTP 5xx | yes |
   | `timeout` | the job's `jobTimeoutMs`, or the results wait; `details.job_id` | no |
   | `auth_error` | ADC missing or a token that cannot be obtained or renewed, HTTP 401 | no |
   | `invalid_arguments` | this server's argument validation | no |
   | `upstream_error` | an HTTP or transport failure that maps to nothing above; `details.status` | no |

   A daily quota (`quotaExceeded` with a "per day" message) is **not**
   retryable — the table maps it to `rate_limited` with `retryable: false`
   and the message says when it resets.
3. **One retry, inside the server, for retryable codes only**, after a
   jittered delay of 0.5–1.5 s, and only for requests that are idempotent as
   sent: a dry run, a metadata read, and a `jobs.query` that has not
   returned a job id. A second failure returns the error to the model with
   `retryable: true`, and the model decides. This server never loops.
4. **`details` always includes what the model needs to act**: `job_id` and
   `location` when a job exists, `reason` verbatim from BigQuery, the
   estimate and the budget on `budget_exceeded`, the offending table and the
   allowlist on `dataset_not_allowed`.
5. **Runtime-side counting is not this server's job.** An agent that keeps
   receiving the same error is the runtime's concern (gem-agent ADR-0075);
   this server's part is to make each error self-explanatory.

## Consequences

- `internal/toolerr` gains `Retryable bool` (serialised always) and
  `AsRetryable()`; `internal/bq/errors.go` holds the single mapping
  function with a table-driven test over recorded BigQuery error bodies.
- The `get_usage` document carries the same table as an error-recovery
  guide, per the fleet convention.
- Transient faults of the kind seen in the incident resolve inside one
  call when they last under two seconds, and are reported honestly when
  they do not.

## Alternatives considered

- **Pass BigQuery's error body through.** Rejected: the body is a nested
  Google API envelope with fields the model does not need, and `reason`
  alone does not say whether to retry.
- **Unlimited or exponential retries in the server.** Rejected: hides the
  server's state from the model and the operator; a query that fails five
  times in a row is information.
- **Classifying by message text** (e.g. searching for "quota"). Rejected
  except where BigQuery gives no structured field: the daily-versus-minute
  distinction of `quotaExceeded` is the one documented exception, made on
  the reason plus a fixed phrase, and tested.

## References

- RFP §2 error contract, §3.7, §3.9
- data-toolbox-mcp `internal/toolerr` (the fleet's structured-error precedent)
- BigQuery error table: https://cloud.google.com/bigquery/docs/error-messages
- gem-agent ADR-0075 (Proposed): the runtime-side fault counter
