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
2. **One reason table.** `internal/bq/errors.go` holds the single map
   from BigQuery `reason` to `{code, retryable, hint}`; `toolerr.Codes`
   holds the single documented list of codes; `get_usage` renders that
   list and a test checks every code the mapping can produce is in it.
   Reason decides first; the HTTP status is only the fallback, because
   BigQuery answers 403 for quota and rate limits as well as for missing
   IAM. The table:

   | code | from | retryable |
   |---|---|---|
   | `invalid_query` | `invalidQuery`, `invalid`, `invalidQueryParameter`; `resourcesExceeded`, `responseTooLarge`, `billingTierLimitExceeded` (with a "do less work" hint); a 400 with no known reason. `details.location` and `details.reason` | no |
   | `statement_not_allowed` | the gate (ADR-0002); `details.statement_type` | no |
   | `dataset_not_allowed` | the gate; `details.table` or `details.routine`, `details.allowed` | no |
   | `budget_exceeded` | the gate (`details.bytes_processed`, `bytes_billed_estimate`, `budget_bytes`, `accuracy`), or BigQuery's `bytesBilledLimitExceeded` when the kernel cap stopped a job whose estimate had passed (its own hint) | no |
   | `access_denied` | `accessDenied`, `userNotAuthorized` (IAM hint); `billingNotEnabled` (billing hint); `accessNotConfigured` (API-not-enabled hint); a 403 with no known reason | no |
   | `not_found` | `notFound`, `tableNotFound`, `datasetNotFound`, HTTP 404; `details.resource` | no |
   | `rate_limited` | `rateLimitExceeded`, `quotaExceeded`, `concurrentQueryLimitExceeded`, HTTP 429 | yes, unless the message says per day |
   | `backend_error` | `backendError`, `internalError`, `jobBackendError`, `jobInternalError`, `unavailable`, HTTP 5xx except 501 | yes |
   | `timeout` | `timeout`, `jobTimeout`, `stopped` whose message says "timed out", HTTP 408/504, the client's own deadline; `details.job_id` | no |
   | `cancelled` | `stopped` for any other reason (console, `bq cancel`) | no |
   | `auth_error` | ADC missing, or a token that cannot be obtained or renewed; `authError`, `unauthorized`, HTTP 401 | no |
   | `duplicate` | `duplicate`, HTTP 409 — handled inside the client on retry, surfaced only if it reaches a tool | no |
   | `invalid_arguments` | this server's argument validation | no |
   | `upstream_error` | `notImplemented`/501, a transport failure, or a status that maps to nothing above; `details.http_status` | transport failures yes, others no |

   A daily quota (`quotaExceeded` whose message says "per day" or
   "daily") is the one text rule in the mapping: BigQuery gives no
   structured field for it, the phrase is fixed, and it is tested.

3. **One retry, inside the server, for retryable codes only**, after a
   jittered delay of 0.5–1.5 s. Every request this client sends is
   idempotent as sent, so the retry can never duplicate work: dry runs
   create no job, metadata reads and `getQueryResults` are reads, and
   **the query itself runs as a job the client names** (`jobs.insert`
   with `jobReference.jobId = bqmcp-<32 hex>`). If the first insert
   reached BigQuery and only the response was lost, the retry is answered
   `409 duplicate` and the client continues with the job that exists —
   the query never runs twice and is never billed twice. A second failure
   returns the error to the model with `retryable: true`, and the model
   decides. This server never loops.

4. **`details` always includes what the model needs to act**: `job_id` and
   `location` when a job exists, `reason` verbatim from BigQuery, the
   estimate and the budget on `budget_exceeded`, the offending table and the
   allowlist on `dataset_not_allowed`.
5. **Runtime-side counting is not this server's job.** An agent that keeps
   receiving the same error is the runtime's concern (gem-agent ADR-0075);
   this server's part is to make each error self-explanatory.

## Independent design review (2026-09-06) — what changed

- **Adopted — the retry could bill twice.** `jobs.query` with `requestId`
  deduplicates only mutating queries; a read timeout after the request
  reached BigQuery would have run a SELECT twice. Replaced by client-named
  jobs through `jobs.insert` (§3), which also gives Phase 2's `cancel_job`
  a handle.
- **Adopted — statistics live on the Job.** `getQueryResults` carries no
  `totalBytesBilled`, `totalSlotMs`, `statementType` or `cacheHit`; the
  client reads them from `jobs.get` once the job is done.
- **Adopted — one table.** The RFP, this record and the code had drifted
  into three tables; the code's `reasonRules` map and `toolerr.Codes` are
  the source, and this record's table above is written from them.
- **Adopted — reason before status**; `stopped` splits into `timeout` and
  `cancelled`; 501 is not retryable; `billingNotEnabled` and
  `accessNotConfigured` get their own hints instead of the IAM one.
- **Adopted — timestamps as ISO 8601 strings.** `formatOptions.timestampOutputFormat=ISO8601_STRING`
  carries picosecond precision that no numeric encoding does; the client
  passes the string through (architecture.md).

## Consequences

- `internal/toolerr` gains `Retryable bool` (serialised always),
  `AsRetryable()`, and the documented `Codes` list; `internal/bq/errors.go`
  holds the reason map with a table-driven test over recorded BigQuery
  error bodies and a test that every produced code is documented.
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
