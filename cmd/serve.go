package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/config"
	"github.com/nlink-jp/bigquery-mcp/internal/logging"
	"github.com/nlink-jp/bigquery-mcp/internal/mcpserver"
	"github.com/nlink-jp/bigquery-mcp/internal/tools"
	"github.com/nlink-jp/bigquery-mcp/internal/transport"
	"github.com/spf13/cobra"
)

// configPath is bound to a persistent flag on rootCmd so both
// `bigquery-mcp serve --config=…` and the bare `bigquery-mcp --config=…`
// pick it up.
var configPath string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the MCP stdio server",
	Long:  "Read JSON-RPC messages from stdin and serve MCP tool calls. This is the default when no subcommand is given.",
	RunE:  runServe,
}

func runServe(cmd *cobra.Command, args []string) error {
	cfg, err := resolveConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.ProjectID == "" {
		return errors.New("no billing project configured: set [project] id in config.toml")
	}

	logger, logFile, err := logging.Setup(cfg.LogLevel, cfg.LogFile)
	if err != nil {
		return err
	}
	if logFile != nil {
		defer logFile.Close()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, err := bq.New(ctx, cfg, logger, Version)
	if err != nil {
		return err
	}

	// stdout is the JSON-RPC channel: nothing else may write to it.
	tr := transport.NewStdioTransport(os.Stdin, os.Stdout)
	srv := mcpserver.New("bigquery-mcp", Version, tr, logger)
	tools.Register(srv, client, cfg, logger)

	logger.Info("bigquery-mcp serving", "project", cfg.ProjectID, "version", Version)
	if err := srv.Serve(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// resolveConfig loads the explicit path if given, then $BIGQUERY_MCP_CONFIG,
// then the default path. A missing default file yields the defaults (and
// serve refuses to start without a project id).
func resolveConfig(explicit string) (*config.Config, error) {
	if explicit != "" {
		return config.Load(explicit)
	}
	if env := os.Getenv("BIGQUERY_MCP_CONFIG"); env != "" {
		return config.Load(env)
	}
	return config.LoadDefault()
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
