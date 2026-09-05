# bigquery-mcp — CLAUDE.md

Project-specific instructions for Claude Code.
Org conventions: https://github.com/nlink-jp/.github/blob/main/CONVENTIONS.md
Project summary, structure, and gotchas: see `AGENTS.md`.

## Non-negotiable rules

- **Tests are mandatory** — a feature is not complete without tests.
- **Never `go build` directly** — always `make build` (outputs to `dist/`).
- **Docs in sync** — README.md and README.ja.md updated in the same commit
  as behaviour changes; docs/en and docs/ja stay mirrors.
- **Small, typed commits** — `feat:`, `fix:`, `test:`, `chore:`, `docs:`.
- **No project ids, dataset names or tokens in the repo** — live tests read
  them from the environment.

## Project-specific rules

- **stdout is the JSON-RPC channel.** Nothing else may ever write to it —
  no fmt.Println, no library that prints. Diagnostics go through slog.
- **The gate order is the product.** `query` is dry run → SELECT check →
  allowlist → budget → run. Any change that lets a query run before or
  around one of those checks is a design regression; change ADR-0002
  first.
- **Never classify SQL by inspecting its text.** BigQuery's `statementType`
  and `referencedTables`/`referencedRoutines` decide. Warnings compare
  bytes, not strings.
- **Queries run as client-named jobs** (`jobs.insert` with a jobId). Do
  not reintroduce `jobs.query` for the run: a retry could bill twice.
- **Errors are structured** (`internal/toolerr`). A new failure path gets
  a stable code, a message that says what to change, an explicit
  `retryable`, and details an agent can act on. New codes go into
  `toolerr.Codes`; new BigQuery reasons go into `reasonRules`; the test
  keeps them in sync.
- **No spill, no silent truncation.** Results stop at the caps and say so.
- New tools follow the existing shape: `var xxxTool = mcpserver.Tool{...}`
  + `(d *deps) xxx(ctx, args)` + registration in `tools.go` + coverage in
  `tools_test.go` + a row in the `get_usage` tools table.
- The RFP records scope decisions; update it (and the ADR it touches)
  when a decision changes.
