# ADR-0001: Call the BigQuery REST API v2 directly; no BigQuery SDK

| Field | Value |
|-------|-------|
| Status | **Accepted** |
| Date | 2026-09-06 |
| Binds | bigquery-mcp |
| Decision makers | nlink-jp maintainers |
| Triggered by | RFP §2 "External Dependencies" and §3.1: the user's initial framing was "implement it ourselves on the BigQuery SDK"; the dependency graph had to be measured before that became the design |

## Context

The server needs seven REST calls (`jobs.insert` for the dry run,
`jobs.query`, `jobs.getQueryResults`, `datasets.list`, `datasets.get`,
`tables.list`, `tables.get`; Phase 2 adds `jobs.get` and `jobs.cancel`) and
one credential source, Application Default Credentials.

Measured on 2026-09-05 with Go 1.27 in an empty module:

| import | modules in the build list | packages linked |
|---|---|---|
| `cloud.google.com/go/bigquery` | 236 | 330 |
| `golang.org/x/oauth2/google` only | 3 | 27 |

The SDK pulls `google.golang.org/grpc`, `google.golang.org/protobuf`,
`google.golang.org/api` and the whole `cloud.google.com/go` core for a
server that speaks JSON over HTTPS to seven endpoints. The organisation's
supply-chain policy allows the standard library, first-party SDKs and direct
REST; within that set the smallest option that meets the need wins.

The REST surface is stable and fully described by the Discovery document
(`https://bigquery.googleapis.com/$discovery/rest?version=v2`), from which
the handful of request and response structs this server needs are written by
hand. The organisation already did the same for Splunk (splunk-mcp) with a
REST client ported from splunk-cli.

## Decision

1. `internal/bq` is a hand-written client over `net/http` against
   `https://bigquery.googleapis.com/bigquery/v2`. It carries exactly the
   fields this server reads or writes; unknown response fields are ignored.
2. Authentication is `golang.org/x/oauth2/google.DefaultTokenSource` with
   scope `https://www.googleapis.com/auth/bigquery`. For user ADC the scope
   is fixed at `gcloud auth application-default login` time (cloud-platform)
   and the request is ignored; for service-account ADC it narrows the token.
   The ADC file is never enumerated or parsed by this code — the library
   reads the one path ADC defines.
3. `cloud.google.com/go/bigquery` and `google.golang.org/api` are not
   dependencies and must not become transitive ones; `go.mod` is the test.

## Consequences

- Three direct third-party modules (cobra, toml, oauth2) and three
  indirect ones (pflag, mousetrap, compute/metadata), matching the fleet's
  other MCP servers plus the credential library.
- New REST fields (a new statistics block, a new job option) require a
  struct edit here rather than an SDK bump. The Discovery document is the
  reference; `docs/en/architecture.md` lists the fields used.
- Retries, paging and error mapping are this server's own code (ADR-0004),
  which is where the RFP wants them — they are the product.
- The ADC read happens in-process. Its EDR footprint is the same file open
  the SDK would perform; an operator who wants a different footprint gets
  `[auth] token_command` in Phase 2.

## Alternatives considered

- **The BigQuery Go SDK.** Rejected on the measurement above: 236 modules
  for seven calls, and the retry and paging behaviour it brings is exactly
  what this server must control itself.
- **`google.golang.org/api/bigquery/v2`** (the generated Discovery client).
  Smaller than the cloud SDK but still brings `google.golang.org/api` and
  its transport stack; the generated types are the same shapes written here
  by hand for a fraction of the surface.
- **Delegating tokens to `gcloud auth print-access-token`** as the only
  credential path. Rejected as the default: it spawns a Python process per
  renewal and fails inside sandboxes without a network (observed in
  gem-agent's read lane). Kept as the Phase 2 `token_command` option.

## References

- RFP §2 External Dependencies, §3.1, §5
- BigQuery Discovery document v2 (fields listed in `docs/en/architecture.md`)
- splunk-mcp `internal/client` (the fleet's REST-direct precedent)
