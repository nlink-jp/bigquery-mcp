package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/mcpserver"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

// paramsSchema is the one object in this server that is deliberately OPEN.
// Its keys are the caller's own query-parameter names — @name in the SQL — so
// they cannot be enumerated in a schema, and additionalProperties:true is what
// makes them legal. Each tool's TOP-LEVEL schema is closed instead
// (organization ADR-021 §10); do not "fix" the nested true to match.
// TestParamsObjectStaysOpen pins it.
const paramsSchema = `"params":{"type":"object","description":"Named query parameters referenced as @name in the SQL. A value is a string, number or boolean (typed as STRING, INT64/FLOAT64, BOOL), or {\"type\": \"DATE\", \"value\": \"2026-09-05\"} for the other scalar types (DATE, TIMESTAMP, DATETIME, TIME, NUMERIC, BIGNUMERIC, BYTES, GEOGRAPHY, JSON). Use literals, not parameters, for partition filters: a parameterised filter may not prune at dry-run time.","additionalProperties":true}`

var dryRunTool = mcpserver.Tool{
	Name:        "dry_run",
	Description: "Estimate a query without running it: bytes that would be processed, the statement type, the referenced tables, the result schema, and whether the query would pass this server's gate (SELECT only, dataset allowlist, byte budget). query runs the same check itself; call this to preview cost or to see why a query is refused.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{
		"query":{"type":"string","description":"GoogleSQL text."},
		` + paramsSchema + `
	},"required":["query"],"additionalProperties":false}`),
}

type sqlArgs struct {
	Query   string                     `json:"query"`
	Params  map[string]json.RawMessage `json:"params"`
	MaxRows *int                       `json:"max_rows"`
}

func (d *deps) dryRun(ctx context.Context, args json.RawMessage) (any, error) {
	var a sqlArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.MaxRows != nil {
		return nil, toolerr.New(toolerr.CodeInvalidArguments, "dry_run takes no max_rows")
	}
	if err := required("query", a.Query); err != nil {
		return nil, err
	}
	params, err := buildParams(a.Params)
	if err != nil {
		return nil, err
	}
	dry, err := d.client.DryRun(ctx, a.Query, params)
	if err != nil {
		return nil, err
	}
	d.logQuery("dry_run", a.Query, dry.StatementType, dry.TotalBytesProcessed, "", nil)
	billed := billedEstimate(dry)
	out := map[string]any{
		"bytes_processed":       dry.TotalBytesProcessed,
		"bytes_human":           humanBytes(dry.TotalBytesProcessed),
		"bytes_billed_estimate": billed,
		"accuracy":              dry.Accuracy,
		"statement_type":        dry.StatementType,
		"referenced_tables":     tableNames(dry.ReferencedTables),
		"schema":                schemaOut(dry.Schema),
		"budget_bytes":          d.cfg.MaxBytesBilled,
		"within_budget":         billed <= d.cfg.MaxBytesBilled,
		"location":              dry.Location,
	}
	if len(dry.ReferencedRoutines) > 0 {
		out["referenced_routines"] = routineNames(dry.ReferencedRoutines)
	}
	if len(dry.UndeclaredParams) > 0 {
		out["undeclared_params"] = paramsOut(dry.UndeclaredParams)
	}
	if verdict := gate(d.cfg, dry); verdict != nil {
		out["allowed"] = false
		out["denied_by"] = verdict.Code
		out["denied_message"] = verdict.Message
	} else {
		out["allowed"] = true
	}
	if w := d.partitionWarnings(ctx, dry); len(w) > 0 {
		out["warnings"] = w
	}
	return out, nil
}

var queryTool = mcpserver.Tool{
	Name:        "query",
	Description: "Run a read-only GoogleSQL query and return rows as column-keyed objects. The query is always dry-run first and refused unless it is a single SELECT, within the dataset allowlist, and within the byte budget; the run carries maximumBytesBilled and a job timeout. Rows stop at max_rows or the response byte budget, and the result then says truncated: true with total_rows — narrow the query or aggregate in SQL rather than paging through everything.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{
		"query":{"type":"string","description":"GoogleSQL SELECT text."},
		` + paramsSchema + `,
		"max_rows":{"type":"integer","minimum":1,"description":"Rows to return at most (default and ceiling come from the server config)."}
	},"required":["query"],"additionalProperties":false}`),
}

