package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"awsmux/internal/core"
	"awsmux/internal/output"
)

var doctorFlags struct {
	format string
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose the environment: aws CLI, shared config files, state dir",
	Args:  cobra.NoArgs,
	RunE:  runDoctor,
}

func init() {
	doctorCmd.Flags().StringVar(&doctorFlags.format, "format", "table", "table | json")
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	if doctorFlags.format != "table" && doctorFlags.format != "json" {
		return Exitf(core.ExitConfigError, "invalid --format %q (want table or json)", doctorFlags.format)
	}
	report := core.Doctor(cmd.Context())
	if err := output.RenderDoctor(os.Stdout, report, doctorFlags.format); err != nil {
		return fmt.Errorf("rendering doctor report: %w", err)
	}
	if !report.OK {
		return Exitf(core.ExitConfigError, "environment check failed (see report above)")
	}
	return nil
}
