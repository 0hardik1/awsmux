package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"awsmux/internal/core"
	"awsmux/internal/output"
)

// AGENT CONTRACT (cli-plan): implement runApply.
//
//  1. core.LoadPlan (prefix ok).
//  2. core.CheckApproval(plan, --approval-token); any failure is
//     ExitApprovalRequired with the reason.
//  3. Execute plan.Targets with exec flags (jsonl streams; see run.go
//     notes). Set plan.Status = PlanExecuted, plan.ExecutionID, SavePlan,
//     SaveExecution.
//  4. Render in --format, summary to stderr in table mode, exit with the
//     execution's stable code.

var applyFlags struct {
	exec          execFlags
	format        string
	outputDir     string
	approvalToken string
}

var applyCmd = &cobra.Command{
	Use:   "apply <plan-id>",
	Short: "Execute an approved plan",
	Args:  cobra.ExactArgs(1),
	RunE:  runApply,
}

func init() {
	addExecFlags(applyCmd, &applyFlags.exec)
	f := applyCmd.Flags()
	f.StringVar(&applyFlags.format, "format", "table", "table | json | jsonl")
	f.StringVar(&applyFlags.outputDir, "output-dir", "", "also write one result file per target into this directory")
	f.StringVar(&applyFlags.approvalToken, "approval-token", "", "token from `awsmux approve` (required unless the plan is read only)")
	rootCmd.AddCommand(applyCmd)
}

func runApply(cmd *cobra.Command, args []string) error {
	if !output.ValidFormat(applyFlags.format) {
		return Exitf(core.ExitConfigError, "invalid format %q (want table, json, or jsonl)", applyFlags.format)
	}
	p, err := core.LoadPlan(args[0])
	if err != nil {
		return Exitf(core.ExitConfigError, "load plan: %v", err)
	}
	if err := core.CheckApproval(p, applyFlags.approvalToken); err != nil {
		return Exitf(core.ExitApprovalRequired, "%v", err)
	}
	// The approval bound specific accounts; refuse to run if any target's
	// live identity no longer matches the plan.
	if err := core.VerifyIdentities(cmd.Context(), p.Targets); err != nil {
		return Exitf(core.ExitApprovalRequired, "%v", err)
	}
	// Cross-process execute-at-most-once gate, shared with the MCP server.
	execID := core.NewID("exec")
	if err := core.ClaimPlan(p.ID, execID); err != nil {
		return Exitf(core.ExitApprovalRequired, "%v", err)
	}

	var onResult func(core.TargetResult)
	if applyFlags.format == "jsonl" {
		onResult = func(r core.TargetResult) {
			_ = output.StreamResult(os.Stdout, r)
		}
	}
	e := core.Execute(cmd.Context(), p.Targets, p.Service, p.Operation, p.Args, applyFlags.exec.options(), onResult)
	e.ID = execID // the id the claim reserved, not the executor-minted one
	e.PlanID = p.ID
	e.Classification = p.Classification

	// Persist the plan transition and the execution even on partial failure;
	// the run happened either way.
	p.Status = core.PlanExecuted
	p.ExecutionID = e.ID
	if err := core.SavePlan(p); err != nil {
		fmt.Fprintf(os.Stderr, "awsmux: warning: save plan status: %v\n", err)
	}
	if err := core.SaveExecution(e); err != nil {
		fmt.Fprintf(os.Stderr, "awsmux: warning: save execution: %v\n", err)
	}

	switch applyFlags.format {
	case "table":
		if err := output.RenderExecution(os.Stdout, e, "table"); err != nil {
			fmt.Fprintf(os.Stderr, "awsmux: warning: render execution: %v\n", err)
		}
		output.RenderSummary(os.Stderr, e)
	case "json":
		if err := output.RenderExecution(os.Stdout, e, "json"); err != nil {
			fmt.Fprintf(os.Stderr, "awsmux: warning: render execution: %v\n", err)
		}
	case "jsonl":
		// Results already streamed; close the stream with the same final
		// summary line RenderExecution's jsonl mode would write.
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Summary core.Summary `json:"summary"`
		}{e.Summary})
	}

	if applyFlags.outputDir != "" {
		if err := output.WriteOutputDir(applyFlags.outputDir, e); err != nil {
			fmt.Fprintf(os.Stderr, "awsmux: warning: write output dir: %v\n", err)
		}
	}

	if code := e.ExitCode(); code != core.ExitOK {
		return Exitf(code, "")
	}
	return nil
}
