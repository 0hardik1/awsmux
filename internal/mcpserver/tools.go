package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/0hardik1/awsmux/internal/core"
)

// toolHandler is invoked with the raw "arguments" object from tools/call.
// A returned error is a domain error (isError result), never a JSON-RPC one.
type toolHandler func(s *server, ctx context.Context, args json.RawMessage) (any, error)

// toolDef is one tools/list entry; the unexported handler is skipped by the
// JSON encoder.
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	handler     toolHandler
}

var tools = []toolDef{
	{
		Name: "list_aws_targets",
		Description: "Discover AWS execution targets (profile plus region pairs) from the " +
			"local AWS shared config and credentials files. Use this first, before planning, to see exactly which " +
			"accounts and principals an operation would touch. By default every target " +
			"is preflighted with sts get-caller-identity, filling account_id and " +
			"principal and surfacing credential problems; set preflight to false to " +
			"skip that. Set dedupe to drop targets that resolve to the same account, " +
			"principal, and region as an earlier one. Set ou or account_tags to " +
			"select by AWS Organizations structure instead of profile name; the " +
			"response then also reports matched org accounts that have no local profile.",
		InputSchema: schemaObject(map[string]any{
			"profiles":     schemaStringArray("Shell-style globs selecting profiles (e.g. [\"prod-*\"]). Empty means all profiles."),
			"exclude":      schemaStringArray("Shell-style globs removing profiles after inclusion."),
			"regions":      schemaStringArray("Regions to expand each profile into (one target per profile per region). Empty means each profile's default region."),
			"preflight":    schemaBoolWithDefault("Verify each target's identity via STS before returning it. Defaults to true.", true),
			"dedupe":       schemaBool("Drop targets duplicating an earlier target's account, principal, and region. Implies preflight."),
			"ou":           schemaStringArray("AWS Organizations OU path globs, e.g. [\"eng/prod\"]. Matches nested OUs too, so \"eng/prod\" also selects \"eng/prod/db\". Filters on the STS-verified account id, and forces preflight on."),
			"account_tags": schemaStringMap("Org account tag filter, e.g. {\"env\":\"prod\"}. Every pair must match. Forces preflight on."),
			"org_role":     schemaString("Role ARN assumed to enumerate AWS Organizations. Enumeration only; it never affects how targets execute."),
			"org_profile":  schemaString("Profile used for the organizations calls. Empty means normal AWS resolution."),
			"org_refresh":  schemaBool("Bypass the cached organization tree (cached for one hour)."),
		}),
		handler: (*server).listTargetsTool,
	},
	{
		Name: "plan_aws_operation",
		Description: "Create an immutable, hashed execution plan for one AWS CLI operation " +
			"(service, operation, args) across the selected targets. This never runs " +
			"anything; it is the required first step before execute_aws_plan. The " +
			"response includes the risk classification and requires_approval. If " +
			"requires_approval is true (mutating, destructive, or unknown operations), " +
			"you cannot execute the plan yourself: a human must run " +
			"\"awsmux approve <plan-id>\" in their terminal and hand you the printed " +
			"approval token, which you then pass to execute_aws_plan. read_only plans " +
			"execute directly with no token. Plans expire one hour after creation. " +
			"For read operations, ALWAYS put a server-side projection in args, e.g. " +
			"[\"--query\", \"Vpcs[].VpcId\"], plus --filters where the service supports " +
			"them: it shrinks every per-target result to just the fields you need, " +
			"keeps the execution response small enough to return whole, and on large " +
			"fleets is the difference between one round trip and many.",
		InputSchema: schemaObject(map[string]any{
			"service":      schemaString("AWS CLI service, e.g. \"ec2\" or \"s3api\"."),
			"operation":    schemaString("AWS CLI operation, e.g. \"describe-instances\"."),
			"args":         schemaStringArray("Extra AWS CLI arguments appended after the operation. For reads, include a JMESPath projection, e.g. [\"--query\", \"Vpcs[].VpcId\"]; unprojected outputs on large fleets get truncated and cost extra round trips."),
			"profiles":     schemaStringArray("Shell-style globs selecting profiles. Empty means all profiles."),
			"exclude":      schemaStringArray("Shell-style globs removing profiles after inclusion."),
			"regions":      schemaStringArray("Regions to expand each profile into. Empty means each profile's default region."),
			"dedupe":       schemaBool("Drop targets duplicating an earlier target's account, principal, and region."),
			"target_ids":   schemaStringArray("Restrict the plan to these resolved target ids (\"profile@region\" or \"profile\"), typically from list_aws_targets. Unknown ids are an error."),
			"ou":           schemaStringArray("AWS Organizations OU path globs, e.g. [\"eng/prod\"]. Matches nested OUs too, so \"eng/prod\" also selects \"eng/prod/db\". Filters on the STS-verified account id, and forces preflight on."),
			"account_tags": schemaStringMap("Org account tag filter, e.g. {\"env\":\"prod\"}. Every pair must match. Forces preflight on."),
			"org_role":     schemaString("Role ARN assumed to enumerate AWS Organizations. Enumeration only; it never affects how targets execute."),
			"org_profile":  schemaString("Profile used for the organizations calls. Empty means normal AWS resolution."),
			"org_refresh":  schemaBool("Bypass the cached organization tree (cached for one hour)."),
		}, "service", "operation"),
		handler: (*server).planOperationTool,
	},
	{
		Name: "execute_aws_plan",
		Description: "Execute a plan created by plan_aws_operation across all its targets " +
			"with a concurrent worker pool. Each plan executes at most once. If the plan " +
			"has requires_approval true you MUST supply approval_token, obtained by a " +
			"human running \"awsmux approve <plan-id>\"; there is no bypass, so if you " +
			"have no token, ask the human to approve first. read_only plans need no " +
			"token. With wait true (the default) this blocks and returns the finished " +
			"execution: a summary plus results grouped by identical outcome, so a " +
			"fleet-wide check where most targets agree comes back tiny. Oversized " +
			"results are truncated with a next_offset for paging via " +
			"get_aws_execution. With wait false it returns an execution_id " +
			"immediately; poll get_aws_execution and use cancel_aws_execution to " +
			"stop it.",
		InputSchema: schemaObject(map[string]any{
			"plan_id":        schemaString("Id of the plan to execute."),
			"approval_token": schemaString("Approval token printed by \"awsmux approve <plan-id>\". Required for any non-read_only plan."),
			"concurrency":    schemaInt("Worker pool size. Defaults to 100 (one aws CLI subprocess per in-flight target); lower it on memory-constrained machines."),
			"timeout_s":      schemaInt("Per-target timeout in seconds. 0 or omitted means no timeout."),
			"max_errors":     schemaInt("Stop scheduling new targets after this many failures; remaining targets are skipped. 0 or omitted means no cap."),
			"wait":           schemaBoolWithDefault("Wait for the run to finish and return the full execution. Set false to run in the background. Defaults to true.", true),
		}, "plan_id"),
		handler: (*server).executePlanTool,
	},
	{
		Name: "get_aws_execution",
		Description: "Fetch one execution by id. A still-running background execution " +
			"(started via execute_aws_plan with wait false) reports status \"running\" " +
			"with a count of targets completed so far; a finished execution returns " +
			"grouped results and a summary, like execute_aws_plan. Pass offset (and " +
			"optionally limit) to page through per-target rows of a large result " +
			"instead. Also works for any past execution in history.",
		InputSchema: schemaObject(map[string]any{
			"execution_id": schemaString("Id of the execution to fetch."),
			"offset":       schemaInt("Return plain per-target rows starting at this index (plan target order) instead of grouped results."),
			"limit":        schemaInt("Maximum rows to return with offset paging. Omitted or 0 means as many as fit."),
		}, "execution_id"),
		handler: (*server).getExecutionTool,
	},
	{
		Name: "cancel_aws_execution",
		Description: "Cancel a running background execution started via execute_aws_plan " +
			"with wait false. In-flight targets are interrupted, unstarted targets are " +
			"marked skipped, and the partial execution is persisted to history. Errors " +
			"if the execution id is unknown or the run already finished.",
		InputSchema: schemaObject(map[string]any{
			"execution_id": schemaString("Id of the running execution to cancel."),
		}, "execution_id"),
		handler: (*server).cancelExecutionTool,
	},
}

