package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupStore(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AWSMUX_HOME", home)
	return home
}

func mkStoredPlan(t *testing.T) *Plan {
	t.Helper()
	p, err := NewPlan("ec2", "describe-instances", []string{"--max-items", "5"},
		[]Target{{ID: "dev@us-east-1", Profile: "dev", Region: "us-east-1"}}, 0)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return p
}

func TestPlanSaveLoadRoundtrip(t *testing.T) {
	setupStore(t)
	p := mkStoredPlan(t)
	if err := SavePlan(p); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	got, err := LoadPlan(p.ID)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if got.ID != p.ID || got.Hash != p.Hash || got.Service != p.Service ||
		got.Operation != p.Operation || got.Status != p.Status {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, p)
	}
	// The stored hash must still verify after the JSON roundtrip.
	if got.ComputeHash() != got.Hash {
		t.Error("loaded plan fails its own hash check")
	}
	if len(got.Args) != 2 || got.Args[0] != "--max-items" {
		t.Errorf("args roundtrip mismatch: %v", got.Args)
	}
}

func TestPlanPrefixLookup(t *testing.T) {
	setupStore(t)
	p := mkStoredPlan(t)
	if err := SavePlan(p); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	got, err := LoadPlan(p.ID[:12])
	if err != nil {
		t.Fatalf("LoadPlan by prefix: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("prefix lookup returned %s, want %s", got.ID, p.ID)
	}

	if _, err := LoadPlan("plan-nosuchid"); err == nil {
		t.Error("unknown id should error")
	}
	if _, err := LoadPlan(""); err == nil {
		t.Error("empty id should error")
	}
}

func TestPlanAmbiguousPrefix(t *testing.T) {
	setupStore(t)
	p1 := mkStoredPlan(t)
	p2 := mkStoredPlan(t)
	if err := SavePlan(p1); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if err := SavePlan(p2); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	// "plan-" prefixes both stored plans.
	_, err := LoadPlan("plan-")
	if err == nil {
		t.Fatal("ambiguous prefix should error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguity, got: %v", err)
	}
}

func mkExecution(t *testing.T) *Execution {
	t.Helper()
	now := time.Now().UTC()
	results := []TargetResult{
		{Target: Target{ID: "dev@us-east-1", Profile: "dev", Region: "us-east-1"}, Status: StatusSuccess},
		{Target: Target{ID: "prod@us-east-1", Profile: "prod", Region: "us-east-1"}, Status: StatusError, ExitCode: 254, ErrorCode: "CommandFailed"},
	}
	return &Execution{
		ID:             NewID("exec"),
		Service:        "ec2",
		Operation:      "describe-instances",
		Classification: ClassReadOnly,
		StartedAt:      now,
		FinishedAt:     now.Add(2 * time.Second),
		Status:         "completed",
		Results:        results,
		Summary:        Summarize(results),
	}
}

func TestExecutionSaveLoadRoundtrip(t *testing.T) {
	setupStore(t)
	e := mkExecution(t)
	if err := SaveExecution(e); err != nil {
		t.Fatalf("SaveExecution: %v", err)
	}

	got, err := LoadExecution(e.ID)
	if err != nil {
		t.Fatalf("LoadExecution: %v", err)
	}
	if got.ID != e.ID || got.Service != e.Service || got.Status != e.Status {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, e)
	}
	if len(got.Results) != 2 || got.Results[1].Status != StatusError {
		t.Errorf("results roundtrip mismatch: %+v", got.Results)
	}
	if got.Summary != e.Summary {
		t.Errorf("summary roundtrip mismatch: got %+v, want %+v", got.Summary, e.Summary)
	}

	// Prefix lookup works for executions too.
	byPrefix, err := LoadExecution(e.ID[:12])
	if err != nil {
		t.Fatalf("LoadExecution by prefix: %v", err)
	}
	if byPrefix.ID != e.ID {
		t.Errorf("prefix lookup returned %s, want %s", byPrefix.ID, e.ID)
	}
}

func TestExecutionIndexNoDuplicateOnDoubleSave(t *testing.T) {
	home := setupStore(t)
	e := mkExecution(t)
	if err := SaveExecution(e); err != nil {
		t.Fatalf("SaveExecution: %v", err)
	}
	e.Status = "completed"
	if err := SaveExecution(e); err != nil {
		t.Fatalf("second SaveExecution: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, "executions", "index.jsonl"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	lines := 0
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("index has %d lines after double save, want 1", lines)
	}

	execs, err := ListExecutions()
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(execs) != 1 || execs[0].ID != e.ID {
		t.Errorf("ListExecutions = %d entries, want exactly the one saved", len(execs))
	}
}

func TestListPlansNewestFirst(t *testing.T) {
	setupStore(t)
	p1 := mkStoredPlan(t)
	p2 := mkStoredPlan(t)
	p1.CreatedAt = time.Now().UTC().Add(-time.Hour)
	if err := SavePlan(p1); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if err := SavePlan(p2); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	plans, err := ListPlans()
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2", len(plans))
	}
	if plans[0].ID != p2.ID {
		t.Errorf("newest plan should be first, got %s", plans[0].ID)
	}
}

func TestDirCreatesLayout(t *testing.T) {
	home := setupStore(t)
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if dir != home {
		t.Errorf("Dir() = %q, want %q", dir, home)
	}
	for _, sub := range []string{"plans", "executions"} {
		if fi, err := os.Stat(filepath.Join(home, sub)); err != nil || !fi.IsDir() {
			t.Errorf("missing subdir %s: %v", sub, err)
		}
	}
}
