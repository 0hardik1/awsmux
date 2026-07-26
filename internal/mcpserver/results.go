package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0hardik1/awsmux/internal/core"
)

// Tool results are read by a model, and every character is paid for on every
// subsequent turn of the conversation. The shapes in this file exist to keep
// that bill small without losing information an agent acts on:
//   - target rosters collapse to one short line per target;
//   - plan echoes carry a count and a preview instead of the full roster the
//     agent just saw from list_aws_targets;
//   - execution results group targets that produced the identical outcome,
//     with the dominant group elided to "rest": true;
//   - anything still larger than mcpMaxResultChars truncates inside awsmux,
//     with paging via get_aws_execution, so the client never has to spill a
//     result to a file the agent may not be able to read.
const (
	// mcpMaxResultChars caps one tool result's encoded results payload. Kept
	// well under common client-side tool-output limits so awsmux, not the
	// client, decides what a truncated result looks like.
	mcpMaxResultChars = 24_000
	// targetsPreview is how many target ids a plan response shows.
	targetsPreview = 5
	// stderrCap bounds per-group stderr echoes; AWS CLI errors repeat the
	// useful part in the first few hundred characters.
	stderrCap = 400
	// restElisionMin is the smallest group size worth eliding to rest: true.
	restElisionMin = 10
)

// unreachablePreviewIDs is how many unreachable account ids the summary names
// before eliding. The consumer is a model paying per token, so this stays a
// count plus a taste, not a roster.
const unreachablePreviewIDs = 5

// compactTargets renders targets one line each: "id account principal", with
// " ou=<path>", " DUPLICATE", and " ERROR: <msg>" suffixes when set. The
// format field tells the agent how to read the lines. orgSel is nil unless
// an org selector (ou or account_tags) actually ran.
func compactTargets(targets []core.Target, orgSel *core.OrgSelection) map[string]any {
	rows := make([]string, len(targets))
	for i, t := range targets {
		var b strings.Builder
		b.WriteString(t.ID)
		if t.AccountID != "" {
			b.WriteString(" " + t.AccountID)
		}
		if t.Principal != "" {
			b.WriteString(" " + t.Principal)
		}
		if t.OUPath != "" {
			b.WriteString(" ou=" + t.OUPath)
		}
		if t.Duplicate {
			b.WriteString(" DUPLICATE")
		}
		if t.PreflightErr != "" {
			b.WriteString(" ERROR: " + t.PreflightErr)
		}
		rows[i] = b.String()
	}

	out := map[string]any{
		"count":   len(targets),
		"format":  "each entry: \"<profile>@<region> <account_id> <principal>\", plus ou=<path>, DUPLICATE, or ERROR: <msg> when applicable",
		"targets": rows,
	}

	// Only report coverage when an org selector actually ran; an absent key
	// costs nothing, and a zero-valued one costs tokens for no information.
	if orgSel != nil {
		out["unreachable"] = map[string]any{
			"count":   len(orgSel.Unreachable),
			"sample":  unreachableSample(orgSel),
			"meaning": "org accounts matching the selector that no local profile reaches; they were not targeted",
		}
	}
	return out
}

// unreachableSample renders up to unreachablePreviewIDs unreachable org
// accounts as "<id> (<name>)". Shared by compactTargets and orgResolveError
// so the sample-building loop and its cap exist in exactly one place.
func unreachableSample(orgSel *core.OrgSelection) []string {
	ids := make([]string, 0, unreachablePreviewIDs)
	for i, a := range orgSel.Unreachable {
		if i == unreachablePreviewIDs {
			break
		}
		ids = append(ids, a.ID+" ("+a.Name+")")
	}
	return ids
}

// planResponse carries everything the agent needs to decide the next step
// (classification, approval, expiry, id) without re-echoing the roster.
func planResponse(p *core.Plan) (map[string]any, error) {
	preview := make([]string, 0, targetsPreview)
	for i, t := range p.Targets {
		if i == targetsPreview {
			break
		}
		preview = append(preview, t.ID)
	}
	m := map[string]any{
		"id":                p.ID,
		"service":           p.Service,
		"operation":         p.Operation,
		"classification":    p.Classification,
		"requires_approval": p.RequiresApproval,
		"status":            p.Status,
		"expires_at":        p.ExpiresAt,
		"hash":              p.Hash,
		"target_count":      len(p.Targets),
		"targets_preview":   preview,
	}
	if len(p.Args) > 0 {
		m["args"] = p.Args
	}
	if n := len(p.Targets) - targetsPreview; n > 0 {
		m["targets_omitted"] = n
	}
	if p.RequiresApproval {
		m["approval_hint"] = fmt.Sprintf(
			"approval required: ask a human to run \"awsmux approve %s\" in their terminal and give you the printed token, then pass it as approval_token to execute_aws_plan",
			p.ID)
	} else {
		m["approval_hint"] = "no approval needed"
	}
	return m, nil
}