func findTool(name string) *toolDef {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// --- JSON Schema helpers -------------------------------------------------

func schemaObject(props map[string]any, required ...string) map[string]any {
	// additionalProperties false plus DisallowUnknownFields in unmarshalArgs:
	// a typo like "profile" for "profiles" must be an error, not an empty
	// selector that silently widens scope to every profile.
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func schemaString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func schemaStringArray(desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": desc,
	}
}

func schemaBool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func schemaBoolWithDefault(desc string, def bool) map[string]any {
	return map[string]any{"type": "boolean", "description": desc, "default": def}
}

func schemaInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func schemaStringMap(desc string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string"},
		"description":          desc,
	}
}

// unmarshalArgs decodes tools/call arguments; absent arguments mean {}.
// Unknown fields are an error: on a tool whose empty selector means "all
// profiles", a misspelled field name must not silently widen scope.
func unmarshalArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// --- Handlers ------------------------------------------------------------

func (s *server) listTargetsTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Profiles    []string          `json:"profiles"`
		Exclude     []string          `json:"exclude"`
		Regions     []string          `json:"regions"`
		Preflight   *bool             `json:"preflight"`
		Dedupe      bool              `json:"dedupe"`
		OU          []string          `json:"ou"`
		AccountTags map[string]string `json:"account_tags"`
		OrgRole     string            `json:"org_role"`
		OrgProfile  string            `json:"org_profile"`
		OrgRefresh  bool              `json:"org_refresh"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return nil, err
	}
	targets, orgSel, err := core.ResolveTargetsWithOrg(ctx, core.Selector{
		Profiles:    a.Profiles,
		Exclude:     a.Exclude,
		Regions:     a.Regions,
		Preflight:   a.Preflight == nil || *a.Preflight,
		Dedupe:      a.Dedupe,
		OU:          a.OU,
		AccountTags: a.AccountTags,
		OrgRole:     a.OrgRole,
		OrgProfile:  a.OrgProfile,
		OrgRefresh:  a.OrgRefresh,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve targets: %w", err)
	}
	return compactTargets(targets, orgSel), nil
}

func (s *server) planOperationTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Service     string            `json:"service"`
		Operation   string            `json:"operation"`
		Args        []string          `json:"args"`
		Profiles    []string          `json:"profiles"`
		Exclude     []string          `json:"exclude"`
		Regions     []string          `json:"regions"`
		Dedupe      bool              `json:"dedupe"`
		TargetIDs   []string          `json:"target_ids"`
		OU          []string          `json:"ou"`
		AccountTags map[string]string `json:"account_tags"`
		OrgRole     string            `json:"org_role"`
		OrgProfile  string            `json:"org_profile"`
		OrgRefresh  bool              `json:"org_refresh"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Service == "" || a.Operation == "" {
		return nil, fmt.Errorf("service and operation are required")
	}
	// Preflight is forced on for planning: the plan hash binds to the
	// identities it was approved against.
	targets, err := core.ResolveTargets(ctx, core.Selector{
		Profiles:    a.Profiles,
		Exclude:     a.Exclude,
		Regions:     a.Regions,
		Preflight:   true,
		Dedupe:      a.Dedupe,
		OU:          a.OU,
		AccountTags: a.AccountTags,
		OrgRole:     a.OrgRole,
		OrgProfile:  a.OrgProfile,
		OrgRefresh:  a.OrgRefresh,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve targets: %w", err)
	}
	if len(a.TargetIDs) > 0 {
		targets, err = filterByID(targets, a.TargetIDs)
		if err != nil {
			return nil, err
		}
	}
	plan, err := core.NewPlan(a.Service, a.Operation, a.Args, targets, 0)
	if err != nil {
		return nil, fmt.Errorf("build plan: %w", err)
	}
	if err := core.SavePlan(plan); err != nil {
		return nil, fmt.Errorf("save plan: %w", err)
	}
	return planResponse(plan)
}

