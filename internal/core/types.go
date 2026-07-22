// Package core is the awsmux execution engine: target discovery, identity
// preflight, operation classification, immutable plans with an approval
// boundary, concurrent execution, and persistent history.
//
// Design rule: everything an agent or the CLI can do goes through these types.
// The MCP server and the cobra commands are thin layers over this package.
package core

import (
	"encoding/json"
	"time"
)

// Classification is the risk class of an AWS CLI operation.
type Classification string

// The risk classes, in increasing order of required ceremony.
const (
	ClassReadOnly    Classification = "read_only"
	ClassMutating    Classification = "mutating"
	ClassDestructive Classification = "destructive"
	// ClassUnknown is treated like ClassMutating by policy (fail safe).
	ClassUnknown Classification = "unknown"
)

// ProfileSource records which shared AWS file(s) defined a profile.
type ProfileSource string

// The possible profile sources.
const (
	SourceConfig      ProfileSource = "config"
	SourceCredentials ProfileSource = "credentials"
	SourceBoth        ProfileSource = "both"
)

// Target is a single execution target: one profile in one region.
type Target struct {
	// ID is "<profile>@<region>" (or just the profile name when region is
	// empty). Stable across runs so plans can reference targets.
	ID      string `json:"id"`
	Profile string `json:"profile"`
	// Region may be empty, meaning the profile default region decides.
	Region string `json:"region,omitempty"`
	// Source is which shared AWS file(s) defined the profile.
	Source ProfileSource `json:"source,omitempty"`
	// AccountID and Principal are filled by identity preflight.
	AccountID string `json:"account_id,omitempty"`
	Principal string `json:"principal,omitempty"`
	// CredentialExpiry is best effort (SSO token cache), nil when unknown.
	CredentialExpiry *time.Time `json:"credential_expiry,omitempty"`
	// Duplicate is set when an earlier target resolved to the same
	// account, principal, and region.
	Duplicate bool `json:"duplicate,omitempty"`
	// PreflightErr holds the sts get-caller-identity failure, if any.
	PreflightErr string `json:"preflight_error,omitempty"`
}

// Profile is one profile parsed from the AWS shared config and credentials
// files.
type Profile struct {
	Name   string        `json:"name"`
	Region string        `json:"region,omitempty"`
	Source ProfileSource `json:"source,omitempty"`
}

// Identity is the result of one sts get-caller-identity call.
type Identity struct {
	Profile   string     `json:"profile"`
	AccountID string     `json:"account_id"`
	ARN       string     `json:"arn"`
	UserID    string     `json:"user_id"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CheckedAt time.Time  `json:"checked_at"`
	Err       string     `json:"error,omitempty"`
}

// Selector describes how to pick targets.
type Selector struct {
	// Profiles are shell-style globs; empty means all profiles.
	Profiles []string `json:"profiles,omitempty"`
	// Exclude are shell-style globs removed after inclusion.
	Exclude []string `json:"exclude,omitempty"`
	// Regions expands each profile into one target per region; empty means
	// one target per profile using its default region.
	Regions []string `json:"regions,omitempty"`
	// Preflight verifies identity via STS before returning targets.
	Preflight bool `json:"preflight,omitempty"`
	// Dedupe drops targets marked Duplicate (implies Preflight).
	Dedupe bool `json:"dedupe,omitempty"`
}

// PlanStatus is the lifecycle state of a plan.
type PlanStatus string

// Plan lifecycle states.
const (
	PlanPlanned   PlanStatus = "planned"
	PlanApproved  PlanStatus = "approved"
	PlanExecuted  PlanStatus = "executed"
	PlanCancelled PlanStatus = "cancelled"
)

// Plan is an immutable, hashed description of one fleet operation. The hash
// covers everything that matters; approval binds to the hash, so the plan
// cannot change between approval and execution.
type Plan struct {
	ID               string         `json:"id"`
	CreatedAt        time.Time      `json:"created_at"`
	ExpiresAt        time.Time      `json:"expires_at"`
	Service          string         `json:"service"`
	Operation        string         `json:"operation"`
	Args             []string       `json:"args,omitempty"`
	Targets          []Target       `json:"targets"`
	Classification   Classification `json:"classification"`
	RequiresApproval bool           `json:"requires_approval"`
	PolicyVersion    string         `json:"policy_version"`
	Hash             string         `json:"hash"`
	Status           PlanStatus     `json:"status"`
	// ApprovalHash is sha256(approval token); the raw token is only ever
	// printed once by `awsmux approve` and never stored.
	ApprovalHash string     `json:"approval_hash,omitempty"`
	ApprovedAt   *time.Time `json:"approved_at,omitempty"`
	ExecutionID  string     `json:"execution_id,omitempty"`
}

// ExecOptions tune the worker pool.
type ExecOptions struct {
	Concurrency int           // <=0 means DefaultConcurrency (100)
	Timeout     time.Duration // per target, 0 = none
	// MaxErrors stops scheduling new targets after N failures; 0 = no cap.
	MaxErrors          int
	StopOnAccessDenied bool
}

// ResultStatus is the outcome of one target.
type ResultStatus string

// The stable per-target failure taxonomy; agents and CI rely on these
// strings, so never rename them, only add.
const (
	StatusSuccess      ResultStatus = "success"
	StatusError        ResultStatus = "error"
	StatusAccessDenied ResultStatus = "access_denied"
	StatusExpiredCreds ResultStatus = "credential_expired"
	StatusTimeout      ResultStatus = "timeout"
	// StatusSkipped means the target never ran (threshold stop or cancel).
	StatusSkipped ResultStatus = "skipped"
)

// TargetResult is the outcome of one target's command.
type TargetResult struct {
	Target     Target       `json:"target"`
	Status     ResultStatus `json:"status"`
	ExitCode   int          `json:"exit_code"`
	DurationMS int64        `json:"duration_ms"`
	// Result holds parsed stdout when it is valid JSON; Stdout otherwise.
	Result    json.RawMessage `json:"result,omitempty"`
	Stdout    string          `json:"stdout,omitempty"`
	Stderr    string          `json:"stderr,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
}

// Summary aggregates results.
type Summary struct {
	Total             int `json:"total"`
	Succeeded         int `json:"succeeded"`
	Failed            int `json:"failed"`
	AccessDenied      int `json:"access_denied"`
	CredentialExpired int `json:"credential_expired"`
	TimedOut          int `json:"timed_out"`
	Skipped           int `json:"skipped"`
}

// Execution is one completed (or running) fleet run, persisted to history.
type Execution struct {
	ID             string         `json:"id"`
	PlanID         string         `json:"plan_id,omitempty"`
	Service        string         `json:"service"`
	Operation      string         `json:"operation"`
	Args           []string       `json:"args,omitempty"`
	Classification Classification `json:"classification"`
	StartedAt      time.Time      `json:"started_at"`
	FinishedAt     time.Time      `json:"finished_at"`
	// Stopped is true when MaxErrors or StopOnAccessDenied halted the run.
	Stopped bool           `json:"stopped,omitempty"`
	Status  string         `json:"status"` // running | completed | cancelled
	Results []TargetResult `json:"results"`
	Summary Summary        `json:"summary"`
}

// Stable exit codes. Agents and CI depend on these; never renumber.
const (
	ExitOK                 = 0 // all targets succeeded
	ExitCommandFailed      = 1 // one or more targets failed
	ExitConfigError        = 2 // selection or configuration failure
	ExitApprovalRequired   = 3 // approval missing, invalid, or rejected
	ExitStoppedByThreshold = 4 // run halted by MaxErrors / StopOnAccessDenied
)
