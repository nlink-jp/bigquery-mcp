package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "bigquery-mcp",
	Short: "Protection-first BigQuery MCP server",
	Long: `bigquery-mcp exposes BigQuery to MCP clients as a local stdio server,
using your own Application Default Credentials.

Every query is dry-run first: the statement must be a SELECT, must stay
inside the configured dataset allowlist, and must fit the byte budget
before it runs. Results come back as column-keyed rows with explicit caps
and truncation accounting; errors carry a stable code, a retryable flag
and machine-readable details.

One server instance serves one billing project; register the binary
several times with different --config paths for several projects.

When invoked with no subcommand, behaves like ` + "`bigquery-mcp serve`" + ` and reads JSON-RPC messages from stdin.`,
	// Don't dump the usage help on RunE errors; cobra still prints "Error: ..." to stderr.
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe(cmd, args)
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "",
		"Path to config.toml (default: $BIGQUERY_MCP_CONFIG, then ~/.config/bigquery-mcp/config.toml)")
}

// Execute runs the root command; cobra prints the error, we set the exit code.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		_ = err
		os.Exit(1)
	}
}