// filterByID keeps the resolved targets whose ID is in want, preserving
// resolution order; ids matching nothing are a domain error listing what
// would have been valid.
func filterByID(targets []core.Target, want []string) ([]core.Target, error) {
	wanted := make(map[string]bool, len(want))
	for _, id := range want {
		wanted[id] = true
	}
	filtered := make([]core.Target, 0, len(want))
	for _, t := range targets {
		if wanted[t.ID] {
			filtered = append(filtered, t)
			delete(wanted, t.ID)
		}
	}
	if len(wanted) > 0 {
		valid := make([]string, 0, len(targets))
		for _, t := range targets {
			valid = append(valid, t.ID)
		}
		if extra := len(valid) - 10; extra > 0 {
			valid = append(valid[:10], fmt.Sprintf("... (%d more)", extra))
		}
		unknown := slices.Sorted(maps.Keys(wanted))
		return nil, fmt.Errorf("unknown target ids: %s (valid ids: %s)",
			strings.Join(unknown, ", "), strings.Join(valid, ", "))
	}
	return filtered, nil
}

func (s *server) executePlanTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		PlanID        string `json:"plan_id"`
		ApprovalToken string `json:"approval_token"`
		Concurrency   int    `json:"concurrency"`
		TimeoutS      int    `json:"timeout_s"`
		MaxErrors     int    `json:"max_errors"`
		Wait          *bool  `json:"wait"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.PlanID == "" {
		return nil, fmt.Errorf("plan_id is required")
	}

	plan, err := core.LoadPlan(a.PlanID)
	if err != nil {
		return nil, fmt.Errorf("load plan: %w", err)
	}
	if err := core.CheckApproval(plan, a.ApprovalToken); err != nil {
		return nil, fmt.Errorf("approval check failed: %w", err)
	}
	// Re-verify live identities before consuming the plan (STS may take a
	// few seconds, so this runs outside execMu): approval bound specific
	// accounts, and changed credentials must not redirect the run. Failing
	// here leaves the plan unclaimed, so it can be retried once fixed.
	if err := core.VerifyIdentities(ctx, plan.Targets); err != nil {
		return nil, err
	}

	// Claim, mark executed, and save under one lock. The claim file is the
	// cross-process "execute at most once" gate (another MCP server or the
	// CLI cannot race it); execMu only serializes sibling handlers in here.
	execID := core.NewID("exec")
	s.execMu.Lock()
	if err := core.ClaimPlan(plan.ID, execID); err != nil {
		s.execMu.Unlock()
		return nil, err
	}
	plan.Status = core.PlanExecuted
	plan.ExecutionID = execID
	if err := core.SavePlan(plan); err != nil {
		s.execMu.Unlock()
		return nil, fmt.Errorf("save plan: %w", err)
	}
	s.execMu.Unlock()

	opts := core.ExecOptions{
		Concurrency: a.Concurrency,
		Timeout:     time.Duration(a.TimeoutS) * time.Second,
		MaxErrors:   a.MaxErrors,
	}

	if a.Wait == nil || *a.Wait {
		exec := core.Execute(ctx, plan.Targets, plan.Service, plan.Operation, plan.Args, opts, nil)
		stampExecution(exec, execID, plan)
		if err := core.SaveExecution(exec); err != nil {
			return nil, fmt.Errorf("save execution: %w", err)
		}
		return executionResponse(exec, 0, 0)
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.reg.add(execID, cancel)
	go func() {
		defer cancel()
		exec := core.Execute(runCtx, plan.Targets, plan.Service, plan.Operation, plan.Args, opts,
			func(core.TargetResult) { s.reg.bump(execID) })
		stampExecution(exec, execID, plan)
		if err := core.SaveExecution(exec); err != nil {
			s.logf("save execution %s: %v", execID, err)
		}
		// Persist before removal so the id is always findable somewhere.
		s.reg.remove(execID)
		s.logf("execution %s finished: %s", execID, exec.Status)
	}()
	return map[string]any{"execution_id": execID, "status": "running"}, nil
}

// stampExecution overrides the executor-minted id with the one already
// persisted on the plan, and links the execution back to its plan.
func stampExecution(e *core.Execution, execID string, plan *core.Plan) {
	e.ID = execID
	e.PlanID = plan.ID
	e.Classification = plan.Classification
}

func (s *server) getExecutionTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		ExecutionID string `json:"execution_id"`
		Offset      int    `json:"offset"`
		Limit       int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.ExecutionID == "" {
		return nil, fmt.Errorf("execution_id is required")
	}
	if snap, running := s.reg.snapshot(a.ExecutionID); running {
		return map[string]any{
			"execution_id":      a.ExecutionID,
			"status":            "running",
			"started_at":        snap.startedAt,
			"results_completed": snap.completed,
		}, nil
	}
	exec, err := core.LoadExecution(a.ExecutionID)
	if err != nil {
		return nil, fmt.Errorf("load execution: %w", err)
	}
	return executionResponse(exec, a.Offset, a.Limit)
}

func (s *server) cancelExecutionTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		ExecutionID string `json:"execution_id"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.ExecutionID == "" {
		return nil, fmt.Errorf("execution_id is required")
	}
	if err := s.reg.cancel(a.ExecutionID); err != nil {
		return nil, err
	}
	return map[string]any{"cancelled": true}, nil
}
