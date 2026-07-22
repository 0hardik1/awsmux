package cmd

import (
	"github.com/spf13/cobra"

	"github.com/0hardik1/awsmux/internal/mcpserver"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve the awsmux MCP server on stdio (the agent interface)",
	Long: "Exposes list_aws_targets, plan_aws_operation, execute_aws_plan, " +
		"get_aws_execution, and cancel_aws_execution over the Model Context " +
		"Protocol. Register with: claude mcp add awsmux -- awsmux mcp",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return mcpserver.Serve(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}
