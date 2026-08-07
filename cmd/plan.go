package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/0hardik1/awsmux/internal/core"
	"github.com/0hardik1/awsmux/internal/output"
)

// AGENT CONTRACT (cli-plan): implement runPlan and runPlans.
//
// runPlan: splitCommand, resolve selector (preflight forced on: a plan's
// hash must bind verified identities), core.NewPlan with --ttl,
// core.SavePlan, then output.RenderPlan to stdout. Zero targets =
// ExitConfigError.
//
// runPlans: list stored plans, newest first, as a table: ID, STATUS,
// CLASSIFICATION, SERVICE OPERATION, TARGETS, EXPIRES (or "expired").

var planFlags struct {
	sel selectorFlags
	ttl time.Duration
}

var planCmd = &cobra.Command{
	Use:   "plan [flags] -- <service> <operation> [args...]",
	Short: "Create an immutable, hashed execution plan (no AWS calls beyond preflight)",
	RunE:  runPlan,
}

var plansCmd = &cobra.Command{
	Use:   "plans",
	Short: "List stored plans",
	Args:  cobra.NoArgs,
	RunE:  runPlans,
}

func init() {
	addSelectorFlags(planCmd, &planFlags.sel)
	planCmd.Flags().DurationVar(&planFlags.ttl, "ttl", core.DefaultPlanTTL, "how long the plan stays executable")
	rootCmd.AddCommand(planCmd)
	rootCmd.AddCommand(plansCmd)
}

func runPlan(cmd *cobra.Command, args []string) error {
	service, operation, rest, err := splitCommand(args, cmd.ArgsLenAtDash())
	if err != nil {
		return err
	}
	if err := planFlags.sel.validate(cmd); err != nil {
		return err
	}
	sel := planFlags.sel.selector()
	// The plan hash binds verified identities, so preflight is not optional.
	sel.Preflight = true
	targets, err := core.ResolveTargets(cmd.Context(), sel)
	if err != nil {
		return Exitf(core.ExitConfigError, "resolve targets: %v", err)
	}
	if len(targets) == 0 {
		return Exitf(core.ExitConfigError, "no targets matched the selection")
	}
	p, err := core.NewPlan(service, operation, rest, targets, planFlags.ttl)
	if err != nil {
		return Exitf(core.ExitConfigError, "create plan: %v", err)
	}
	if err := core.SavePlan(p); err != nil {
		return Exitf(core.ExitConfigError, "save plan: %v", err)
	}
	output.RenderPlan(os.Stdout, p)
	return nil
}

func runPlans(cmd *cobra.Command, args []string) error {
	plans, err := core.ListPlans()
	if err != nil {
		return Exitf(core.ExitConfigError, "list plans: %v", err)
	}
	if len(plans) == 0 {
		fmt.Println("no plans stored; create one with `awsmux plan`")
		return nil
	}
	now := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tCLASSIFICATION\tSERVICE OPERATION\tTARGETS\tEXPIRES")
	for _, p := range plans {
		expires := "expired"
		if !p.Expired(now) {
			expires = "in " + coarseDuration(time.Until(p.ExpiresAt))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s %s\t%d\t%s\n",
			p.ID, p.Status, p.Classification, p.Service, p.Operation, len(p.Targets), expires)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("render plans: %w", err)
	}
	return nil
}
