package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check credentials, the billing project and IAM, naming the first failing step",
	RunE:  runDoctor,
}

func runDoctor(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	cfg, err := resolveConfig(configPath)
	if err != nil {
		return err
	}
	src := cfg.Path
	if src == "" {
		src = "(no config file found — defaults)"
	}
	fmt.Fprintf(out, "config:   %s\n", src)
	if cfg.ProjectID == "" {
		fmt.Fprintln(out, "project:  FAIL — set [project] id in config.toml")
		return errors.New("doctor: no billing project configured")
	}
	fmt.Fprintf(out, "project:  %s\n", cfg.ProjectID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := bq.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), Version)
	if err != nil {
		return err
	}

	// 1. ADC
	if err := client.CheckAuth(ctx); err != nil {
		fmt.Fprintf(out, "adc:      FAIL — %s\n", describe(err))
		return errors.New("doctor: credentials unavailable")
	}
	fmt.Fprintln(out, "adc:      OK (Application Default Credentials found, token obtained)")

	// 2. Billing project reachable
	if _, err := client.ListDatasets(ctx, ""); err != nil {
		fmt.Fprintf(out, "reach:    FAIL — %s\n", describe(err))
		return errors.New("doctor: billing project unreachable")
	}
	fmt.Fprintln(out, "reach:    OK (datasets.list on the billing project answered)")

	// 3. IAM for jobs
	dry, err := client.DryRun(ctx, "SELECT 1 AS doctor", nil)
	if err != nil {
		fmt.Fprintf(out, "jobs:     FAIL — %s\n", describe(err))
		return errors.New("doctor: cannot create query jobs")
	}
	fmt.Fprintf(out, "jobs:     OK (dry run accepted; statementType=%s)\n", dry.StatementType)
	fmt.Fprintln(out, "doctor:   all checks passed")
	return nil
}

func describe(err error) string {
	var te *toolerr.Error
	if errors.As(err, &te) {
		return fmt.Sprintf("[%s] %s", te.Code, te.Message)
	}
	return err.Error()
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
