package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultConcurrency is used when ExecOptions.Concurrency <= 0.
const DefaultConcurrency = 4

// maxCapture caps stored stdout/stderr per target at 64 KiB.
const maxCapture = 64 * 1024

// BuildCommand returns the argv (excluding the "aws" binary itself) for one
// target:
//
//	--profile <p> [--region <r>] --output json <service> <operation> <args...>
//
// Region is omitted when the target's Region is empty. If args already
// contain "--output", do not add another.
func BuildCommand(t Target, service, operation string, args []string) []string {
	argv := []string{"--profile", t.Profile}
	if t.Region != "" {
		argv = append(argv, "--region", t.Region)
	}
	hasOutput := false
	for _, a := range args {
		if a == "--output" || strings.HasPrefix(a, "--output=") {
			hasOutput = true
			break
		}
	}
	if !hasOutput {
		argv = append(argv, "--output", "json")
	}
	argv = append(argv, service, operation)
	return append(argv, args...)
}

// Execute runs service/operation against every target with a worker pool of
// opts.Concurrency. Per-target problems become results, never an error; the
// process exit code comes from Execution.ExitCode().
//
//   - Each target runs "aws" with BuildCommand argv via exec.CommandContext,
//     under context.WithTimeout when opts.Timeout > 0.
//   - Stdout that parses as JSON goes in Result; otherwise raw in Stdout.
//     Stored Stdout/Stderr are truncated to 64 KiB each.
//   - After opts.MaxErrors failures (0 = unlimited), or the first
//     access_denied when opts.StopOnAccessDenied, workers stop picking up new
//     jobs: remaining targets get StatusSkipped and Execution.Stopped = true.
//   - onResult (may be nil) is called one result at a time in completion
//     order (this is the JSONL stream).
//   - Results in the returned Execution are ordered like the input targets.
//   - ctx cancellation marks unstarted targets skipped and in-flight ones
//     with their context error; Execution.Status = "cancelled".
func Execute(ctx context.Context, targets []Target, service, operation string, args []string, opts ExecOptions, onResult func(TargetResult)) *Execution {
	execution := &Execution{
		ID:             NewID("exec"),
		Service:        service,
		Operation:      operation,
		Args:           args,
		Classification: Classify(service, operation),
		StartedAt:      time.Now().UTC(),
		Results:        make([]TargetResult, len(targets)),
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	if len(targets) > 0 && concurrency > len(targets) {
		concurrency = len(targets)
	}

	var (
		mu       sync.Mutex // serializes result recording and onResult
		failures atomic.Int64
		stopped  atomic.Bool
		wg       sync.WaitGroup
	)
	record := func(i int, r TargetResult) {
		mu.Lock()
		defer mu.Unlock()
		execution.Results[i] = r
		if onResult != nil {
			onResult(r)
		}
	}

	jobs := make(chan int, len(targets))
	for i := range targets {
		jobs <- i
	}
	close(jobs)

	wg.Add(concurrency)
	for w := 0; w < concurrency; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				if stopped.Load() || ctx.Err() != nil {
					record(i, TargetResult{
						Target:    targets[i],
						Status:    StatusSkipped,
						ErrorCode: "NotRun",
					})
					continue
				}
				r := runTarget(ctx, targets[i], service, operation, args, opts.Timeout)
				switch r.Status {
				case StatusError, StatusTimeout, StatusAccessDenied, StatusExpiredCreds:
					n := failures.Add(1)
					if opts.MaxErrors > 0 && n >= int64(opts.MaxErrors) {
						stopped.Store(true)
					}
					if opts.StopOnAccessDenied && r.Status == StatusAccessDenied {
						stopped.Store(true)
					}
				}
				record(i, r)
			}
		}()
	}
	wg.Wait()

	execution.FinishedAt = time.Now().UTC()
	execution.Stopped = stopped.Load()
	if ctx.Err() != nil {
		execution.Status = "cancelled"
	} else {
		execution.Status = "completed"
	}
	execution.Summary = Summarize(execution.Results)
	return execution
}

