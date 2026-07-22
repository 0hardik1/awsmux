package core

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"
)

// AGENT CONTRACT (policy): implement every stub in this file. Keep the
// signatures exactly as written; add unexported helpers in this file freely.

// PolicyVersion is stamped into plans; bump when approval rules change.
const PolicyVersion = "v1"

// RequiresApproval reports whether a classification needs an approval
// token: ReadOnly runs freely; Mutating, Destructive, and Unknown all
// require one.
func RequiresApproval(c Classification) bool {
	return c != ClassReadOnly
}

// NewApprovalToken mints a random 32-hex-char token for the plan, stores
// sha256(token) in p.ApprovalHash, sets ApprovedAt and Status PlanApproved,
// and returns the raw token. Error when the plan is expired or cancelled.
// The caller persists the plan; the raw token is never stored.
func NewApprovalToken(p *Plan) (string, error) {
	now := time.Now()
	if p.Expired(now) {
		return "", fmt.Errorf("cannot approve plan %s: expired at %s", p.ID, p.ExpiresAt.Format(time.RFC3339))
	}
	if p.Status == PlanCancelled {
		return "", fmt.Errorf("cannot approve plan %s: plan is cancelled", p.ID)
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate approval token: %w", err)
	}
	token := hex.EncodeToString(buf[:])
	p.ApprovalHash = hashToken(token)
	at := now.UTC()
	p.ApprovedAt = &at
	p.Status = PlanApproved
	return token, nil
}

// CheckApproval decides whether the plan may execute now:
//   - Expired or cancelled or already executed: error.
//   - Hash mismatch (p.ComputeHash() != p.Hash): error, the plan was
//     tampered with after approval.
//   - Args containing reserved global options (--profile/--region): error.
//     NewPlan already rejects these; rechecking here covers plans stored
//     before that rule existed, since the hash proves consistency, not
//     provenance.
//   - RequiresApproval and sha256(token) != ApprovalHash (or no approval
//     ever minted): error. ReadOnly plans pass with an empty token.
//
// Errors from this function map to ExitApprovalRequired.
func CheckApproval(p *Plan, token string) error {
	if p.Expired(time.Now()) {
		return fmt.Errorf("plan %s expired at %s: create a new plan", p.ID, p.ExpiresAt.Format(time.RFC3339))
	}
	if err := ValidateArgs(p.Args); err != nil {
		return fmt.Errorf("plan %s: %w", p.ID, err)
	}
	if p.Status == PlanCancelled {
		return fmt.Errorf("plan %s was cancelled", p.ID)
	}
	if p.Status == PlanExecuted {
		return fmt.Errorf("plan %s was already executed", p.ID)
	}
	if p.ComputeHash() != p.Hash {
		return fmt.Errorf("plan %s hash mismatch: plan was modified after approval", p.ID)
	}
	if !p.RequiresApproval {
		return nil
	}
	if p.ApprovalHash == "" {
		return fmt.Errorf("plan %s has not been approved: run \"awsmux approve %s\" to get a token", p.ID, p.ID)
	}
	if subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(p.ApprovalHash)) != 1 {
		return fmt.Errorf("invalid approval token for plan %s: run \"awsmux approve %s\" to get a new token", p.ID, p.ID)
	}
	return nil
}

// hashToken returns the hex sha256 of a raw approval token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
