package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"awsmux/internal/core"
	"awsmux/internal/output"
)

// AGENT CONTRACT (cli-plan): implement runApprove. This is the human
// approval boundary agents cannot cross:
//
//  1. core.LoadPlan (prefix ok). Must be PlanPlanned and unexpired;
//     otherwise ExitApprovalRequired with a reason.
//  2. Verify p.ComputeHash() == p.Hash (tamper check) -> else refuse.
//  3. Show output.RenderPlan. Unless --yes, require the user to type
//     exactly "approve" (destructive plans: type the operation name) on a
//     TTY; anything else = ExitApprovalRequired "approval rejected".
//     Without a TTY and without --yes, refuse.
//  4. core.NewApprovalToken, core.SavePlan, then print the token on its own
//     stdout line as: approval token (give this to the executor, it is not
//     stored): <token>

var approveFlags struct {
	yes bool
}

var approveCmd = &cobra.Command{
	Use:   "approve <plan-id>",
	Short: "Approve a plan and mint its one-time approval token",
	Args:  cobra.ExactArgs(1),
	RunE:  runApprove,
}

func init() {
	approveCmd.Flags().BoolVarP(&approveFlags.yes, "yes", "y", false, "approve without the interactive confirmation")
	rootCmd.AddCommand(approveCmd)
}

// stdinIsCharDevice reports whether stdin is an interactive terminal.
// Deliberately duplicated here (interactive.go has its own check) so this
// file stays self-contained.
func stdinIsCharDevice() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func runApprove(cmd *cobra.Command, args []string) error {
	p, err := core.LoadPlan(args[0])
	if err != nil {
		return Exitf(core.ExitConfigError, "load plan: %v", err)
	}
	if p.Status != core.PlanPlanned {
		return Exitf(core.ExitApprovalRequired, "plan %s is %s; only planned plans can be approved", p.ID, p.Status)
	}
	if p.Expired(time.Now()) {
		return Exitf(core.ExitApprovalRequired, "plan %s expired at %s; create a fresh plan", p.ID, p.ExpiresAt.Format(time.RFC3339))
	}
	if p.ComputeHash() != p.Hash {
		return Exitf(core.ExitApprovalRequired, "plan %s failed its hash check; refusing to approve a tampered plan", p.ID)
	}

	output.RenderPlan(os.Stdout, p)

	if !approveFlags.yes {
		if !stdinIsCharDevice() {
			return Exitf(core.ExitApprovalRequired, "stdin is not a TTY; rerun with --yes to approve non-interactively")
		}
		want := "approve"
		if p.Classification == core.ClassDestructive {
			want = p.Operation
			fmt.Fprintf(os.Stderr, "destructive plan: type %q to approve: ", want)
		} else {
			fmt.Fprintf(os.Stderr, "type %q to approve: ", want)
		}
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return Exitf(core.ExitApprovalRequired, "approval rejected")
		}
		if strings.TrimSpace(line) != want {
			return Exitf(core.ExitApprovalRequired, "approval rejected")
		}
	}

	token, err := core.NewApprovalToken(p)
	if err != nil {
		return Exitf(core.ExitApprovalRequired, "approve plan %s: %v", p.ID, err)
	}
	if err := core.SavePlan(p); err != nil {
		return Exitf(core.ExitConfigError, "save plan: %v", err)
	}
	fmt.Printf("approval token (give this to the executor, it is not stored): %s\n", token)
	return nil
}
