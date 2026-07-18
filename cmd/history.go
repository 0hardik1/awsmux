package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"awsmux/internal/core"
	"awsmux/internal/output"
)

// AGENT CONTRACT (cli-plan): implement runHistory and runHistoryShow.
//
// runHistory: core.ListExecutions, table to stdout, newest first:
// ID, WHEN (relative, e.g. "2h ago"), SERVICE OPERATION, CLASS, TARGETS,
// OK/FAIL, DURATION. Empty history prints a friendly line, exit 0.
//
// runHistoryShow: core.LoadExecution (prefix ok), render with
// output.RenderExecution in --format.

var historyShowFlags struct {
	format string
}

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "List past executions",
	Args:  cobra.NoArgs,
	RunE:  runHistory,
}

var historyShowCmd = &cobra.Command{
	Use:   "show <execution-id>",
	Short: "Show one execution in full",
	Args:  cobra.ExactArgs(1),
	RunE:  runHistoryShow,
}

func init() {
	historyShowCmd.Flags().StringVar(&historyShowFlags.format, "format", "table", "table | json | jsonl")
	historyCmd.AddCommand(historyShowCmd)
	rootCmd.AddCommand(historyCmd)
}

// coarseDuration renders d in a single coarse unit: s, m, h, or d.
func coarseDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func runHistory(cmd *cobra.Command, args []string) error {
	execs, err := core.ListExecutions()
	if err != nil {
		return Exitf(core.ExitConfigError, "list executions: %v", err)
	}
	if len(execs) == 0 {
		fmt.Println("no executions yet; run something with `awsmux run`")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tWHEN\tSERVICE OPERATION\tCLASS\tTARGETS\tOK/FAIL\tDURATION")
	for _, e := range execs {
		when := coarseDuration(time.Since(e.StartedAt)) + " ago"
		dur := e.FinishedAt.Sub(e.StartedAt)
		if dur < 0 {
			dur = 0
		}
		fmt.Fprintf(w, "%s\t%s\t%s %s\t%s\t%d\t%d/%d\t%s\n",
			e.ID, when, e.Service, e.Operation, e.Classification,
			e.Summary.Total, e.Summary.Succeeded, e.Summary.Failed,
			dur.Round(100*time.Millisecond))
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("render history: %w", err)
	}
	return nil
}

func runHistoryShow(cmd *cobra.Command, args []string) error {
	if !output.ValidFormat(historyShowFlags.format) {
		return Exitf(core.ExitConfigError, "invalid format %q (want table, json, or jsonl)", historyShowFlags.format)
	}
	e, err := core.LoadExecution(args[0])
	if err != nil {
		return Exitf(core.ExitConfigError, "load execution: %v", err)
	}
	if err := output.RenderExecution(os.Stdout, e, historyShowFlags.format); err != nil {
		return fmt.Errorf("render execution: %w", err)
	}
	if historyShowFlags.format == "table" {
		output.RenderSummary(os.Stderr, e)
	}
	return nil
}
