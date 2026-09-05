package tools

import (
	"context"
	"fmt"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
)

// maxWarningLookups bounds the tables.get calls spent on advice.
const maxWarningLookups = 5

// partitionWarnings returns advisory lines derived from numbers, never
// from the SQL text (RFP §3.3, review finding 7):
//
//   - a referenced table is partitioned and the estimate covers its whole
//     size — the partition column pruned nothing;
//   - the estimate is not PRECISE — the kernel cap may still stop the job.
//
// Advice only: the budget is the rule. Lookups are bounded and a failed
// lookup is skipped, because a warning must never fail a query.
func (d *deps) partitionWarnings(ctx context.Context, dry *bq.DryRunResult) []string {
	var out []string
	if dry.Accuracy != "" && dry.Accuracy != "PRECISE" {
		out = append(out, fmt.Sprintf("the byte estimate is %s, not precise: the run may still be stopped by maximumBytesBilled", dry.Accuracy))
	}
	var sizes []int64
	var infos []*bq.TableInfo
	for i, ref := range dry.ReferencedTables {
		if i >= maxWarningLookups {
			break
		}
		info, err := d.client.GetTable(ctx, ref.ProjectID, ref.DatasetID, ref.TableID)
		if err != nil {
			continue
		}
		infos = append(infos, info)
		sizes = append(sizes, parseInt(info.NumBytes))
	}
	var total int64
	for _, s := range sizes {
		total += s
	}
	for i, info := range infos {
		col, kind := info.PartitionColumn()
		if col == "" || sizes[i] <= 0 {
			continue
		}
		// One table: the estimate equals its size when nothing was pruned.
		// Several: only say so when the estimate covers all of them.
		fullScan := dry.TotalBytesProcessed >= sizes[i]
		if len(infos) > 1 {
			fullScan = dry.TotalBytesProcessed >= total
		}
		if !fullScan {
			continue
		}
		hint := "a WHERE on that column prunes partitions and cuts bytes"
		if info.RequirePartitionFilter {
			hint = "it requires a partition filter, so BigQuery will refuse the query without one"
		}
		out = append(out, fmt.Sprintf("%s is %s-partitioned by %s but the estimate covers the whole table (%s) — %s",
			info.TableReference.String(), kind, col, humanBytes(sizes[i]), hint))
	}
	return out
}
