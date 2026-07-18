package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// AGENT CONTRACT (plan): implement every stub in this file. Keep the
// signatures exactly as written; add unexported helpers in this file freely.

// DefaultPlanTTL is how long a plan stays executable.
const DefaultPlanTTL = 1 * time.Hour

// NewPlan builds a plan for service/operation over targets: classify the
// operation, ask policy whether approval is required, set CreatedAt /
// ExpiresAt (ttl <= 0 means DefaultPlanTTL), Status PlanPlanned, ID via
// NewID("plan"), PolicyVersion, then Hash = ComputeHash(). Error when
// targets is empty.
func NewPlan(service, operation string, args []string, targets []Target, ttl time.Duration) (*Plan, error) {
	if len(targets) == 0 {
		return nil, errors.New("plan requires at least one target")
	}
	if ttl <= 0 {
		ttl = DefaultPlanTTL
	}
	class := Classify(service, operation)
	now := time.Now().UTC()
	p := &Plan{
		ID:               NewID("plan"),
		CreatedAt:        now,
		ExpiresAt:        now.Add(ttl),
		Service:          service,
		Operation:        operation,
		Args:             args,
		Targets:          targets,
		Classification:   class,
		RequiresApproval: RequiresApproval(class),
		PolicyVersion:    PolicyVersion,
		Status:           PlanPlanned,
	}
	p.Hash = p.ComputeHash()
	return p, nil
}

// ComputeHash returns the hex sha256 over a canonical JSON encoding of the
// fields that define the plan: service, operation, args, each target's
// (ID, Profile, Region, AccountID, Principal), classification, policy
// version, and expires_at (RFC 3339). Field order must be fixed so the hash
// is reproducible; approval binds to this hash.
func (p *Plan) ComputeHash() string {
	type targetKey struct {
		ID        string `json:"id"`
		Profile   string `json:"profile"`
		Region    string `json:"region"`
		AccountID string `json:"account_id"`
		Principal string `json:"principal"`
	}
	keys := make([]targetKey, len(p.Targets))
	for i, t := range p.Targets {
		keys[i] = targetKey{
			ID:        t.ID,
			Profile:   t.Profile,
			Region:    t.Region,
			AccountID: t.AccountID,
			Principal: t.Principal,
		}
	}
	// Normalize an empty args slice to nil so the hash survives the JSON
	// round-trip through the store (omitempty drops empty args on save).
	args := p.Args
	if len(args) == 0 {
		args = nil
	}
	payload := struct {
		Service        string         `json:"service"`
		Operation      string         `json:"operation"`
		Args           []string       `json:"args"`
		Targets        []targetKey    `json:"targets"`
		Classification Classification `json:"classification"`
		PolicyVersion  string         `json:"policy_version"`
		ExpiresAt      string         `json:"expires_at"`
	}{
		Service:        p.Service,
		Operation:      p.Operation,
		Args:           args,
		Targets:        keys,
		Classification: p.Classification,
		PolicyVersion:  p.PolicyVersion,
		ExpiresAt:      p.ExpiresAt.UTC().Format(time.RFC3339),
	}
	// Marshaling a struct of strings cannot fail.
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Expired reports whether the plan is past ExpiresAt.
func (p *Plan) Expired(now time.Time) bool {
	return now.After(p.ExpiresAt)
}

// NewID returns "<prefix>-<26 lowercase base32 chars>" from
// crypto/rand + the current unix millis so IDs sort roughly by time
// (a ULID-style layout is fine; no dependency).
func NewID(prefix string) string {
	var b [16]byte
	ms := time.Now().UnixMilli()
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	// crypto/rand.Read never returns an error as of Go 1.24.
	_, _ = rand.Read(b[6:])
	return prefix + "-" + encodeBase32(b)
}

// encodeBase32 encodes 16 bytes as 26 chars of a lowercase Crockford-style
// alphabet, ULID layout: two implicit leading zero bits pad 128 bits to 130,
// then each 5-bit group maps to one character, most significant bits first.
func encodeBase32(b [16]byte) string {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	var out [26]byte
	acc := uint(0)
	nbits := 2 // the two zero pad bits
	j := 0
	for _, by := range b {
		acc = acc<<8 | uint(by)
		nbits += 8
		for nbits >= 5 {
			out[j] = alphabet[(acc>>(nbits-5))&31]
			nbits -= 5
			j++
		}
	}
	return string(out[:])
}
