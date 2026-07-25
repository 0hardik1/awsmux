// Package output renders targets, results, and executions as table, json,
// or jsonl. jsonl is the agent/CI contract: one self-describing object per
// line, streamed as results complete.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/0hardik1/awsmux/internal/core"
)

// ValidFormat reports whether f is "table", "json", or "jsonl".
func ValidFormat(f string) bool {
	switch f {
	case "table", "json", "jsonl":
		return true
	}
	return false
}

// RenderTargets writes the target list. Table columns:
// ID, PROFILE, ACCOUNT, REGION, PRINCIPAL (shortened to the part after the
// last "/"), SOURCE (config | credentials | both), NOTES (duplicate /
// preflight error). json = one array; jsonl = one target per line.
func RenderTargets(w io.Writer, targets []core.Target, format string) error {
	switch format {
	case "table":
		return renderTargetTable(w, targets)
	case "json":
		b, err := json.MarshalIndent(targets, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding targets: %w", err)
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	case "jsonl":
		enc := json.NewEncoder(w)
		for _, t := range targets {
			if err := enc.Encode(t); err != nil {
				return fmt.Errorf("encoding target %s: %w", t.ID, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q (want table, json, or jsonl)", format)
	}
}

// StreamResult writes one result as a jsonl line the moment it completes:
// a flat object {profile, account_id, region, status, duration_ms,
// exit_code, result | stdout, error_code, stderr} (omit empties, keep
// stderr short). Used as the Execute onResult callback in jsonl mode.
func StreamResult(w io.Writer, r core.TargetResult) error {
	line := struct {
		Profile    string            `json:"profile"`
		AccountID  string            `json:"account_id,omitempty"`
		Region     string            `json:"region,omitempty"`
		Status     core.ResultStatus `json:"status"`
		DurationMS int64             `json:"duration_ms"`
		ExitCode   int               `json:"exit_code"`
		Result     json.RawMessage   `json:"result,omitempty"`
		Stdout     string            `json:"stdout,omitempty"`
		ErrorCode  string            `json:"error_code,omitempty"`
		Stderr     string            `json:"stderr,omitempty"`
	}{
		Profile:    r.Target.Profile,
		AccountID:  r.Target.AccountID,
		Region:     r.Target.Region,
		Status:     r.Status,
		DurationMS: r.DurationMS,
		ExitCode:   r.ExitCode,
		Result:     r.Result,
		ErrorCode:  r.ErrorCode,
		Stderr:     singleLine(r.Stderr, 300),
	}
	// Result and Stdout are mutually exclusive on the wire: parsed JSON wins.
	if len(line.Result) == 0 {
		line.Stdout = r.Stdout
	}
	b, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("encoding result for %s: %w", r.Target.ID, err)
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// RenderExecution writes the full execution in the given format. Table
// mode: one row per target (PROFILE, ACCOUNT, REGION, STATUS, DURATION,
// ERROR); callers print RenderSummary separately (to stderr). json mode:
// the whole Execution, indented. jsonl mode: every result via StreamResult
// then a final {"summary": ...} line.
func RenderExecution(w io.Writer, e *core.Execution, format string) error {
	switch format {
	case "table":
		tw := new(tabwriter.Writer).Init(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PROFILE\tACCOUNT\tREGION\tSTATUS\tDURATION\tERROR")
		for _, r := range e.Results {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Target.Profile,
				orDash(r.Target.AccountID),
				orDash(r.Target.Region),
				string(r.Status),
				formatMillis(r.DurationMS),
				r.ErrorCode)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("rendering result table: %w", err)
		}
		return nil
	case "json":
		b, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding execution: %w", err)
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	case "jsonl":
		for _, r := range e.Results {
			if err := StreamResult(w, r); err != nil {
				return err
			}
		}
		b, err := json.Marshal(struct {
			Summary core.Summary `json:"summary"`
		}{e.Summary})
		if err != nil {
			return fmt.Errorf("encoding summary: %w", err)
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	default:
		return fmt.Errorf("unknown format %q (want table, json, or jsonl)", format)
	}
}

// RenderSummary writes the human summary block:
//
//	18 targets: 15 succeeded, 2 access denied, 1 credential expired
//	duration 11.2s   exit 1
func RenderSummary(w io.Writer, e *core.Execution) {
	s := e.Summary
	parts := []string{fmt.Sprintf("%d succeeded", s.Succeeded)}
	// Failed aggregates every failure kind; show only the plain-error slice
	// here so the categories add up to the total.
	plainFailed := s.Failed - s.AccessDenied - s.CredentialExpired - s.TimedOut
	if plainFailed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", plainFailed))
	}
	if s.AccessDenied > 0 {
		parts = append(parts, fmt.Sprintf("%d access denied", s.AccessDenied))
	}
	if s.CredentialExpired > 0 {
		parts = append(parts, fmt.Sprintf("%d credential expired", s.CredentialExpired))
	}
	if s.TimedOut > 0 {
		parts = append(parts, fmt.Sprintf("%d timed out", s.TimedOut))
	}
	if s.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", s.Skipped))
	}
	fmt.Fprintf(w, "%d targets: %s\n", s.Total, strings.Join(parts, ", "))
	dur := e.FinishedAt.Sub(e.StartedAt)
	fmt.Fprintf(w, "duration %.1fs   exit %d\n", dur.Seconds(), e.ExitCode())
}

// RenderPlan writes the human-facing plan preview (used by `awsmux plan`
// and interactive confirmation): id, service/operation + args,
// classification (upper-cased), target table, requires approval, expiry,
// hash, and the exact next command to run (approve/apply).
func RenderPlan(w io.Writer, p *core.Plan) {
	cmdline := "aws " + p.Service + " " + p.Operation
	if len(p.Args) > 0 {
		cmdline += " " + strings.Join(p.Args, " ")
	}
	approval := "no"
	if p.RequiresApproval {
		approval = "yes"
	}
	hash := p.Hash
	if len(hash) > 16 {
		hash = hash[:16]
	}
	tw := new(tabwriter.Writer).Init(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "Plan\t%s\n", p.ID)
	fmt.Fprintf(tw, "Command\t%q\n", cmdline)
	fmt.Fprintf(tw, "Classification\t%s\n", strings.ToUpper(string(p.Classification)))
	fmt.Fprintf(tw, "Requires approval\t%s\n", approval)
	fmt.Fprintf(tw, "Expires\t%s\n", p.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintf(tw, "Hash\t%s\n", hash)
	tw.Flush()
	fmt.Fprintln(w)
	_ = renderTargetTable(w, p.Targets)
	fmt.Fprintln(w)
	if p.RequiresApproval {
		fmt.Fprintf(w, "Next: awsmux approve %s  &&  awsmux apply %s --approval-token <token>\n", p.ID, p.ID)
	} else {
		fmt.Fprintf(w, "Next: awsmux apply %s\n", p.ID)
	}
}

// WriteOutputDir writes one <profile>[_<region>].json file per result into
// dir (created if needed), containing the raw Result / Stdout, plus a
// _summary.json for the execution.
func WriteOutputDir(dir string, e *core.Execution) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}
	for _, r := range e.Results {
		name := safeFileName(r.Target.Profile)
		if r.Target.Region != "" {
			name += "_" + safeFileName(r.Target.Region)
		}
		name += ".json"
		body := []byte(r.Result)
		if len(body) == 0 {
			body = []byte(r.Stdout)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	sum := struct {
		ID             string              `json:"id"`
		PlanID         string              `json:"plan_id,omitempty"`
		Service        string              `json:"service"`
		Operation      string              `json:"operation"`
		Args           []string            `json:"args,omitempty"`
		Classification core.Classification `json:"classification"`
		StartedAt      time.Time           `json:"started_at"`
		FinishedAt     time.Time           `json:"finished_at"`
		Stopped        bool                `json:"stopped,omitempty"`
		Status         string              `json:"status"`
		Summary        core.Summary        `json:"summary"`
	}{
		ID:             e.ID,
		PlanID:         e.PlanID,
		Service:        e.Service,
		Operation:      e.Operation,
		Args:           e.Args,
		Classification: e.Classification,
		StartedAt:      e.StartedAt,
		FinishedAt:     e.FinishedAt,
		Stopped:        e.Stopped,
		Status:         e.Status,
		Summary:        e.Summary,
	}
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding summary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_summary.json"), append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing _summary.json: %w", err)
	}
	return nil
}

// RenderDoctor writes the environment diagnostic. Table mode: one row per
// check (CHECK, STATUS, DETAIL) with ok/FAIL markers and env-override
// annotations. json mode: the whole report, indented. jsonl is rejected:
// the report is a single object, not a stream.
func RenderDoctor(w io.Writer, r core.DoctorReport, format string) error {
	switch format {
	case "table":
		tw := new(tabwriter.Writer).Init(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "CHECK\tSTATUS\tDETAIL")
		row := func(check string, ok bool, detail string) {
			status := "ok"
			if !ok {
				status = "FAIL"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", check, status, detail)
		}
		if r.AWSCLIErr != "" {
			row("aws cli", false, singleLine(r.AWSCLIErr, 120))
		} else {
			row("aws cli", true, r.AWSCLIVersion+" at "+r.AWSCLIPath)
		}
		row("config file", r.ConfigFile.ParseErr == "", doctorFileDetail(r.ConfigFile))
		row("credentials file", r.CredentialsFile.ParseErr == "", doctorFileDetail(r.CredentialsFile))
		profDetail := fmt.Sprintf("%d discovered", r.ProfilesTotal)
		if r.ProfilesBoth > 0 {
			profDetail += fmt.Sprintf(" (%d defined in both files)", r.ProfilesBoth)
		}
		row("profiles", r.ProfilesTotal > 0, profDetail)
		switch {
		case r.Home.Writable:
			row("state dir", true, r.Home.Path+" writable"+doctorEnvNote(r.Home.EnvVar))
		default:
			row("state dir", false, singleLine(r.Home.Err, 120))
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("rendering doctor table: %w", err)
		}
		return nil
	case "json":
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding doctor report: %w", err)
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	default:
		return fmt.Errorf("unknown format %q for doctor (want table or json)", format)
	}
}

// doctorFileDetail summarizes one shared-file check for the table renderer.
func doctorFileDetail(f core.DoctorFileReport) string {
	if f.ParseErr != "" {
		return singleLine(f.ParseErr, 120)
	}
	if !f.Exists {
		return "not found at " + f.Path + doctorEnvNote(f.EnvVar) + ", optional"
	}
	return fmt.Sprintf("%d profiles at %s%s", f.Profiles, f.Path, doctorEnvNote(f.EnvVar))
}

func doctorEnvNote(envVar string) string {
	if envVar == "" {
		return ""
	}
	return " (set via " + envVar + ")"
}

// renderTargetTable prints the target table, adding an OU column only when an
// org selector actually filled one in, so ordinary runs keep their layout.
func renderTargetTable(w io.Writer, targets []core.Target) error {
	withOU := false
	for _, t := range targets {
		if t.OUPath != "" || t.OrgAccountName != "" {
			withOU = true
			break
		}
	}

	tw := new(tabwriter.Writer).Init(w, 0, 4, 2, ' ', 0)
	if withOU {
		fmt.Fprintln(tw, "ID\tPROFILE\tACCOUNT\tREGION\tPRINCIPAL\tSOURCE\tOU\tNOTES")
	} else {
		fmt.Fprintln(tw, "ID\tPROFILE\tACCOUNT\tREGION\tPRINCIPAL\tSOURCE\tNOTES")
	}
	for _, t := range targets {
		if withOU {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				t.ID, t.Profile, orDash(t.AccountID), orDash(t.Region),
				orDash(shortPrincipal(t.Principal)), orDash(string(t.Source)),
				orDash(t.OUPath), targetNotes(t))
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Profile, orDash(t.AccountID), orDash(t.Region),
			orDash(shortPrincipal(t.Principal)), orDash(string(t.Source)),
			targetNotes(t))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("rendering target table: %w", err)
	}
	return nil
}

// unreachablePreview is how many accounts the summary names before eliding.
const unreachablePreview = 3

// RenderUnreachable writes the coverage summary for org accounts that matched
// the selector but that no local profile reaches. It writes nothing when
// there are none, so callers can always call it. Knowing that an OU holds 140
// accounts you can reach 37 of is the fact worth having before a fan-out.
func RenderUnreachable(w io.Writer, accts []core.OrgAccount, showAll bool) error {
	if len(accts) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d accounts matched the selector but have no local profile:\n", len(accts))

	shown := accts
	if !showAll && len(shown) > unreachablePreview {
		shown = shown[:unreachablePreview]
	}
	for _, a := range shown {
		fmt.Fprintf(&b, "  %s (%s)", a.ID, a.Name)
		if a.OUPath != "" {
			fmt.Fprintf(&b, " in %s", a.OUPath)
		}
		if a.Status != "" && a.Status != "ACTIVE" {
			fmt.Fprintf(&b, " [%s]", a.Status)
		}
		b.WriteString("\n")
	}
	if n := len(accts) - len(shown); n > 0 {
		fmt.Fprintf(&b, "  +%d more; pass --show-unreachable to list them all\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func shortPrincipal(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func targetNotes(t core.Target) string {
	var notes []string
	if t.Duplicate {
		notes = append(notes, "duplicate")
	}
	if t.PreflightErr != "" {
		notes = append(notes, "preflight: "+singleLine(t.PreflightErr, 80))
	}
	return strings.Join(notes, "; ")
}

// safeFileName keeps profile and region names from escaping the output dir.
func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == os.PathSeparator {
			return '-'
		}
		return r
	}, s)
}

// singleLine collapses all whitespace runs to single spaces and truncates
// the result to maxLen runes.
func singleLine(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxLen {
		return string(r[:maxLen])
	}
	return s
}

func formatMillis(ms int64) string {
	return fmt.Sprintf("%.1fs", float64(ms)/1000.0)
}
