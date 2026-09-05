package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/mcpserver"
)

var listDatasetsTool = mcpserver.Tool{
	Name:        "list_datasets",
	Description: "List the datasets of a project (the billing project when omitted). Discovery only: nothing is billed and IAM decides what is visible.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{
		"project":{"type":"string","description":"Project to list; defaults to the configured billing project."}
	}}`),
}

type listDatasetsArgs struct {
	Project string `json:"project"`
}

func (d *deps) listDatasets(ctx context.Context, args json.RawMessage) (any, error) {
	var a listDatasetsArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	ds, err := d.client.ListDatasets(ctx, a.Project)
	if err != nil {
		return nil, err
	}
	if ds == nil {
		ds = []bq.Dataset{}
	}
	project := a.Project
	if project == "" {
		project = d.client.Project()
	}
	return map[string]any{"project": project, "datasets": ds, "count": len(ds)}, nil
}

var listTablesTool = mcpserver.Tool{
	Name:        "list_tables",
	Description: "List the tables and views of a dataset with their kind and partition column. Discovery only; nothing is billed.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{
		"dataset":{"type":"string","description":"Dataset id."},
		"project":{"type":"string","description":"Project of the dataset; defaults to the configured billing project."}
	},"required":["dataset"]}`),
}

type listTablesArgs struct {
	Dataset string `json:"dataset"`
	Project string `json:"project"`
}

func (d *deps) listTables(ctx context.Context, args json.RawMessage) (any, error) {
	var a listTablesArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if err := required("dataset", a.Dataset); err != nil {
		return nil, err
	}
	ts, err := d.client.ListTables(ctx, a.Project, a.Dataset)
	if err != nil {
		return nil, err
	}
	if ts == nil {
		ts = []bq.TableSummary{}
	}
	project := a.Project
	if project == "" {
		project = d.client.Project()
	}
	return map[string]any{"project": project, "dataset": a.Dataset, "tables": ts, "count": len(ts)}, nil
}

var describeTableTool = mcpserver.Tool{
	Name:        "describe_table",
	Description: "Describe a table: schema (nested fields included), partition column and kind, clustering, row count, size, partition expiration and timestamps. Read this before writing SQL against a table: filtering on the partition column is what keeps a query inside the budget.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{
		"dataset":{"type":"string","description":"Dataset id."},
		"table":{"type":"string","description":"Table or view id."},
		"project":{"type":"string","description":"Project of the dataset; defaults to the configured billing project."}
	},"required":["dataset","table"]}`),
}

type describeTableArgs struct {
	Dataset string `json:"dataset"`
	Table   string `json:"table"`
	Project string `json:"project"`
}

type partitionOut struct {
	Column         string  `json:"column"`
	Kind           string  `json:"kind"`
	Granularity    string  `json:"granularity,omitempty"`
	ExpirationDays float64 `json:"expiration_days,omitempty"`
	RequireFilter  bool    `json:"require_filter"`
}

func (d *deps) describeTable(ctx context.Context, args json.RawMessage) (any, error) {
	var a describeTableArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if err := required("dataset", a.Dataset); err != nil {
		return nil, err
	}
	if err := required("table", a.Table); err != nil {
		return nil, err
	}
	info, err := d.client.GetTable(ctx, a.Project, a.Dataset, a.Table)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"table":       info.TableReference.String(),
		"type":        info.Type,
		"schema":      schemaOut(info.Schema),
		"num_rows":    parseInt(info.NumRows),
		"num_bytes":   parseInt(info.NumBytes),
		"size":        humanBytes(parseInt(info.NumBytes)),
		"created":     msToRFC3339(info.CreationTime),
		"modified":    msToRFC3339(info.LastModifiedTime),
		"location":    info.Location,
		"description": strings.TrimSpace(info.Description),
	}
	if info.ExpirationTime != "" {
		out["expires"] = msToRFC3339(info.ExpirationTime)
	}
	if col, kind := info.PartitionColumn(); col != "" {
		p := partitionOut{Column: col, Kind: kind, RequireFilter: info.RequirePartitionFilter}
		if info.TimePartitioning != nil {
			p.Granularity = info.TimePartitioning.Type
			p.ExpirationDays = daysFromMs(info.TimePartitioning.ExpirationMs)
			p.RequireFilter = p.RequireFilter || info.TimePartitioning.RequirePartitionFilter
		}
		out["partition"] = p
	}
	if info.Clustering != nil && len(info.Clustering.Fields) > 0 {
		out["clustering"] = info.Clustering.Fields
	}
	if info.View != nil && info.View.Query != "" {
		out["view_query"] = info.View.Query
	}
	return out, nil
}
