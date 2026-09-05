//go:build live

package tools

// Live end-to-end tests against a real project. Opt-in:
//
//	BIGQUERY_MCP_TEST_PROJECT=<billing project> [BIGQUERY_MCP_TEST_DATASET=<dataset>] make live-test
//
// The project id comes from the environment and is never committed. The
// over-budget and statement checks use a public dataset so that no run is
// billed: dry runs are free, and the gate refuses before anything runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/config"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

const publicBig = "`bigquery-public-data.samples.wikipedia`" // ~35 GB: always over a 10 MiB budget

func liveDeps(t *testing.T) (*deps, context.Context) {
	t.Helper()
	project := os.Getenv("BIGQUERY_MCP_TEST_PROJECT")
	if project == "" {
		t.Skip("BIGQUERY_MCP_TEST_PROJECT not set")
	}
	cfg := config.Default()
	cfg.ProjectID = project
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := bq.New(ctx, cfg, logger, "live-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckAuth(ctx); err != nil {
		t.Fatalf("ADC: %v", err)
	}
	return &deps{client: c, cfg: cfg, logger: logger}, ctx
}

func liveCall(t *testing.T, fn func(context.Context, json.RawMessage) (any, error), ctx context.Context, args string) (map[string]any, *toolerr.Error) {
	t.Helper()
	out, err := fn(ctx, json.RawMessage(args))
	if err != nil {
		te, ok := err.(*toolerr.Error)
		if !ok {
			t.Fatalf("non-contract error: %v", err)
		}
		return nil, te
	}
	b, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, nil
}

func TestLiveDiscoveryAndQuery(t *testing.T) {
	d, ctx := liveDeps(t)
	m, te := liveCall(t, d.listDatasets, ctx, `{}`)
	if te != nil {
		t.Fatalf("list_datasets: %v", te)
	}
	t.Logf("list_datasets: %v datasets", m["count"])

	dataset := os.Getenv("BIGQUERY_MCP_TEST_DATASET")
	if dataset == "" {
		t.Skip("BIGQUERY_MCP_TEST_DATASET not set; discovery only")
	}
	m, te = liveCall(t, d.listTables, ctx, fmt.Sprintf(`{"dataset":%q}`, dataset))
	if te != nil {
		t.Fatalf("list_tables: %v", te)
	}
	tables, _ := m["tables"].([]any)
	if len(tables) == 0 {
		t.Skip("dataset has no tables")
	}
	table := tables[0].(map[string]any)["table_id"].(string)
	t.Logf("list_tables: %d tables; using %s (partition column %q)", len(tables), table, tables[0].(map[string]any)["partition_column"])

	m, te = liveCall(t, d.describeTable, ctx, fmt.Sprintf(`{"dataset":%q,"table":%q}`, dataset, table))
	if te != nil {
		t.Fatalf("describe_table: %v", te)
	}
	t.Logf("describe_table: %v rows, %v, partition %v", m["num_rows"], m["size"], m["partition"])

	fq := fmt.Sprintf("`%s.%s.%s`", d.cfg.ProjectID, dataset, table)
	m, te = liveCall(t, d.dryRun, ctx, fmt.Sprintf(`{"query":"SELECT COUNT(*) AS n FROM %s"}`, fq))
	if te != nil {
		t.Fatalf("dry_run: %v", te)
	}
	if m["allowed"] != true || m["statement_type"] != "SELECT" {
		t.Errorf("dry_run verdict: %v", m)
	}
	t.Logf("dry_run: %v bytes (%v, accuracy %v), tables %v, warnings %v", m["bytes_processed"], m["bytes_human"], m["accuracy"], m["referenced_tables"], m["warnings"])

	m, te = liveCall(t, d.query, ctx, fmt.Sprintf(`{"query":"SELECT COUNT(*) AS n FROM %s","max_rows":5}`, fq))
	if te != nil {
		t.Fatalf("query: %v", te)
	}
	rows, _ := m["rows"].([]any)
	if len(rows) != 1 || m["truncated"] != false || m["job_id"] == "" || !strings.HasPrefix(m["job_id"].(string), "bqmcp-") {
		t.Errorf("query result: %v", m)
	}
	t.Logf("query: rows=%v total=%v bytes_processed=%v bytes_billed=%v cache=%v job=%v location=%v slot_ms=%v",
		rows, m["total_rows"], m["bytes_processed"], m["bytes_billed"], m["cache_hit"], m["job_id"], m["location"], m["slot_ms"])

	// Truncation by max_rows with a real multi-row result and a real page.
	m, te = liveCall(t, d.query, ctx, fmt.Sprintf(`{"query":"SELECT * FROM %s LIMIT 7","max_rows":3}`, fq))
	if te != nil {
		t.Fatalf("query (truncate): %v", te)
	}
	if m["truncated"] != true || m["truncated_by"] != "max_rows" || m["returned_rows"] != float64(3) {
		t.Errorf("truncation accounting: truncated=%v by=%v returned=%v total=%v", m["truncated"], m["truncated_by"], m["returned_rows"], m["total_rows"])
	}
	t.Logf("truncation: %v", m["note"])

	// A typed parameter round-trips.
	m, te = liveCall(t, d.query, ctx, `{"query":"SELECT @d AS d, @n AS n, @s AS s","params":{"d":{"type":"DATE","value":"2026-09-05"},"n":3,"s":"x"}}`)
	if te != nil {
		t.Fatalf("query (params): %v", te)
	}
	row := m["rows"].([]any)[0].(map[string]any)
	if row["d"] != "2026-09-05" || row["n"] != float64(3) || row["s"] != "x" {
		t.Errorf("params round trip: %v", row)
	}
}

