package core

import (
	"strings"
	"testing"
	"time"
)

// basePlan returns a fully deterministic plan (fixed timestamps, no NewPlan
// randomness) so hash comparisons are exact.
func basePlan() *Plan {
	created := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	p := &Plan{
		ID:        "plan-test",
		CreatedAt: created,
		ExpiresAt: created.Add(time.Hour),
		Service:   "ec2",
		Operation: "terminate-instances",
		Args:      []string{"--instance-ids", "i-123"},
		Targets: []Target{
			{ID: "prod@us-east-1", Profile: "prod", Region: "us-east-1", AccountID: "111111111111", Principal: "arn:aws:iam::111111111111:role/admin"},
			{ID: "staging@eu-west-1", Profile: "staging", Region: "eu-west-1", AccountID: "222222222222", Principal: "arn:aws:iam::222222222222:role/admin"},
		},
		Classification:   ClassDestructive,
		RequiresApproval: true,
		PolicyVersion:    PolicyVersion,
		Status:           PlanPlanned,
	}
	p.Hash = p.ComputeHash()
	return p
}

func TestComputeHashDeterminism(t *testing.T) {
	p1 := basePlan()
	p2 := basePlan()
	if p1.ComputeHash() != p2.ComputeHash() {
		t.Fatal("identical plans should hash equal")
	}

	changed := basePlan()
	changed.Args = []string{"--instance-ids", "i-999"}
	if changed.ComputeHash() == p1.Hash {
		t.Error("changing an arg must change the hash")
	}

	changed = basePlan()
	changed.Targets[0].AccountID = "333333333333"
	if changed.ComputeHash() == p1.Hash {
		t.Error("changing a target must change the hash")
	}

	changed = basePlan()
	changed.Targets = changed.Targets[:1]
	if changed.ComputeHash() == p1.Hash {
		t.Error("dropping a target must change the hash")
	}
}

func TestComputeHashArgsNilVsEmpty(t *testing.T) {
	// omitempty drops empty args on save, so nil and []string{} must agree.
	p := basePlan()
	p.Args = nil
	nilHash := p.ComputeHash()
	p.Args = []string{}
	if p.ComputeHash() != nilHash {
		t.Error("nil args and empty args must hash the same")
	}
}

func TestNewPlan(t *testing.T) {
	targets := []Target{{ID: "dev", Profile: "dev"}}

	if _, err := NewPlan("ec2", "describe-instances", nil, nil, 0); err == nil {
		t.Error("NewPlan with no targets should error")
	}

	p, err := NewPlan("ec2", "describe-instances", nil, targets, 0)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if p.Classification != ClassReadOnly {
		t.Errorf("classification = %q, want read_only", p.Classification)
	}
	if p.RequiresApproval {
		t.Error("read_only plan should not require approval")
	}
	if p.Status != PlanPlanned {
		t.Errorf("status = %q, want planned", p.Status)
	}
	if !strings.HasPrefix(p.ID, "plan-") {
		t.Errorf("id = %q, want plan- prefix", p.ID)
	}
	if p.Hash != p.ComputeHash() {
		t.Error("stored hash does not match ComputeHash")
	}
	if got := p.ExpiresAt.Sub(p.CreatedAt); got != DefaultPlanTTL {
		t.Errorf("ttl = %v, want default %v", got, DefaultPlanTTL)
	}

	m, err := NewPlan("ec2", "terminate-instances", nil, targets, 0)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if !m.RequiresApproval {
		t.Error("destructive plan must require approval")
	}
}

func TestCheckApprovalReadOnly(t *testing.T) {
	p, err := NewPlan("ec2", "describe-instances", nil, []Target{{ID: "dev", Profile: "dev"}}, 0)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if err := CheckApproval(p, ""); err != nil {
		t.Errorf("read_only plan should pass with empty token, got: %v", err)
	}
}

func TestCheckApprovalMutating(t *testing.T) {
	newMutating := func(t *testing.T) *Plan {
		t.Helper()
		p, err := NewPlan("iam", "create-user", []string{"--user-name", "x"}, []Target{{ID: "dev", Profile: "dev"}}, 0)
		if err != nil {
			t.Fatalf("NewPlan: %v", err)
		}
		return p
	}

	t.Run("no token", func(t *testing.T) {
		p := newMutating(t)
		if err := CheckApproval(p, ""); err == nil {
			t.Error("mutating plan without any approval should fail")
		}
	})

	t.Run("minted token passes", func(t *testing.T) {
		p := newMutating(t)
		token, err := NewApprovalToken(p)
		if err != nil {
			t.Fatalf("NewApprovalToken: %v", err)
		}
		if token == "" {
			t.Fatal("empty token minted")
		}
		if p.Status != PlanApproved {
			t.Errorf("status = %q, want approved", p.Status)
		}
		if err := CheckApproval(p, token); err != nil {
			t.Errorf("valid token should pass, got: %v", err)
		}
	})

	t.Run("wrong token fails", func(t *testing.T) {
		p := newMutating(t)
		if _, err := NewApprovalToken(p); err != nil {
			t.Fatalf("NewApprovalToken: %v", err)
		}
		if err := CheckApproval(p, "deadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
			t.Error("wrong token should fail")
		}
	})

	t.Run("hash tamper fails", func(t *testing.T) {
		p := newMutating(t)
		token, err := NewApprovalToken(p)
		if err != nil {
			t.Fatalf("NewApprovalToken: %v", err)
		}
		p.Args = append(p.Args, "--path", "/evil/")
		if err := CheckApproval(p, token); err == nil {
			t.Error("tampered plan should fail even with a valid token")
		}
	})

	t.Run("already executed fails", func(t *testing.T) {
		p := newMutating(t)
		token, err := NewApprovalToken(p)
		if err != nil {
			t.Fatalf("NewApprovalToken: %v", err)
		}
		p.Status = PlanExecuted
		if err := CheckApproval(p, token); err == nil {
			t.Error("executed plan should not pass approval again")
		}
	})
}

func TestCheckApprovalExpired(t *testing.T) {
	p, err := NewPlan("iam", "create-user", nil, []Target{{ID: "dev", Profile: "dev"}}, 0)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	token, err := NewApprovalToken(p)
	if err != nil {
		t.Fatalf("NewApprovalToken: %v", err)
	}
	p.ExpiresAt = time.Now().Add(-time.Minute)
	if err := CheckApproval(p, token); err == nil {
		t.Error("expired plan should fail CheckApproval")
	}
	if _, err := NewApprovalToken(p); err == nil {
		t.Error("expired plan should not be approvable")
	}
}

func TestExpired(t *testing.T) {
	p := basePlan()
	if p.Expired(p.ExpiresAt.Add(-time.Second)) {
		t.Error("plan should not be expired before ExpiresAt")
	}
	if !p.Expired(p.ExpiresAt.Add(time.Second)) {
		t.Error("plan should be expired after ExpiresAt")
	}
}

func TestNewID(t *testing.T) {
	a := NewID("plan")
	b := NewID("plan")
	if a == b {
		t.Error("two ids should differ")
	}
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, "plan-") {
			t.Errorf("id %q missing prefix", id)
		}
		if got := len(strings.TrimPrefix(id, "plan-")); got != 26 {
			t.Errorf("id %q suffix length = %d, want 26", id, got)
		}
	}
}