func (d *deps) query(ctx context.Context, args json.RawMessage) (any, error) {
	var a sqlArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if err := required("query", a.Query); err != nil {
		return nil, err
	}
	maxRows := d.cfg.DefaultMaxRows
	if a.MaxRows != nil {
		switch {
		case *a.MaxRows < 1:
			return nil, toolerr.New(toolerr.CodeInvalidArguments, "max_rows must be at least 1")
		case *a.MaxRows > d.cfg.HardMaxRows:
			return nil, toolerr.Newf(toolerr.CodeInvalidArguments, "max_rows %d is above this server's ceiling of %d; aggregate or filter in SQL instead", *a.MaxRows, d.cfg.HardMaxRows).
				WithDetails(map[string]any{"hard_max_rows": d.cfg.HardMaxRows})
		}
		maxRows = *a.MaxRows
	}
	params, err := buildParams(a.Params)
	if err != nil {
		return nil, err
	}

	// Dry run → gate → run: the order is the product (ADR-0002).
	dry, err := d.client.DryRun(ctx, a.Query, params)
	if err != nil {
		return nil, err
	}
	if verdict := gate(d.cfg, dry); verdict != nil {
		d.logQuery("refused", a.Query, dry.StatementType, dry.TotalBytesProcessed, verdict.Code, nil)
		return nil, verdict
	}
	warnings := d.partitionWarnings(ctx, dry)

	sh := newShaper(maxRows, d.cfg.MaxBytes)
	pageSize := maxRows
	if pageSize > 5000 {
		pageSize = 5000
	}
	start := time.Now()
	res, err := d.client.Query(ctx, bq.QueryOptions{
		SQL:            a.Query,
		Params:         params,
		MaxBytesBilled: d.cfg.MaxBytesBilled,
		JobTimeout:     d.cfg.JobTimeout,
		PageSize:       pageSize,
		Sink:           sh.sink,
	})
	if err != nil {
		d.logQuery("failed", a.Query, dry.StatementType, dry.TotalBytesProcessed, asCode(err), nil)
		return nil, err
	}
	d.logQuery("ok", a.Query, res.StatementType, res.BytesProcessed, "", map[string]any{
		"job_id": res.JobID, "rows": len(sh.rows), "total_rows": res.TotalRows, "bytes_billed": res.BytesBilled, "ms": time.Since(start).Milliseconds(),
	})

	truncated := sh.truncatedBy != "" || int64(len(sh.rows)) < res.TotalRows
	by := sh.truncatedBy
	if truncated && by == "" {
		by = "max_rows"
	}
	rows := sh.rows
	if rows == nil {
		rows = []json.RawMessage{}
	}
	out := map[string]any{
		"rows":            rows,
		"schema":          schemaOut(res.Schema),
		"total_rows":      res.TotalRows,
		"returned_rows":   len(rows),
		"truncated":       truncated,
		"bytes_processed": res.BytesProcessed,
		"bytes_billed":    res.BytesBilled,
		"cache_hit":       res.CacheHit,
		"job_id":          res.JobID,
		"location":        res.Location,
		"slot_ms":         res.SlotMs,
	}
	if truncated {
		out["truncated_by"] = by
		out["note"] = fmt.Sprintf("%d of %d rows returned (stopped by %s). Narrow the query with WHERE, aggregate in SQL, or raise max_rows up to %d.",
			len(rows), res.TotalRows, by, d.cfg.HardMaxRows)
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out, nil
}

func asCode(err error) string {
	if te, ok := err.(*toolerr.Error); ok {
		return te.Code
	}
	return "error"
}

// logQuery writes one structured line per query; the SQL text itself only
// when the operator opted in (RFP §3.10).
func (d *deps) logQuery(outcome, sql, statementType string, bytes int64, code string, extra map[string]any) {
	attrs := []any{"outcome", outcome, "statement_type", statementType, "bytes_processed", bytes}
	if code != "" {
		attrs = append(attrs, "code", code)
	}
	for k, v := range extra {
		attrs = append(attrs, k, v)
	}
	if d.cfg.LogQueries {
		attrs = append(attrs, "sql", sql)
	}
	d.logger.Info("query", attrs...)
}