func TestLiveGateRefusesWithoutRunning(t *testing.T) {
	d, ctx := liveDeps(t)

	// Over budget: the estimate for a 35 GB public table is above 10 MiB.
	d.cfg.MaxBytesBilled = config.MinBytesBilled
	_, te := liveCall(t, d.query, ctx, fmt.Sprintf(`{"query":"SELECT title FROM %s"}`, publicBig))
	if te == nil || te.Code != toolerr.CodeBudgetExceeded {
		t.Fatalf("expected budget_exceeded, got %v", te)
	}
	t.Logf("budget_exceeded: %s | details %v", te.Message, te.Details)
	d.cfg.MaxBytesBilled = config.DefaultMaxBytesBilled

	// dry_run on the same query reports the verdict as data and a full-scan warning.
	m, te2 := liveCall(t, d.dryRun, ctx, fmt.Sprintf(`{"query":"SELECT title FROM %s"}`, publicBig))
	if te2 != nil {
		t.Fatalf("dry_run: %v", te2)
	}
	t.Logf("dry_run public: bytes=%v accuracy=%v allowed=%v warnings=%v", m["bytes_human"], m["accuracy"], m["allowed"], m["warnings"])

	// A script is classified SCRIPT and refused.
	_, te = liveCall(t, d.query, ctx, `{"query":"DECLARE x INT64 DEFAULT 1; SELECT x;"}`)
	if te == nil || te.Code != toolerr.CodeStatementNotAllowed {
		t.Fatalf("expected statement_not_allowed, got %v", te)
	}
	t.Logf("statement_not_allowed: %v", te.Details)

	// DML against a table the credential cannot write: BigQuery checks the
	// permission before classifying, so the refusal is access_denied; the
	// query still never runs.
	_, te = liveCall(t, d.query, ctx, fmt.Sprintf(`{"query":"DELETE FROM %s WHERE true"}`, publicBig))
	if te == nil || (te.Code != toolerr.CodeStatementNotAllowed && te.Code != toolerr.CodeAccessDenied) {
		t.Fatalf("expected statement_not_allowed or access_denied for DML on a public table, got %v", te)
	}
	// DML against our own table is classified and refused by the gate
	// (a dry run writes nothing).
	if dataset := os.Getenv("BIGQUERY_MCP_TEST_DATASET"); dataset != "" {
		m, te2 := liveCall(t, d.listTables, ctx, fmt.Sprintf(`{"dataset":%q}`, dataset))
		if te2 == nil {
			if tables, _ := m["tables"].([]any); len(tables) > 0 {
				own := fmt.Sprintf("`%s.%s.%s`", d.cfg.ProjectID, dataset, tables[0].(map[string]any)["table_id"])
				_, te = liveCall(t, d.query, ctx, fmt.Sprintf(`{"query":"DELETE FROM %s WHERE false"}`, own))
				if te == nil || te.Code != toolerr.CodeStatementNotAllowed {
					t.Fatalf("expected statement_not_allowed for DML on an own table, got %v", te)
				}
				t.Logf("DML on own table: %v", te.Details)
			}
		}
	}

	// Allowlist: a public table outside the list is refused.
	d.cfg.Datasets = []string{d.cfg.ProjectID + ".*"}
	_, te = liveCall(t, d.query, ctx, fmt.Sprintf(`{"query":"SELECT 1 FROM %s LIMIT 1"}`, publicBig))
	if te == nil || te.Code != toolerr.CodeDatasetNotAllowed {
		t.Fatalf("expected dataset_not_allowed, got %v", te)
	}
	d.cfg.Datasets = nil

	// Invalid SQL carries BigQuery's location.
	_, te = liveCall(t, d.dryRun, ctx, `{"query":"SELECT FROM WHERE"}`)
	if te == nil || te.Code != toolerr.CodeInvalidQuery {
		t.Fatalf("expected invalid_query, got %v", te)
	}
	t.Logf("invalid_query: %s | %v", te.Message, te.Details)

	// Not found.
	_, te = liveCall(t, d.listTables, ctx, `{"dataset":"no_such_dataset_bqmcp"}`)
	if te == nil || te.Code != toolerr.CodeNotFound {
		t.Fatalf("expected not_found, got %v", te)
	}
}
