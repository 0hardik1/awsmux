package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/0hardik1/awsmux/internal/core"
	"github.com/0hardik1/awsmux/internal/output"
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
	if err := targetsFlags.sel.validate(cmd); err != nil {
		return err
	}
	sel := targetsFlags.sel.selector()
	targets, orgSel, err := core.ResolveTargetsWithOrg(cmd.Context(), sel)
	if err != nil {
		// The zero-match error text hints at --show-unreachable, and that
		// hint is only honorable because ResolveTargetsWithOrg still
		// returns the computed OrgSelection alongside the error. Render the
		// unreachable list before returning so the hint has something to
		// point at, rather than guarding the error away first.
		if orgSel != nil && targetsFlags.sel.showUnreachable {
			_ = output.RenderUnreachable(os.Stderr, orgSel.Unreachable, targetsFlags.sel.showUnreachable)
		}
		return Exitf(core.ExitConfigError, "%s", err)
	}
	if len(targets) == 0 {
		return Exitf(core.ExitConfigError, "no profiles matched the selection")
	}
	if err := output.RenderTargets(os.Stdout, targets, targetsFlags.format); err != nil {
		return fmt.Errorf("rendering targets: %w", err)
	}
	// Coverage goes to stderr so it never contaminates json or jsonl stdout,
	// which agents and CI parse.
	if orgSel != nil {
		if err := output.RenderUnreachable(os.Stderr, orgSel.Unreachable, targetsFlags.sel.showUnreachable); err != nil {
			return fmt.Errorf("rendering unreachable accounts: %w", err)
		}
	}
	if sel.Preflight || sel.Dedupe || sel.UsesOrg() {
		for _, t := range targets {
			if t.PreflightErr != "" {
				fmt.Fprintf(os.Stderr, "awsmux: preflight failed for %s: %s\n", t.ID, t.PreflightErr)
			}
		}
	}
	return nil
}
