package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/0hardik1/awsmux/internal/core"
)

func TestUnmarshalArgsRejectsUnknownFields(t *testing.T) {
	var a struct {
		Profiles []string `json:"profiles"`
	}
	// A typo like "profile" must be an error: unmarshalled silently it would
	// leave an empty selector, whose documented meaning is "all profiles".
	if err := unmarshalArgs(json.RawMessage(`{"profile": ["prod-*"]}`), &a); err == nil {
		t.Error("misspelled field should be rejected, not ignored")
	}
	if err := unmarshalArgs(json.RawMessage(`{"profiles": ["prod-*"]}`), &a); err != nil {
		t.Errorf("valid args rejected: %v", err)
	}
	if err := unmarshalArgs(nil, &a); err != nil {
		t.Errorf("absent args rejected: %v", err)
	}
}

func TestSchemasForbidAdditionalProperties(t *testing.T) {
	for _, td := range tools {
		if got, ok := td.InputSchema["additionalProperties"].(bool); !ok || got {
			t.Errorf("tool %s: inputSchema must set additionalProperties false", td.Name)
		}
	}
}

func TestListTargetsAcceptsOrgFields(t *testing.T) {
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
	raw := json.RawMessage(`{"ou":["eng/prod"],"account_tags":{"env":"prod"},"org_role":"arn:aws:iam::1:role/R","org_profile":"mgmt","org_refresh":true}`)
	if err := unmarshalArgs(raw, &a); err != nil {
		t.Fatalf("unmarshalArgs: %v", err)
	}
	if len(a.OU) != 1 || a.OU[0] != "eng/prod" {
		t.Errorf("OU = %v, want [eng/prod]", a.OU)
	}
	if a.AccountTags["env"] != "prod" {
		t.Errorf("AccountTags = %v, want env=prod", a.AccountTags)
	}
	if !a.OrgRefresh || a.OrgProfile != "mgmt" {
		t.Errorf("org scalars decoded wrong: %+v", a)
	}
}

func TestUnmarshalArgsStillRejectsMisspelledOrgField(t *testing.T) {
	var a struct {
		OU []string `json:"ou"`
	}
	// "ous" is not a field: a typo must be an error, never a silent
	// unfiltered selection.
	raw := json.RawMessage(`{"ous":["eng/prod"]}`)
	if err := unmarshalArgs(raw, &a); err == nil {
		t.Fatal("misspelled field was accepted; selection would silently widen")
	}
}

func TestCompactTargetsIncludesOUAndUnreachable(t *testing.T) {
	targets := []core.Target{{
		ID:             "alpha@us-east-1",
		Profile:        "alpha",
		AccountID:      "111122223333",
		Principal:      "arn:aws:iam::111122223333:user/alpha",
		OUPath:         "eng/prod",
		OrgAccountName: "prod-web",
	}}
	orgSel := &core.OrgSelection{
		Matched: []string{"111122223333", "222233334444"},
		Unreachable: []core.OrgAccount{
			{ID: "222233334444", Name: "prod-api", OUPath: "eng/prod"},
		},
	}

	out := compactTargets(targets, orgSel)
	rows, ok := out["targets"].([]string)
	if !ok || len(rows) != 1 {
		t.Fatalf("targets = %v, want one row", out["targets"])
	}
	if !strings.Contains(rows[0], "eng/prod") {
		t.Errorf("row missing OU path: %q", rows[0])
	}
	un, ok := out["unreachable"].(map[string]any)
	if !ok {
		t.Fatalf("unreachable missing from result: %+v", out)
	}
	if un["count"] != 1 {
		t.Errorf("unreachable count = %v, want 1", un["count"])
	}
}

func TestCompactTargetsOmitsUnreachableWithoutOrg(t *testing.T) {
	targets := []core.Target{{ID: "alpha", Profile: "alpha", AccountID: "111122223333"}}
	out := compactTargets(targets, nil)
	if _, present := out["unreachable"]; present {
		t.Error("unreachable must be absent when no org selector ran; it costs tokens for nothing")
	}
}
