// Package cmd is the human CLI layer over the awsmux core engine. Every
// command is a thin wrapper: resolve targets, build or load a plan, run the
// executor, render via internal/output, and exit with the stable codes in
// core (ExitOK .. ExitStoppedByThreshold).
package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/0hardik1/awsmux/internal/core"
)

var rootCmd = &cobra.Command{
	Use:   "awsmux",
	Short: "Mission control for the AWS CLI: fleet execution across profiles and regions",
	Long: strings.TrimSpace(`
awsmux runs AWS CLI operations across many profiles, accounts, and regions
with identity preflight, risk classification, an approval boundary for
mutating operations, structured output, and execution history.

Agents connect through "awsmux mcp"; humans use run / plan / approve /
apply. Exit codes are stable: 0 all succeeded, 1 some failed, 2 config or
selection error, 3 approval required or rejected, 4 stopped by threshold.
`),
	SilenceUsage:  true,
	SilenceErrors: true,
}

// ExitError carries one of the stable core.Exit* codes out of a command.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// Exitf builds an ExitError with a formatted message.
func Exitf(code int, format string, a ...any) *ExitError {
	return &ExitError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	if err := rootCmd.Execute(); err != nil {
		var xe *ExitError
		if errors.As(err, &xe) {
			if xe.Msg != "" {
				fmt.Fprintln(os.Stderr, "awsmux: "+xe.Msg)
			}
			return xe.Code
		}
		fmt.Fprintln(os.Stderr, "awsmux: "+err.Error())
		return core.ExitConfigError
	}
	return core.ExitOK
}

// selectorFlags are shared by every command that picks targets.
type selectorFlags struct {
	profiles        []string
	exclude         []string
	regions         []string
	preflight       bool
	dedupe          bool
	ou              []string
	accountTags     map[string]string
	orgRole         string
	orgProfile      string
	orgRefresh      bool
	showUnreachable bool
}

func addSelectorFlags(cmd *cobra.Command, sf *selectorFlags) {
	f := cmd.Flags()
	f.StringSliceVar(&sf.profiles, "profiles", nil, "profile glob(s), e.g. 'prod-*' (default: all profiles)")
	f.StringSliceVar(&sf.exclude, "exclude", nil, "profile glob(s) to exclude, e.g. '*-prod-*'")
	f.StringSliceVar(&sf.regions, "regions", nil, "region(s), one target per profile x region (default: profile default region)")
	f.BoolVar(&sf.preflight, "preflight", true, "verify each target identity via sts get-caller-identity")
	f.BoolVar(&sf.dedupe, "dedupe", false, "drop targets that resolve to a duplicate account+principal+region")
	f.StringSliceVar(&sf.ou, "ou", nil, "AWS Organizations OU path glob(s), e.g. 'eng/prod' (matches nested OUs too)")
	f.StringToStringVar(&sf.accountTags, "account-tag", nil, "org account tag filter(s), e.g. env=prod (every pair must match)")
	f.StringVar(&sf.orgRole, "org-role", "", "role ARN assumed to enumerate AWS Organizations (enumeration only, never execution)")
	f.StringVar(&sf.orgProfile, "org-profile", "", "profile for the organizations calls (default: normal AWS resolution)")
	f.BoolVar(&sf.orgRefresh, "org-refresh", false, "bypass the cached organization tree")
	f.BoolVar(&sf.showUnreachable, "show-unreachable", false, "list every matched org account that has no local profile")
}

func (sf *selectorFlags) selector() core.Selector {
	return core.Selector{
		Profiles:    sf.profiles,
		Exclude:     sf.exclude,
		Regions:     sf.regions,
		Preflight:   sf.preflight,
		Dedupe:      sf.dedupe,
		OU:          sf.ou,
		AccountTags: sf.accountTags,
		OrgRole:     sf.orgRole,
		OrgProfile:  sf.orgProfile,
		OrgRefresh:  sf.orgRefresh,
	}
}

// validate rejects flag combinations that cannot be honored. An org selector
// filters on the account ID STS verified, so it cannot run with preflight
// explicitly disabled; silently overriding the user would be worse than
// saying so.
func (sf *selectorFlags) validate(cmd *cobra.Command) error {
	if !sf.selector().UsesOrg() {
		return nil
	}
	if cmd.Flags().Changed("preflight") && !sf.preflight {
		return Exitf(core.ExitConfigError,
			"--ou and --account-tag filter on the account ID that STS verified, so they cannot run with --preflight=false")
	}
	return nil
}

// execFlags are shared by run / apply / replay.
type execFlags struct {
	concurrency        int
	timeout            time.Duration
	maxErrors          int
	stopOnAccessDenied bool
}

func addExecFlags(cmd *cobra.Command, ef *execFlags) {
	f := cmd.Flags()
	f.IntVar(&ef.concurrency, "concurrency", core.DefaultConcurrency, "max targets in flight")
	f.DurationVar(&ef.timeout, "timeout", 0, "per-target timeout, e.g. 30s (0 = none)")
	f.IntVar(&ef.maxErrors, "max-errors", 0, "stop scheduling after N failures (0 = unlimited)")
	f.BoolVar(&ef.stopOnAccessDenied, "stop-on-access-denied", false, "stop scheduling on the first access denied")
}

func (ef *execFlags) options() core.ExecOptions {
	return core.ExecOptions{
		Concurrency:        ef.concurrency,
		Timeout:            ef.timeout,
		MaxErrors:          ef.maxErrors,
		StopOnAccessDenied: ef.stopOnAccessDenied,
	}
}

// splitCommand splits cobra args at "--" into the aws service, operation,
// and trailing args. dash is cmd.ArgsLenAtDash(). Returns an ExitError
// (ExitConfigError) when the command part is missing or too short.
func splitCommand(args []string, dash int) (service, operation string, rest []string, err error) {
	if dash < 0 || len(args)-dash < 2 {
		return "", "", nil, Exitf(core.ExitConfigError,
			"missing AWS command: usage looks like `awsmux run [flags] -- ec2 describe-instances [args]`")
	}
	cmdArgs := args[dash:]
	return cmdArgs[0], cmdArgs[1], cmdArgs[2:], nil
}
