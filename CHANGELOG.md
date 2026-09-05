# Changelog

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
