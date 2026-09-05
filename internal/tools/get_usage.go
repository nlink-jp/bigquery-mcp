package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nlink-jp/bigquery-mcp/internal/mcpserver"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

var getUsageTool = mcpserver.Tool{
	Name:        "get_usage",
	Description: "Full reference for bigquery-mcp: the workflow, what the gate refuses, the result caps, and the error-recovery table. Call this before your first query.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
}

func (d *deps) getUsage(ctx context.Context, args json.RawMessage) (any, error) {
	allow := "(none — every dataset the credentials can read)"
	if len(d.cfg.Datasets) > 0 {
		allow = strings.Join(d.cfg.Datasets, ", ")
	}
	text := fmt.Sprintf(usageTemplate,
		d.client.Project(), humanBytes(d.cfg.MaxBytesBilled), d.cfg.JobTimeout, allow,
		d.cfg.DefaultMaxRows, d.cfg.HardMaxRows, humanBytes(d.cfg.MaxBytes), errorTable())
	return mcpserver.RawResult{Content: []mcpserver.ContentBlock{{Type: "text", Text: text}}}, nil
}

// errorTable renders toolerr.Codes — the one documented list (ADR-0004).
func errorTable() string {
	var b strings.Builder
	b.WriteString("| code | cause | recovery | retryable |\n|---|---|---|---|\n")
	for _, c := range toolerr.Codes {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.Code, c.Cause, c.Recovery, c.Retryable)
	}
	return b.String()
}

const usageTemplate = `# bigquery-mcp usage

Protection-first BigQuery MCP server. This instance bills to project **%s**
and runs with your own Application Default Credentials. It calls no LLM:
you write the SQL.

- Byte budget: **%s** per query (the dry-run estimate, rounded to MiB with a 10 MiB minimum per table, must fit; the run also carries maximumBytesBilled)
- Job timeout: **%s**
- Dataset allowlist: %s
- Rows per query: %d by default, %d at most (` + "`max_rows`" + `); response byte budget %s

## Tools

| Tool | Purpose |
|---|---|
| list_datasets | Datasets of a project (billing project by default) |
| list_tables | Tables and views of a dataset, with partition column |
| describe_table | Schema, partitioning, clustering, rows, size, timestamps, view SQL |
| dry_run | Bytes, accuracy, statement type, referenced tables and routines, schema, gate verdict, warnings |
| query | Dry run → gate → run; rows as column-keyed objects under the caps |
| get_usage | This document |

## Workflow

1. ` + "`list_datasets`" + ` → ` + "`list_tables`" + ` → ` + "`describe_table`" + ` to learn the schema and the
   partition column before writing SQL.
2. ` + "`dry_run`" + ` when cost is uncertain: it says the bytes, whether the gate
   would pass, and whether the partition column pruned anything.
3. ` + "`query`" + `. Aggregate in SQL; ask for detail rows only when you need them.
   Use literals, not parameters, in partition filters: a parameterised
   filter may not prune at dry-run time.

## What the gate refuses (nothing runs when it does)

| code | meaning | what to do |
|---|---|---|
| statement_not_allowed | BigQuery classified the statement as something other than SELECT (scripts, DML, DDL, EXPORT, CALL, ASSERT) | rewrite as one SELECT |
| dataset_not_allowed | a referenced table or routine is outside the allowlist | query the allowed datasets, or ask the operator |
| budget_exceeded | the dry-run estimate is above the budget | filter on the partition column, select fewer columns, aggregate; LIMIT does not reduce scanned bytes |

A SELECT can still spend money outside the byte budget through remote
functions, ML. and AI. functions; IAM on connections and models is the
boundary there.

## Results

Rows are objects keyed by column name. Nested records are objects, repeated
fields are arrays, TIMESTAMP is an ISO 8601 UTC string, NUMERIC/BIGNUMERIC
and integers beyond 53 bits are strings (a column may mix numbers and
strings across rows for that reason). When ` + "`truncated`" + ` is true,
` + "`total_rows`" + ` holds BigQuery's full count and ` + "`truncated_by`" + ` says which cap
ended the result — narrow the query or aggregate rather than raising
max_rows.

## Errors

Every error is JSON: ` + "`{code, message, retryable, details}`" + `. When
` + "`retryable`" + ` is true the server already retried once; try again later
or report it. When it is false, retrying the same call cannot help.

%s`
