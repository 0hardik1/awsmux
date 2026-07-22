package core

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// AWSBinEnv overrides the command used to invoke the AWS CLI. It may contain
// leading arguments separated by spaces (e.g. "/path/to/stub-aws --flag"),
// which is how tests swap in a stand-in CLI without touching any other code
// path.
const AWSBinEnv = "AWSMUX_AWS_BIN"

// awsExec builds the exec.Cmd for one AWS CLI invocation, honoring AWSBinEnv.
func awsExec(ctx context.Context, argv ...string) *exec.Cmd {
	if override := strings.TrimSpace(os.Getenv(AWSBinEnv)); override != "" {
		parts := strings.Fields(override)
		return exec.CommandContext(ctx, parts[0], append(parts[1:], argv...)...)
	}
	return exec.CommandContext(ctx, "aws", argv...)
}
