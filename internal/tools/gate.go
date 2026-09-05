package tools

import (
	"strings"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/config"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

// billedEstimate approximates what BigQuery would bill for the dry-run
// estimate under on-demand pricing: bytes are rounded up to the next MiB,
// and every referenced table costs at least 10 MiB (review finding 8).
func billedEstimate(dry *bq.DryRunResult) int64 {
	const mib = int64(1 << 20)
	b := dry.TotalBytesProcessed
	if b%mib != 0 {
		b = (b/mib + 1) * mib
	}
	if minimum := config.MinBytesBilled * int64(max(len(dry.ReferencedTables), 1)); b < minimum {
		b = minimum
	}
	return b
}

// gate is the three checks of ADR-0002, in order. It returns nil when the
// query may run. The verdict is a *toolerr.Error so dry_run can render it
// as data and query can return it as the error.
func gate(cfg *config.Config, dry *bq.DryRunResult) *toolerr.Error {
	if dry.StatementType != "SELECT" {
		st := dry.StatementType
		if st == "" {
			st = "(unknown)"
		}
		return toolerr.Newf(toolerr.CodeStatementNotAllowed,
			"only SELECT statements run here; BigQuery classified this statement as %s. Rewrite it as a single SELECT (no scripts, DML, DDL or EXPORT)", st).
			WithDetails(map[string]any{"statement_type": dry.StatementType})
	}
	if len(cfg.Datasets) > 0 {
		for _, t := range dry.ReferencedTables {
			if !datasetAllowed(cfg.Datasets, t.ProjectID, t.DatasetID) {
				return notAllowed(cfg, "table", t.String())
			}
		}
		// A table function or UDF can read tables its own dataset owns;
		// the routine's dataset is held to the same list (review finding 4).
		for _, r := range dry.ReferencedRoutines {
			if !datasetAllowed(cfg.Datasets, r.ProjectID, r.DatasetID) {
				return notAllowed(cfg, "routine", r.String())
			}
		}
	}
	if billed := billedEstimate(dry); billed > cfg.MaxBytesBilled {
		return toolerr.Newf(toolerr.CodeBudgetExceeded,
			"the dry run estimates %s processed (about %s billed) but the budget is %s; nothing ran. Filter on the partition column, select fewer columns, or aggregate — LIMIT does not reduce scanned bytes. The operator can raise [budget] max_bytes_billed",
			humanBytes(dry.TotalBytesProcessed), humanBytes(billed), humanBytes(cfg.MaxBytesBilled)).
			WithDetails(map[string]any{"bytes_processed": dry.TotalBytesProcessed, "bytes_billed_estimate": billed, "budget_bytes": cfg.MaxBytesBilled, "accuracy": dry.Accuracy})
	}
	return nil
}

func notAllowed(cfg *config.Config, kind, name string) *toolerr.Error {
	return toolerr.Newf(toolerr.CodeDatasetNotAllowed,
		"%s %s is outside the datasets this server may read (%s); query only the allowed datasets or ask the operator to extend [access] datasets",
		kind, name, strings.Join(cfg.Datasets, ", ")).
		WithDetails(map[string]any{kind: name, "allowed": cfg.Datasets})
}

// datasetAllowed matches project.dataset against the allowlist patterns
// ("project.dataset" exact, or "project.*"). Comparison is exact-case:
// BigQuery dataset ids are case-sensitive.
func datasetAllowed(patterns []string, project, dataset string) bool {
	for _, p := range patterns {
		pp, pd, ok := config.SplitDataset(p)
		if !ok {
			continue
		}
		if pp == project && (pd == "*" || pd == dataset) {
			return true
		}
	}
	return false
}