// runTarget executes the aws CLI once for a single target.
func runTarget(ctx context.Context, t Target, service, operation string, args []string, timeout time.Duration) TargetResult {
	res := TargetResult{Target: t}

	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, "aws", BuildCommand(t, service, operation, args)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	res.DurationMS = time.Since(start).Milliseconds()

	trimmed := strings.TrimSpace(stdout.String())
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		res.Result = json.RawMessage(trimmed)
	} else {
		res.Stdout = truncateCapture(stdout.String())
	}
	res.Stderr = truncateCapture(stderr.String())

	if err == nil {
		res.Status = StatusSuccess
		return res
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
	} else {
		res.ExitCode = -1
		if res.Stderr == "" {
			// The process never started (e.g. aws binary missing), so
			// surface the launch error where stderr would be.
			res.Stderr = truncateCapture(err.Error())
		}
	}
	res.Status, res.ErrorCode = classifyFailure(res.ExitCode, stderr.String(), runCtx.Err())
	return res
}

// truncateCapture caps s at maxCapture bytes with a truncation marker.
func truncateCapture(s string) string {
	if len(s) <= maxCapture {
		return s
	}
	return s[:maxCapture] + "... [truncated]"
}

// Summarize tallies results into a Summary (Failed counts error + timeout;
// AccessDenied and CredentialExpired counted separately and also as Failed).
func Summarize(results []TargetResult) Summary {
	s := Summary{Total: len(results)}
	for _, r := range results {
		switch r.Status {
		case StatusSuccess:
			s.Succeeded++
		case StatusError:
			s.Failed++
		case StatusTimeout:
			s.TimedOut++
			s.Failed++
		case StatusAccessDenied:
			s.AccessDenied++
			s.Failed++
		case StatusExpiredCreds:
			s.CredentialExpired++
			s.Failed++
		case StatusSkipped:
			s.Skipped++
		}
	}
	return s
}

// ExitCode maps the execution to the stable awsmux exit codes: ExitOK,
// ExitCommandFailed, or ExitStoppedByThreshold (when Stopped).
func (e *Execution) ExitCode() int {
	if e.Stopped {
		return ExitStoppedByThreshold
	}
	if e.Summary.Failed > 0 || e.Summary.Skipped > 0 {
		return ExitCommandFailed
	}
	return ExitOK
}

// errCodeRE matches "(SomethingException)" style codes in AWS CLI stderr,
// e.g. "An error occurred (ThrottlingException) when calling ...". Dots are
// allowed for codes like InvalidInstanceID.NotFound.
var errCodeRE = regexp.MustCompile(`\(([A-Za-z][A-Za-z0-9.]{2,127})\)`)

// classifyFailure inspects a non-zero exit and stderr to produce the result
// status and a short machine error code. The context error is checked first
// so a killed process is reported as a timeout (or cancellation) rather than
// a generic failure.
func classifyFailure(exitCode int, stderr string, ctxErr error) (ResultStatus, string) {
	_ = exitCode
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return StatusTimeout, "Timeout"
	}
	if errors.Is(ctxErr, context.Canceled) {
		return StatusError, "Cancelled"
	}
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "accessdenied"),
		strings.Contains(lower, "unauthorizedoperation"),
		strings.Contains(lower, "not authorized"):
		return StatusAccessDenied, "AccessDenied"
	case strings.Contains(lower, "expiredtoken"),
		strings.Contains(lower, "token has expired"),
		strings.Contains(lower, "credentials have expired"),
		strings.Contains(lower, "security token included in the request is expired"),
		strings.Contains(lower, "sso session"):
		// Deliberately narrow: a bare "expired" in stderr can describe the
		// resource (an expired presigned URL, certificate, or snapshot),
		// not the caller's credentials.
		return StatusExpiredCreds, "ExpiredCredentials"
	}
	if m := errCodeRE.FindStringSubmatch(stderr); m != nil {
		return StatusError, m[1]
	}
	return StatusError, "CommandFailed"
}
