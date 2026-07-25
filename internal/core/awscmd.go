package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// AWSBinEnv overrides the command used to invoke the AWS CLI. It may contain
// leading arguments separated by spaces (e.g. "/path/to/stub-aws --flag"),
// which is how tests swap in a stand-in CLI without touching any other code
// path.
const AWSBinEnv = "AWSMUX_AWS_BIN"

// awsBinFallbacks lists well-known aws CLI install locations probed when
// PATH lookup fails. MCP clients launched from a GUI (Claude Desktop and
// friends) spawn `awsmux mcp` with a minimal PATH that rarely includes the
// aws CLI, so relying on PATH alone would break the server exactly where it
// is hardest to debug. Package-level so tests can substitute candidates.
var awsBinFallbacks = defaultAWSBinFallbacks()

func defaultAWSBinFallbacks() []string {
	if runtime.GOOS == "windows" {
		var out []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
			if dir := os.Getenv(env); dir != "" {
				out = append(out, filepath.Join(dir, "Amazon", "AWSCLIV2", "aws.exe"))
			}
		}
		return out
	}
	out := []string{
		"/opt/homebrew/bin/aws",                 // Homebrew on Apple silicon
		"/usr/local/bin/aws",                    // Homebrew on Intel, official macOS pkg
		"/usr/local/aws-cli/v2/current/bin/aws", // official Linux installer
		"/usr/bin/aws",                          // distro packages
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".local", "bin", "aws")) // pip --user
	}
	return out
}

// resolveAWSBin picks the AWS CLI invocation: the AWSMUX_AWS_BIN override
// verbatim (split into binary + leading args), else "aws" when PATH resolves
// it, else the first existing well-known install location. When nothing is
// found it still returns "aws" so callers surface the standard not-found
// error from exec.
func resolveAWSBin() (bin string, leadingArgs []string) {
	if override := strings.TrimSpace(os.Getenv(AWSBinEnv)); override != "" {
		parts := strings.Fields(override)
		return parts[0], parts[1:]
	}
	if _, err := exec.LookPath("aws"); err == nil {
		return "aws", nil
	}
	for _, cand := range awsBinFallbacks {
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand, nil
		}
	}
	return "aws", nil
}

// awsExec builds the exec.Cmd for one AWS CLI invocation via resolveAWSBin.
func awsExec(ctx context.Context, argv ...string) *exec.Cmd {
	bin, lead := resolveAWSBin()
	return exec.CommandContext(ctx, bin, append(lead, argv...)...)
}

// awsExecEnv is awsExec with extra "KEY=value" entries appended to the child's
// environment. Only org enumeration uses it, because that is the only path
// that may run under temporary credentials from an assumed role. Every other
// call site uses awsExec and inherits the process environment unchanged.
func awsExecEnv(ctx context.Context, extraEnv []string, argv ...string) *exec.Cmd {
	cmd := awsExec(ctx, argv...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	return cmd
}
