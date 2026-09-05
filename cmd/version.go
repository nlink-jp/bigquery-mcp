package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is overridden at build time via -ldflags "-X .../cmd.Version=<vX.Y.Z>".
var Version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)

	// Every tool in the org answers `--version`, and the shared homebrew
	// formula template tests for it. Setting Version here keeps the
	// linker-injected value as the single source; the template strips
	// cobra's "<name> version " prefix so both spellings print the same.
	rootCmd.Version = Version
	rootCmd.SetVersionTemplate("{{.Version}}\n")
}
