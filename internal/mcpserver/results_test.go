package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/0hardik1/awsmux/internal/core"
)

func mkResult(id string, status core.ResultStatus, value string) core.TargetResult {
	r := core.TargetResult{
		Target: core.Target{ID: id, Profile: id, AccountID: "200000000000",
			Principal: "arn:aws:iam::200000000000:root"},
		Status:     status,
		DurationMS: 123,
	}
	if value != "" {
		r.Result = json.RawMessage(value)
	}
	return r
}

func mkExecution(results []core.TargetResult) *core.Execution {
	return &core.Execution{
		ID: "exec-1", Service: "ec2", Operation: "describe-security-groups",
		Status: "completed", Results: results, Summary: core.Summarize(results),
	}
}

func TestExecutionResponseGroupsIdenticalOutcomes(t *testing.T) {
	// 99 targets agree, 1 differs: the response must name the exception and
	// elide the majority, not repeat 100 rows.
	results := []core.TargetResult{mkResult("needle@us-east-1", core.StatusSuccess, `[{"GroupId":"sg-1"}]`)}
	for i := 0; i < 99; i++ {
		results = append(results, mkResult(fmt.Sprintf("t%d@us-east-1", i), core.StatusSuccess, `[]`))
	}
	resp, err := executionResponse(mkExecution(results), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	groups := resp["results"].([]map[string]any)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	if got := groups[0]["targets"].([]string); len(got) != 1 || got[0] != "needle@us-east-1" {
		t.Errorf("exception group should name only the needle, got %v", got)
	}
	if groups[1]["rest"] != true || groups[1]["count"] != 99 {
		t.Errorf("majority group should be elided as rest with count 99, got %v", groups[1])
	}
	if resp["results_note"] == nil {
		t.Error("rest elision must ship its explanatory note")
	}
	encoded, _ := json.Marshal(resp)
	if len(encoded) > 2_000 {
		t.Errorf("100-target agree/except response should be tiny, got %d bytes", len(encoded))
	}
}

func TestExecutionResponseSmallGroupsListAllTargets(t *testing.T) {
	// Below restElisionMin nothing is elided; membership stays explicit.
	results := []core.TargetResult{}
	for i := 0; i < 5; i++ {
		results = append(results, mkResult(fmt.Sprintf("t%d", i), core.StatusSuccess, `[]`))
	}
	resp, _ := executionResponse(mkExecution(results), 0, 0)
	groups := resp["results"].([]map[string]any)
	if len(groups) != 1 || groups[0]["rest"] == true {
		t.Fatalf("small fleet must not elide targets: %v", groups)
	}
	if got := groups[0]["targets"].([]string); len(got) != 5 {
		t.Errorf("want 5 explicit targets, got %v", got)
	}
}

func TestExecutionResponseFallsBackToPagingWhenOversized(t *testing.T) {
	// Every target distinct and fat: grouping cannot help, so the response
	// must self-truncate under the budget and hand back a next_offset.
	results := []core.TargetResult{}
	for i := 0; i < 200; i++ {
		results = append(results, mkResult(fmt.Sprintf("t%d", i), core.StatusSuccess,
			fmt.Sprintf(`{"unique":%d,"pad":%q}`, i, strings.Repeat("x", 400))))
	}
	resp, _ := executionResponse(mkExecution(results), 0, 0)
	if resp["results_truncated"] != true {
		t.Fatal("oversized distinct results must truncate")
	}
	shown := resp["results_shown"].(int)
	next := resp["next_offset"].(int)
	if shown == 0 || next != shown {
		t.Errorf("paging bookkeeping broken: shown=%d next=%d", shown, next)
	}
	if encoded, _ := json.Marshal(resp); len(encoded) > mcpMaxResultChars+4_000 {
		t.Errorf("truncated response still oversized: %d bytes", len(encoded))
	}
	if resp["hint"] == nil {
		t.Error("truncated response must carry the --query guidance hint")
	}

	// Resuming from next_offset walks the remainder without overlap.
	page2, _ := executionResponse(mkExecution(results), next, 50)
	rows := page2["results"].([]map[string]any)
	if len(rows) != 50 {
		t.Fatalf("want 50 rows, got %d", len(rows))
	}
	if rows[0]["target"] != fmt.Sprintf("t%d", next) {
		t.Errorf("page 2 should start at t%d, got %v", next, rows[0]["target"])
	}
}

func TestResultRowKeepsFailureDiagnostics(t *testing.T) {
	r := mkResult("bad", core.StatusAccessDenied, "")
	r.ExitCode = 254
	r.ErrorCode = "AccessDenied"
	r.Stderr = strings.Repeat("e", stderrCap+100)
	row := resultRow(r)
	if row["status"] != core.StatusAccessDenied || row["exit_code"] != 254 {
		t.Errorf("failure diagnostics dropped: %v", row)
	}
	if got := row["stderr"].(string); len(got) > stderrCap+len("... [truncated]") {
		t.Errorf("stderr not capped: %d chars", len(got))
	}
	ok := resultRow(mkResult("good", core.StatusSuccess, `["vpc-1"]`))
	if _, has := ok["status"]; has {
		t.Error("success rows must omit status; success is the default reading")
	}
}

func TestCompactTargetsFormat(t *testing.T) {
	got := compactTargets([]core.Target{
		{ID: "a@us-east-1", AccountID: "1", Principal: "arn:x", Duplicate: true},
		{ID: "b@us-east-1", PreflightErr: "sts failed"},
	})
	rows := got["targets"].([]string)
	if rows[0] != "a@us-east-1 1 arn:x DUPLICATE" {
		t.Errorf("row 0: %q", rows[0])
	}
	if rows[1] != "b@us-east-1 ERROR: sts failed" {
		t.Errorf("row 1: %q", rows[1])
	}
}

func TestPlanResponsePreviewsInsteadOfEchoingRoster(t *testing.T) {
	targets := make([]core.Target, 100)
	for i := range targets {
		targets[i] = core.Target{ID: fmt.Sprintf("t%d@r", i),
			AccountID: "200000000000", Principal: "arn:aws:iam::200000000000:root"}
	}
	plan, err := core.NewPlan("ec2", "describe-vpcs", nil, targets, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := planResponse(plan)
	if err != nil {
		t.Fatal(err)
	}
	if resp["target_count"] != 100 || len(resp["targets_preview"].([]string)) != targetsPreview {
		t.Errorf("want count 100 with %d-target preview, got %v", targetsPreview, resp)
	}
	if resp["targets_omitted"] != 100-targetsPreview {
		t.Errorf("targets_omitted wrong: %v", resp["targets_omitted"])
	}
	if _, has := resp["targets"]; has {
		t.Error("plan response must not echo the full roster")
	}
	if encoded, _ := json.Marshal(resp); len(encoded) > 1_500 {
		t.Errorf("plan echo should be roster-size independent, got %d bytes", len(encoded))
	}
}