// resultRow is one target's outcome in compact form: success rows are just
// {target, value} or {target, stdout}; failures add status and diagnostics.
func resultRow(r core.TargetResult) map[string]any {
	m := map[string]any{"target": r.Target.ID}
	if r.Status != core.StatusSuccess {
		m["status"] = r.Status
		if r.ExitCode != 0 {
			m["exit_code"] = r.ExitCode
		}
		if r.ErrorCode != "" {
			m["error_code"] = r.ErrorCode
		}
		if r.Stderr != "" {
			m["stderr"] = capString(r.Stderr, stderrCap)
		}
	}
	if len(r.Result) > 0 {
		m["value"] = compactRaw(r.Result)
	} else if r.Stdout != "" {
		m["stdout"] = r.Stdout
	}
	return m
}

// groupSignature keys results that are identical from the agent's point of
// view: same status, same diagnostics, same output.
func groupSignature(r core.TargetResult) string {
	return strings.Join([]string{
		string(r.Status),
		fmt.Sprint(r.ExitCode),
		r.ErrorCode,
		string(compactRaw(r.Result)),
		r.Stdout,
		capString(r.Stderr, stderrCap),
	}, "\x00")
}

// executionResponse renders an execution for the agent. With offset or limit
// set it pages plain per-target rows (stable plan order). Otherwise it groups
// identical outcomes, eliding the dominant group to rest: true, and falls
// back to truncated paging when even the grouped form exceeds the budget.
func executionResponse(e *core.Execution, offset, limit int) (map[string]any, error) {
	base := map[string]any{
		"id":             e.ID,
		"service":        e.Service,
		"operation":      e.Operation,
		"classification": e.Classification,
		"status":         e.Status,
		"started_at":     e.StartedAt,
		"finished_at":    e.FinishedAt,
		"summary":        e.Summary,
	}
	if e.PlanID != "" {
		base["plan_id"] = e.PlanID
	}
	if len(e.Args) > 0 {
		base["args"] = e.Args
	}
	if e.Stopped {
		base["stopped"] = true
	}

	if offset > 0 || limit > 0 {
		return pageRows(base, e.Results, offset, limit), nil
	}

	grouped := groupResults(e.Results)
	if encodedLen(grouped) <= mcpMaxResultChars {
		base["results"] = grouped
		if hasRest(grouped) {
			base["results_note"] = "results are grouped by identical outcome; the entry with rest: true covers every target not listed in another entry"
		}
		return base, nil
	}
	resp := pageRows(base, e.Results, 0, 0)
	resp["hint"] = "result too large to return whole: prefer re-planning with a server-side projection in args, e.g. [\"--query\", \"<JMESPath>\"], or page the remaining rows via get_aws_execution {execution_id, offset}"
	return resp, nil
}

// groupResults buckets rows by identical outcome, largest bucket first, and
// elides the dominant bucket's target list when it is big enough that naming
// the exceptions is cheaper than naming the members.
func groupResults(results []core.TargetResult) []map[string]any {
	order := []string{}
	buckets := map[string][]core.TargetResult{}
	for _, r := range results {
		sig := groupSignature(r)
		if _, ok := buckets[sig]; !ok {
			order = append(order, sig)
		}
		buckets[sig] = append(buckets[sig], r)
	}

	biggest := ""
	for _, sig := range order {
		if biggest == "" || len(buckets[sig]) > len(buckets[biggest]) {
			biggest = sig
		}
	}

	groups := make([]map[string]any, 0, len(order))
	for _, sig := range order {
		rows := buckets[sig]
		g := resultRow(rows[0])
		delete(g, "target")
		g["count"] = len(rows)
		if sig == biggest && len(rows) >= restElisionMin && len(rows)*2 >= len(results) {
			g["rest"] = true
		} else {
			ids := make([]string, len(rows))
			for i, r := range rows {
				ids[i] = r.Target.ID
			}
			g["targets"] = ids
		}
		groups = append(groups, g)
	}
	// Dominant group last: exceptions are what the agent scans for.
	for i, g := range groups {
		if g["rest"] == true {
			groups = append(append(groups[:i], groups[i+1:]...), g)
			break
		}
	}
	return groups
}

// pageRows returns rows[offset:offset+limit] in compact form, shrinking the
// window further if the encoded page would exceed the budget. limit 0 means
// as many rows as fit.
func pageRows(base map[string]any, results []core.TargetResult, offset, limit int) map[string]any {
	total := len(results)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}

	rows := []map[string]any{}
	size := 0
	for i := offset; i < end; i++ {
		row := resultRow(results[i])
		size += encodedLen(row)
		if size > mcpMaxResultChars && len(rows) > 0 {
			break
		}
		rows = append(rows, row)
	}

	base["results"] = rows
	base["results_total"] = total
	base["results_offset"] = offset
	base["results_shown"] = len(rows)
	if offset+len(rows) < total {
		base["results_truncated"] = true
		base["next_offset"] = offset + len(rows)
	}
	return base
}

func hasRest(groups []map[string]any) bool {
	for _, g := range groups {
		if g["rest"] == true {
			return true
		}
	}
	return false
}

// compactRaw re-encodes raw JSON without insignificant whitespace; the aws
// CLI pretty-prints with 4-space indents, which is pure token waste here.
func compactRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return json.RawMessage(buf.Bytes())
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... [truncated]"
}

func encodedLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}
