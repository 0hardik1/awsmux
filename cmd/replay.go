package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/0hardik1/awsmux/internal/core"
	"github.com/0hardik1/awsmux/internal/output"
)

// AGENT CONTRACT (cli-plan): implement runReplay.
//
//  1. core.LoadExecution (prefix ok). Rebuild the target list from the old
//     execution's (profile, region) pairs and re-run identity preflight;
//     a profile that no longer exists fails the whole replay with
//     ExitConfigError (say which).
//  2. Re-classify the operation fresh and apply exactly the run.go gates
//     (mutating needs --yes, destructive refuses and points at plan /
//     approve / apply). A replay is a new execution, not a bypass.
//  3. Execute with exec flags, save to history, render in --format, exit
//     with the stable code.

var replayFlags struct {
	exec   execFlags
	format string
	yes    bool
}

var replayCmd = &cobra.Command{
	Use:   "replay <execution-id>",
	Short: "Re-run a past execution against freshly verified targets",
	Args:  cobra.ExactArgs(1),
	RunE:  runReplay,
}

func init() {
	addExecFlags(replayCmd, &replayFlags.exec)
	f := replayCmd.Flags()
	f.StringVar(&replayFlags.format, "format", "table", "table | json | jsonl")
	f.BoolVarP(&replayFlags.yes, "yes", "y", false, "skip confirmation for mutating (never destructive) operations")
	rootCmd.AddCommand(replayCmd)
}

func runReplay(cmd *cobra.Command, args []string) error {
	if !output.ValidFormat(replayFlags.format) {
		return Exitf(core.ExitConfigError, "invalid format %q (want table, json, or jsonl)", replayFlags.format)
	}
	old, err := core.LoadExecution(args[0])
	if err != nil {
		return Exitf(core.ExitConfigError, "load execution: %v", err)
	}
	if len(old.Results) == 0 {
		return Exitf(core.ExitConfigError, "execution %s has no targets to replay", old.ID)
	}

	profiles, err := core.LoadProfiles()
	if err != nil {
		return Exitf(core.ExitConfigError, "load profiles: %v", err)
	}
	known := make(map[string]bool, len(profiles))
	for _, p := range profiles {
		known[p.Name] = true
	}
	targets := make([]core.Target, 0, len(old.Results))
	for _, r := range old.Results {
		if !known[r.Target.Profile] {
			return Exitf(core.ExitConfigError,
				"profile %q from execution %s no longer exists in the AWS config/credentials files", r.Target.Profile, old.ID)
		}
		targets = append(targets, core.NewTarget(r.Target.Profile, r.Target.Region))
	}
	if err := core.ValidateArgs(old.Args); err != nil {
		return Exitf(core.ExitConfigError, "stored execution args: %s", err)
	}
	targets = core.Preflight(cmd.Context(), targets)
	// A failed preflight blocks the replay, it is not just informational.
	if err := core.CheckVerified(targets); err != nil {
		return Exitf(core.ExitConfigError, "%s", err)
	}

	// A replay is a new execution: re-classify fresh and enforce the same
	// gates as `awsmux run`, never trusting the stored classification.
	cls := core.Classify(old.Service, old.Operation)
	switch {
	case cls == core.ClassDestructive:
		return Exitf(core.ExitApprovalRequired,
			"%s %s is destructive; replay will not run it, use `awsmux plan`, `awsmux approve`, then `awsmux apply`",
			old.Service, old.Operation)
	case cls != core.ClassReadOnly && !replayFlags.yes:
		if !stdinIsCharDevice() {
			return Exitf(core.ExitApprovalRequired,
				"%s %s is %s; pass --yes or confirm interactively on a TTY", old.Service, old.Operation, cls)
		}
		fmt.Fprintf(os.Stderr, "replay %s %s (%s) on %d target(s)? type %q to continue: ",
			old.Service, old.Operation, cls, len(targets), "yes")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return Exitf(core.ExitApprovalRequired, "replay rejected")
		}
		if strings.TrimSpace(line) != "yes" {
			return Exitf(core.ExitApprovalRequired, "replay rejected")
		}
	}

	var onResult func(core.TargetResult)
	if replayFlags.format == "jsonl" {
		onResult = func(r core.TargetResult) {
			_ = output.StreamResult(os.Stdout, r)
		}
	}
	e := core.Execute(cmd.Context(), targets, old.Service, old.Operation, old.Args, replayFlags.exec.options(), onResult)
	e.Classification = cls

	if err := core.SaveExecution(e); err != nil {
		fmt.Fprintf(os.Stderr, "awsmux: warning: save execution: %v\n", err)
	}

	switch replayFlags.format {
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

	if code := e.ExitCode(); code != core.ExitOK {
		return Exitf(code, "")
	}
	return nil
}
