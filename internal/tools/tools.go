// Package tools implements the six bigquery-mcp tools (RFP §2).
//
// Every tool is a mcpserver.Tool descriptor plus a (*deps) handler,
// registered in Register, covered by tools_test.go against a fake
// BigQuery, and listed in get_usage. The execution path of query — dry
// run, gate, run, shape — is fixed here and cannot be reordered by a
// caller (ADR-0002, ADR-0003).
package tools

import (
	"encoding/json"
	"log/slog"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/config"
	"github.com/nlink-jp/bigquery-mcp/internal/mcpserver"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

// deps carries the shared dependencies into the handlers.
type deps struct {
	client *bq.Client
	cfg    *config.Config
	logger *slog.Logger
}

// Register registers every tool on the server.
func Register(srv *mcpserver.Server, c *bq.Client, cfg *config.Config, logger *slog.Logger) {
	d := &deps{client: c, cfg: cfg, logger: logger}
	srv.RegisterTool(listDatasetsTool, d.listDatasets)
	srv.RegisterTool(listTablesTool, d.listTables)
	srv.RegisterTool(describeTableTool, d.describeTable)
	srv.RegisterTool(dryRunTool, d.dryRun)
	srv.RegisterTool(queryTool, d.query)
	srv.RegisterTool(getUsageTool, d.getUsage)
}

// parseArgs decodes the tool arguments; unknown fields are rejected so a
// misspelled max_rows never silently falls back to the default.
func parseArgs(args json.RawMessage, into any) error {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytesReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return toolerr.Newf(toolerr.CodeInvalidArguments, "invalid arguments: %v", err)
	}
	return nil
}

// required returns invalid_arguments naming the missing field.
func required(name, value string) error {
	if value == "" {
		return toolerr.Newf(toolerr.CodeInvalidArguments, "%s is required", name)
	}
	return nil
}
