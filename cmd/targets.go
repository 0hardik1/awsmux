package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"awsmux/internal/core"
	"awsmux/internal/output"
)

var targetsFlags struct {
	sel    selectorFlags
	format string
}

var targetsCmd = &cobra.Command{
	Use:   "targets",
	Short: "Discover and verify execution targets (profiles x regions)",
	Args:  cobra.NoArgs,
	RunE:  runTargets,
}

func init() {
	addSelectorFlags(targetsCmd, &targetsFlags.sel)
	targetsCmd.Flags().StringVar(&targetsFlags.format, "format", "table", "table | json | jsonl")
	rootCmd.AddCommand(targetsCmd)
}

func runTargets(cmd *cobra.Command, args []string) error {
	if !output.ValidFormat(targetsFlags.format) {
		return Exitf(core.ExitConfigError, "invalid --format %q (want table, json, or jsonl)", targetsFlags.format)
	}
	sel := targetsFlags.sel.selector()
	targets, err := core.ResolveTargets(cmd.Context(), sel)
	if err != nil {
		return Exitf(core.ExitConfigError, "%s", err)
	}
	if len(targets) == 0 {
		return Exitf(core.ExitConfigError, "no profiles matched the selection")
	}
	if err := output.RenderTargets(os.Stdout, targets, targetsFlags.format); err != nil {
		return fmt.Errorf("rendering targets: %w", err)
	}
	if sel.Preflight || sel.Dedupe {
		for _, t := range targets {
			if t.PreflightErr != "" {
				fmt.Fprintf(os.Stderr, "awsmux: preflight failed for %s: %s\n", t.ID, t.PreflightErr)
			}
		}
	}
	return nil
}
