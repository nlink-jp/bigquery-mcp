# Changelog

## [Unreleased]

### Fixed

- **`make verify-release` now fails closed.** Its last block chained unzip, the
  packaged binary's `--version` and `spctl` with `&&` and ended the whole chain
  in `|| true`, so a zip that did not unpack or a binary that did not run exited
  0 and the upload proceeded. Each step is now judged on its own, the packaged
  binary's `--version` must contain the tag being released, and only the
  informational `spctl` line may be ignored. Matches the org template
  (CONVENTIONS.md §Code Signing → Verifying a release).

## [0.1.1] - 2026-09-21

### Fixed

- All six MCP tool schemas now set `additionalProperties: false` at the top
  level, as organization ADR-021 §10 requires, so a client validating
  arguments against the schema refuses a mistyped parameter instead of
  forwarding it. `parseArgs` already rejected unknown fields, so both halves
  of the contract now agree.

### Added

- `TestEveryToolSchemaIsClosed` — walks the production registry via a real
  `tools/list` and fails if any tool's top-level schema omits
  `additionalProperties: false` or sets it true.
- `TestParamsObjectStaysOpen` — the nested `params` object of `dry_run` and
  `query` must keep `additionalProperties: true`: its keys are the caller's
  own named query parameters, which no schema can enumerate, so closing it
  would reject every parameterised query. Only the top level is closed, and
  the exception is now protected against a future sweep.
- `TestUnknownArgumentIsRejected` — proves the strictness is real and not
  merely declared: a misspelled `max_rows` comes back as
  `invalid_arguments` naming the field, rather than silently falling back to
  the configured default.

## [0.1.0] - 2026-09-06

### Added

- Six MCP tools over stdio: `list_datasets`, `list_tables`,
  `describe_table`, `dry_run`, `query`, `get_usage`.
- The gate inside `query`: dry run through `jobs.insert`, then
  `statement_not_allowed` unless BigQuery classified the statement as
  `SELECT`, `dataset_not_allowed` against an optional `[access] datasets`
  allowlist (tables and routines), `budget_exceeded` when the billed
  estimate (MiB rounding, 10 MiB per table) exceeds
  `[budget] max_bytes_billed`. Nothing runs when a check fails.
- Runs as client-named jobs with `maximumBytesBilled`, `jobTimeoutMs` and
  the label `bigquery-mcp: true`; billing statistics read from `jobs.get`.
- Rows as column-keyed objects (nested records, repeated fields, ISO 8601
  timestamps); explicit caps `max_rows` and `[results] max_bytes` with
  `truncated`, `truncated_by` and `total_rows` accounting; no server-side
  files.
- Named query parameters, typed by inference or as `{type, value}`.
- Error contract `{code, message, retryable, details}` from one BigQuery
  reason table; one jittered retry for retryable failures; every produced
  code documented in `get_usage`.
- Byte-based warnings: a partitioned table scanned whole, an imprecise
  estimate.
- `doctor` subcommand: ADC → billing project reach → jobs IAM.
- Sectioned TOML config with strict keys; Application Default Credentials
  (`golang.org/x/oauth2/google`), no BigQuery SDK.
- Opt-in live tests (`make live-test`) against a real project.
