package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/0hardik1/awsmux/internal/core"
	"github.com/0hardik1/awsmux/internal/output"
)

var runFlags struct {
	sel         selectorFlags
	exec        execFlags
	format      string
	outputDir   string
	interactive bool
	yes         bool
}

var runCmd = &cobra.Command{
	Use:   "run [flags] -- <service> <operation> [args...]",
	Short: "Run one AWS CLI operation across many targets",
	RunE:  runRun,
}

func init() {
	addSelectorFlags(runCmd, &runFlags.sel)
	addExecFlags(runCmd, &runFlags.exec)
	f := runCmd.Flags()
	f.StringVar(&runFlags.format, "format", "table", "table | json | jsonl")
	f.StringVar(&runFlags.outputDir, "output-dir", "", "also write one result file per target into this directory")
	f.BoolVarP(&runFlags.interactive, "interactive", "i", false, "pick targets and confirm interactively")
	f.BoolVarP(&runFlags.yes, "yes", "y", false, "skip confirmation for mutating (never destructive) operations")
	rootCmd.AddCommand(runCmd)
}

func runRun(cmd *cobra.Command, args []string) error {
	if !output.ValidFormat(runFlags.format) {
		return Exitf(core.ExitConfigError, "invalid --format %q (want table, json, or jsonl)", runFlags.format)
	}
	service, operation, rest, err := splitCommand(args, cmd.ArgsLenAtDash())
	if err != nil {
		return err
	}
	if runFlags.interactive && !isTTY() {
		return Exitf(core.ExitConfigError, "--interactive requires a terminal on stdin")
	}

	if err := core.ValidateArgs(rest); err != nil {
		return Exitf(core.ExitConfigError, "%s", err)
	}

	sel := runFlags.sel.selector()
	// Never execute against unverified identities, whatever --preflight says.
	sel.Preflight = true
	targets, err := core.ResolveTargets(cmd.Context(), sel)
	if err != nil {
		return Exitf(core.ExitConfigError, "%s", err)
	}
	if len(targets) == 0 {
		return Exitf(core.ExitConfigError, "no targets matched the selection")
	}
	// A failed preflight blocks the run, it is not just informational.
	if err := core.CheckVerified(targets); err != nil {
		return Exitf(core.ExitConfigError, "%s (fix credentials or --exclude the profile)", err)
	}

	class := core.Classify(service, operation)
	opts := runFlags.exec.options()

	if runFlags.interactive {
		picked, err := pickTargets(os.Stdin, os.Stderr, targets)
		if err != nil {
			return Exitf(core.ExitConfigError, "%s", err)
		}
		targets = picked
		renderRunPreview(os.Stderr, service, operation, rest, class, targets, opts)
		// --yes may skip the typed confirmation, but never for destructive.
		if class == core.ClassDestructive || !runFlags.yes {
			if err := confirmMutation(os.Stdin, os.Stderr, class, operation); err != nil {
				return Exitf(core.ExitApprovalRequired, "%s", err)
			}
		}
	} else {
		switch class {
		case core.ClassReadOnly:
			// Runs freely.
		case core.ClassDestructive:
			return Exitf(core.ExitApprovalRequired,
				"destructive operation %s %s needs the approval workflow: awsmux plan / approve / apply (or run interactively with -i)",
				service, operation)
		default: // mutating or unknown
			if !runFlags.yes {
				return Exitf(core.ExitApprovalRequired,
					"%s operation %s %s needs --yes, --interactive, or the awsmux plan / approve / apply workflow",
					class, service, operation)
			}
		}
	}

	var onResult func(core.TargetResult)
	if runFlags.format == "jsonl" {
		onResult = func(r core.TargetResult) {
			if err := output.StreamResult(os.Stdout, r); err != nil {
				fmt.Fprintf(os.Stderr, "awsmux: warning: streaming result for %s: %s\n", r.Target.ID, err)
			}
		}
	}

	exe := core.Execute(cmd.Context(), targets, service, operation, rest, opts, onResult)

	var renderErr error
	switch runFlags.format {
	case "jsonl":
		// Results were already streamed; only the summary goes to stderr.
		output.RenderSummary(os.Stderr, exe)
	case "json":
		renderErr = output.RenderExecution(os.Stdout, exe, "json")
	default: // table
		renderErr = output.RenderExecution(os.Stdout, exe, "table")
		output.RenderSummary(os.Stderr, exe)
	}

	if err := core.SaveExecution(exe); err != nil {
		fmt.Fprintf(os.Stderr, "awsmux: warning: could not save execution history: %s\n", err)
	}
	if runFlags.outputDir != "" {
		if err := output.WriteOutputDir(runFlags.outputDir, exe); err != nil {
			fmt.Fprintf(os.Stderr, "awsmux: warning: could not write --output-dir: %s\n", err)
		}
	}
	if renderErr != nil {
		return fmt.Errorf("rendering execution: %w", renderErr)
	}
	if code := exe.ExitCode(); code != core.ExitOK {
		return Exitf(code, "")
	}
	return nil
}

// renderRunPreview shows what an interactive run is about to do: the exact
// command, its classification, concurrency, and the chosen targets.
func renderRunPreview(w io.Writer, service, operation string, args []string, class core.Classification, targets []core.Target, opts core.ExecOptions) {
	cmdline := "aws " + service + " " + operation
	if len(args) > 0 {
		cmdline += " " + strings.Join(args, " ")
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = core.DefaultConcurrency
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Command         %s\n", cmdline)
	fmt.Fprintf(w, "Classification  %s\n", strings.ToUpper(string(class)))
	fmt.Fprintf(w, "Concurrency     %d\n", conc)
	if err := output.RenderTargets(w, targets, "table"); err != nil {
		fmt.Fprintf(w, "awsmux: warning: rendering targets: %s\n", err)
	}
	fmt.Fprintln(w)
}
