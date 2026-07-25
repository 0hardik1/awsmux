# Org-Aware Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `awsmux` select fan-out targets by AWS Organizations OU path and account tags, joining on the STS-verified account ID, and report org accounts that have no local profile as unreachable.

**Architecture:** A new `internal/core/org.go` enumerates the organization through the existing `aws` CLI seam and caches the tree in `$AWSMUX_HOME/org-cache.json`. `ResolveTargets` gains one filter stage that runs *after* preflight, so it joins on the account ID STS verified rather than on a profile name. Execution is untouched: still one `aws --profile <p>` child per target.

**Tech Stack:** Go 1.26, stdlib plus cobra only. Tests are plain stdlib `testing`, driven through the `AWSMUX_AWS_BIN` stub-binary seam, needing no network and no Docker.

## Amendments made during execution

Review caught defects in this plan's own code. The committed implementation is
authoritative where the two disagree; the full rulings live in the execution
ledger at `.superpowers/sdd/2026-07-25-org-aware-discovery/progress.md`.

- **Task 1, `matchTags`:** amended inline below. A plain map lookup let an
  untagged account match an empty-valued tag filter. Uses a comma-ok lookup.
- **Task 3, org cache:** amended inline below. The cache was a single unkeyed
  blob, so switching organizations inside the TTL served the wrong tree. `Org`
  now carries a collision-proof `Source`.
- **Task 4, `filterByOrg`:** **not amended below, read this before reusing that
  code.** Two defects were found and fixed in the implementation. First, the
  loop dropped targets whose preflight failed, which removed them before
  `CheckVerified` could block and turned a hard identity failure into a quietly
  narrower fan-out; errored targets must be carried through untouched. Second,
  the zero-match check keyed on `len(kept)`, which those carried-through targets
  inflate, so the coverage-explaining error could never fire once any preflight
  failed; the decision must use a counter of targets that actually matched, and
  the message must mention any targets it could not evaluate.

## Global Constraints

- **Dependencies: stdlib plus cobra, nothing else.** Do not add a module dependency for INI, JSON-RPC, AWS SDK, or test assertions. This is a design rule, not an accident.
- **Layering:** all policy, classification, and execution logic lives in `internal/core`. `cmd/` and `internal/mcpserver/` stay thin wrappers.
- **AGENT CONTRACT comments** at the top of `discovery.go`, `identity.go`, and `policy.go` pin exported signatures. Do not change a pinned signature; add new exported functions alongside.
- **No em dashes** anywhere: code comments, docs, commit messages. Use a colon, comma, parentheses, or two sentences.
- **Conventional Commits** are enforced by a commit-msg hook and CI. Allowed types: feat fix docs chore ci build refactor test perf style revert.
- **Never push to `main`.** All work lands on branch `feat/org-aware-discovery` and merges via PR.
- **Verify with** `go build ./... && go vet ./... && go test ./...` before every commit. The pre-commit hook also runs gofmt and golangci-lint.
- **Spec:** `docs/superpowers/specs/2026-07-25-org-aware-discovery-design.md`. Read it before starting.

---

### Task 1: OU path matching and account filtering (pure functions)

Establishes the data model and the matching rule with zero I/O, so the semantics are locked down and tested before any AWS calls exist.

**Files:**
- Create: `internal/core/org.go`
- Create: `internal/core/org_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `OrgAccount` struct (fields `ID, Name, Status, OUPath string`, `Tags map[string]string`), `Org` struct (fields `MasterAccountID string`, `Accounts map[string]OrgAccount`, `TagsFetched bool`, `FetchedAt time.Time`), `const OrgCacheTTL = 1 * time.Hour`, `func MatchOUPath(pattern, ouPath string) bool`, `func MatchAccount(acct OrgAccount, ouPatterns []string, tags map[string]string) bool`.

- [ ] **Step 1: Write the failing test**

Create `internal/core/org_test.go`:

```go
package core

import "testing"

func TestMatchOUPath(t *testing.T) {
	tests := []struct {
		pattern string
		ouPath  string
		want    bool
	}{
		// Exact match.
		{"eng/prod", "eng/prod", true},
		// Recursive: a child OU is inside its parent.
		{"eng/prod", "eng/prod/db", true},
		{"eng/prod", "eng/prod/db/replicas", true},
		// Siblings and unrelated trees do not match.
		{"eng/prod", "eng/dev", false},
		{"eng/prod", "platform/prod", false},
		// A longer pattern than the path cannot match.
		{"eng/prod/db", "eng/prod", false},
		// Globs apply within a segment.
		{"eng/*", "eng/prod", true},
		{"eng/*", "eng/prod/db", true},
		{"eng/*", "platform/prod", false},
		{"*", "eng", true},
		{"*", "eng/prod", true},
		// Partial-segment globs, as in MatchGlob.
		{"eng/pr??", "eng/prod", true},
		{"eng/pr??", "eng/production", false},
		// Root-level accounts have no OU path, so no pattern matches them.
		{"*", "", false},
		{"eng", "", false},
		// Surrounding and repeated slashes are tolerated on both sides.
		{"/eng/prod/", "eng/prod", true},
		{"eng//prod", "/eng/prod", true},
		// An empty pattern matches nothing (callers use an empty list for
		// "no filter", never an empty string).
		{"", "eng/prod", false},
		// Malformed globs never match, they do not error.
		{"eng/[", "eng/prod", false},
	}
	for _, tc := range tests {
		if got := MatchOUPath(tc.pattern, tc.ouPath); got != tc.want {
			t.Errorf("MatchOUPath(%q, %q) = %v, want %v", tc.pattern, tc.ouPath, got, tc.want)
		}
	}
}

func TestMatchAccount(t *testing.T) {
	acct := OrgAccount{
		ID:     "111122223333",
		Name:   "prod-web",
		Status: "ACTIVE",
		OUPath: "eng/prod",
		Tags:   map[string]string{"env": "prod", "tier": "critical"},
	}

	tests := []struct {
		name string
		ous  []string
		tags map[string]string
		want bool
	}{
		{"no filters matches everything", nil, nil, true},
		{"ou hit", []string{"eng/prod"}, nil, true},
		{"ou miss", []string{"eng/dev"}, nil, false},
		{"any ou pattern may match", []string{"eng/dev", "eng/prod"}, nil, true},
		{"tag hit", nil, map[string]string{"env": "prod"}, true},
		{"tag value mismatch", nil, map[string]string{"env": "dev"}, false},
		{"absent tag key", nil, map[string]string{"owner": "team"}, false},
		{"every tag must match", nil, map[string]string{"env": "prod", "tier": "spot"}, false},
		{"all tags match", nil, map[string]string{"env": "prod", "tier": "critical"}, true},
		{"ou and tag are ANDed", []string{"eng/dev"}, map[string]string{"env": "prod"}, false},
		{"both satisfied", []string{"eng/*"}, map[string]string{"env": "prod"}, true},
	}
	for _, tc := range tests {
		if got := MatchAccount(acct, tc.ous, tc.tags); got != tc.want {
			t.Errorf("%s: MatchAccount = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMatchAccountNoTagsOnAccount(t *testing.T) {
	// An account whose tags were never fetched must not satisfy a tag filter.
	acct := OrgAccount{ID: "111122223333", OUPath: "eng/prod"}
	if MatchAccount(acct, nil, map[string]string{"env": "prod"}) {
		t.Error("account with no tags satisfied a tag filter; must fail closed")
	}
	// A filter requesting an empty value must not match a missing key: a nil
	// map returns "" for every lookup, so a single-value lookup would match.
	if MatchAccount(acct, nil, map[string]string{"env": ""}) {
		t.Error("account with no tags matched an empty-valued tag filter; must fail closed")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/core/ -run 'TestMatchOUPath|TestMatchAccount' -v`
Expected: FAIL to build, with `undefined: OrgAccount` and `undefined: MatchOUPath`.

- [ ] **Step 3: Write the implementation**

Create `internal/core/org.go`:

```go
package core

import (
	"path"
	"strings"
	"time"
)

// OrgCacheTTL bounds how long an enumerated organization tree is trusted.
// An hour, rather than IdentityCacheTTL's five minutes: org structure changes
// weekly at most, while agents poll list_aws_targets constantly and every
// refresh costs a full tree walk plus an optional assume-role.
const OrgCacheTTL = 1 * time.Hour

// OrgAccount is one account as AWS Organizations reports it. ID is the join
// key against a Target's STS-verified AccountID. Name is the Organizations
// account name and has nothing to do with any local profile name.
type OrgAccount struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // ACTIVE | SUSPENDED | PENDING_CLOSURE
	// OUPath is the slash-joined OU path below the root, "" for an account
	// sitting directly under the root.
	OUPath string            `json:"ou_path"`
	Tags   map[string]string `json:"tags,omitempty"`
}

// Org is an enumerated organization, indexed by account ID.
type Org struct {
	MasterAccountID string                `json:"master_account_id"`
	Accounts        map[string]OrgAccount `json:"accounts"`
	// TagsFetched records whether per-account tags were retrieved, so a tree
	// enumerated for an OU-only query is not reused to answer a tag filter.
	TagsFetched bool `json:"tags_fetched"`
	// Source is the profile and assumed role that produced this tree. A cache
	// entry written by one set of credentials must never answer a query made
	// with another: the account tree would belong to a different organization
	// entirely, so the filter would select against the wrong org.
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
}

// MatchOUPath reports whether an OU pattern matches an account's OU path. The
// pattern must match a prefix of the path segment by segment, with path.Match
// glob semantics inside each segment, so "eng/prod" matches both "eng/prod"
// and "eng/prod/db", and "eng/*" matches both of those plus "eng/dev".
//
// Matching is deliberately recursive: a child OU inherits its parent's SCPs
// and is genuinely inside it, so treating "--ou eng/prod" as exact-path-only
// would silently skip accounts in a sub-OU, and silently skipping accounts is
// the worse failure for a fleet tool.
//
// Handing the whole path to MatchGlob would not produce this: Go's path.Match
// never lets * cross a /, so "eng/*" would match "eng/prod" but not
// "eng/prod/db".
//
// An account directly under the root has an empty OU path and is matched by
// no pattern at all, including "*", because every pattern needs at least one
// segment. Select the management account by profile instead.
func MatchOUPath(pattern, ouPath string) bool {
	pat := splitOU(pattern)
	segs := splitOU(ouPath)
	if len(pat) == 0 || len(pat) > len(segs) {
		return false
	}
	for i, p := range pat {
		// Malformed patterns simply never match, as in MatchGlob.
		if ok, err := path.Match(p, segs[i]); err != nil || !ok {
			return false
		}
	}
	return true
}

// splitOU splits an OU path into non-empty segments, so leading, trailing,
// and repeated slashes are all tolerated.
func splitOU(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// matchOUAny reports whether any pattern matches. An empty pattern list means
// "no OU filter" and matches everything, mirroring MatchGlob's defaultMatch.
func matchOUAny(patterns []string, ouPath string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if MatchOUPath(p, ouPath) {
			return true
		}
	}
	return false
}

// matchTags reports whether the account carries every required tag pair. The
// key must be present and its value must match, so an account whose tags were
// never fetched never satisfies a tag filter. The comma-ok lookup matters: a
// nil map returns "" for every key, so a plain lookup would let an account
// with no tags match a filter requesting an empty value.
func matchTags(acct OrgAccount, want map[string]string) bool {
	for k, v := range want {
		if val, ok := acct.Tags[k]; !ok || val != v {
			return false
		}
	}
	return true
}

// MatchAccount reports whether an account satisfies both the OU patterns and
// the tag requirements. The two filters are ANDed; empty means unfiltered.
func MatchAccount(acct OrgAccount, ouPatterns []string, tags map[string]string) bool {
	return matchOUAny(ouPatterns, acct.OUPath) && matchTags(acct, tags)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/core/ -run 'TestMatchOUPath|TestMatchAccount' -v`
Expected: PASS, all three tests.

- [ ] **Step 5: Verify the whole build and suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/core/org.go internal/core/org_test.go
git commit -m "feat(core): add org account model and recursive OU path matching"
```

---

### Task 2: Enumerate the organization through the aws CLI seam

Adds the live enumeration: an optional assume-role for the enumeration credentials, then a recursive tree walk, then optional per-account tags. No caching yet.

**Files:**
- Modify: `internal/core/awscmd.go` (append `awsExecEnv`)
- Modify: `internal/core/org.go` (append `OrgOptions`, `orgClient`, `assumeRoleEnv`, `enumerateOrg`)
- Modify: `internal/core/org_test.go` (append enumeration tests)

**Interfaces:**
- Consumes: `Org`, `OrgAccount` from Task 1; `awsExec(ctx, argv ...string) *exec.Cmd` from `awscmd.go`.
- Produces: `type OrgOptions struct { Profile, AssumeRole string; Refresh, WantTags bool }`, `func enumerateOrg(ctx context.Context, opts OrgOptions) (*Org, error)`, `func awsExecEnv(ctx context.Context, extraEnv []string, argv ...string) *exec.Cmd`, `const orgTagConcurrency = 32`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/org_test.go`:

```go
// writeOrgStub installs a stand-in aws CLI that answers the organizations and
// sts calls enumeration makes, and appends every invocation to a log file so
// tests can assert on call counts and on which flags were passed.
//
// The org it describes:
//
//	root
//	  eng/
//	    prod/      -> 111122223333 (prod-web), 222233334444 (prod-api)
//	      db/      -> 333344445555 (prod-db)
//	    dev/       -> 444455556666 (dev-sandbox)
//	  999988887777 (management, directly under root)
func writeOrgStub(t *testing.T) (stubPath, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub")
	}
	dir := t.TempDir()
	stubPath = filepath.Join(dir, "aws")
	logPath = filepath.Join(dir, "calls.log")

	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
# Strip the global flags awsmux appends so positional matching is stable.
svc=$1
op=$2
case "$svc $op" in
  "sts assume-role")
    echo '{"Credentials":{"AccessKeyId":"ASIASTUB","SecretAccessKey":"stubsecret","SessionToken":"stubtoken"}}'
    ;;
  "organizations describe-organization")
    echo '{"Organization":{"MasterAccountId":"999988887777"}}'
    ;;
  "organizations list-roots")
    echo '{"Roots":[{"Id":"r-root"}]}'
    ;;
  "organizations list-accounts-for-parent")
    case "$*" in
      *r-root*)   echo '{"Accounts":[{"Id":"999988887777","Name":"management","Status":"ACTIVE"}]}' ;;
      *ou-eng-prod-db*) echo '{"Accounts":[{"Id":"333344445555","Name":"prod-db","Status":"ACTIVE"}]}' ;;
      *ou-eng-prod*) echo '{"Accounts":[{"Id":"111122223333","Name":"prod-web","Status":"ACTIVE"},{"Id":"222233334444","Name":"prod-api","Status":"SUSPENDED"}]}' ;;
      *ou-eng-dev*) echo '{"Accounts":[{"Id":"444455556666","Name":"dev-sandbox","Status":"ACTIVE"}]}' ;;
      *) echo '{"Accounts":[]}' ;;
    esac
    ;;
  "organizations list-organizational-units-for-parent")
    case "$*" in
      *r-root*)         echo '{"OrganizationalUnits":[{"Id":"ou-eng","Name":"eng"}]}' ;;
      *ou-eng-prod-db*) echo '{"OrganizationalUnits":[]}' ;;
      *ou-eng-prod*)    echo '{"OrganizationalUnits":[{"Id":"ou-eng-prod-db","Name":"db"}]}' ;;
      *ou-eng-dev*)     echo '{"OrganizationalUnits":[]}' ;;
      *ou-eng*)         echo '{"OrganizationalUnits":[{"Id":"ou-eng-prod","Name":"prod"},{"Id":"ou-eng-dev","Name":"dev"}]}' ;;
      *)                echo '{"OrganizationalUnits":[]}' ;;
    esac
    ;;
  "organizations list-tags-for-resource")
    case "$*" in
      *111122223333*) echo '{"Tags":[{"Key":"env","Value":"prod"}]}' ;;
      *) echo '{"Tags":[]}' ;;
    esac
    ;;
  *)
    echo "unexpected call: $*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return stubPath, logPath
}

func readCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestEnumerateOrgBuildsOUPaths(t *testing.T) {
	stub, _ := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)

	org, err := enumerateOrg(context.Background(), OrgOptions{})
	if err != nil {
		t.Fatalf("enumerateOrg: %v", err)
	}
	if org.MasterAccountID != "999988887777" {
		t.Errorf("MasterAccountID = %q, want 999988887777", org.MasterAccountID)
	}
	want := map[string]string{
		"999988887777": "",             // directly under the root
		"111122223333": "eng/prod",
		"222233334444": "eng/prod",
		"333344445555": "eng/prod/db",  // nested one deeper
		"444455556666": "eng/dev",
	}
	if len(org.Accounts) != len(want) {
		t.Fatalf("got %d accounts, want %d: %+v", len(org.Accounts), len(want), org.Accounts)
	}
	for id, wantPath := range want {
		got, ok := org.Accounts[id]
		if !ok {
			t.Errorf("account %s missing", id)
			continue
		}
		if got.OUPath != wantPath {
			t.Errorf("account %s OUPath = %q, want %q", id, got.OUPath, wantPath)
		}
	}
	if org.Accounts["222233334444"].Status != "SUSPENDED" {
		t.Error("status must be carried through verbatim, never used to filter")
	}
	if org.TagsFetched {
		t.Error("TagsFetched must be false when WantTags was not set")
	}
}

func TestEnumerateOrgSkipsTagsUnlessWanted(t *testing.T) {
	stub, logPath := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)

	if _, err := enumerateOrg(context.Background(), OrgOptions{}); err != nil {
		t.Fatalf("enumerateOrg: %v", err)
	}
	for _, c := range readCalls(t, logPath) {
		if strings.Contains(c, "list-tags-for-resource") {
			t.Fatalf("tags fetched without WantTags: %s", c)
		}
	}
}

func TestEnumerateOrgFetchesTagsWhenWanted(t *testing.T) {
	stub, _ := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)

	org, err := enumerateOrg(context.Background(), OrgOptions{WantTags: true})
	if err != nil {
		t.Fatalf("enumerateOrg: %v", err)
	}
	if !org.TagsFetched {
		t.Error("TagsFetched = false, want true")
	}
	if got := org.Accounts["111122223333"].Tags["env"]; got != "prod" {
		t.Errorf("tag env = %q, want prod", got)
	}
	if len(org.Accounts["444455556666"].Tags) != 0 {
		t.Error("untagged account should have no tags")
	}
}

func TestEnumerateOrgAssumesRoleAndDropsProfile(t *testing.T) {
	stub, logPath := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)

	_, err := enumerateOrg(context.Background(), OrgOptions{
		Profile:    "mgmt",
		AssumeRole: "arn:aws:iam::999988887777:role/OrgRead",
	})
	if err != nil {
		t.Fatalf("enumerateOrg: %v", err)
	}

	calls := readCalls(t, logPath)
	if len(calls) == 0 {
		t.Fatal("no calls logged")
	}
	if !strings.Contains(calls[0], "sts assume-role") ||
		!strings.Contains(calls[0], "arn:aws:iam::999988887777:role/OrgRead") {
		t.Fatalf("first call = %q, want the assume-role", calls[0])
	}
	if !strings.Contains(calls[0], "--profile mgmt") {
		t.Errorf("assume-role must run through the base profile: %q", calls[0])
	}
	// Every organizations call must run on the assumed credentials, so none
	// may carry --profile: a profile flag overrides the environment
	// credentials and would silently enumerate as the wrong principal.
	for _, c := range calls[1:] {
		if strings.Contains(c, "--profile") {
			t.Errorf("organizations call carried --profile under an assumed role: %q", c)
		}
	}
}

func TestEnumerateOrgUsesProfileWithoutAssumeRole(t *testing.T) {
	stub, logPath := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)

	if _, err := enumerateOrg(context.Background(), OrgOptions{Profile: "mgmt"}); err != nil {
		t.Fatalf("enumerateOrg: %v", err)
	}
	calls := readCalls(t, logPath)
	for _, c := range calls {
		if strings.Contains(c, "assume-role") {
			t.Fatalf("no role was requested, must not assume one: %q", c)
		}
		if !strings.Contains(c, "--profile mgmt") {
			t.Errorf("call missing --profile mgmt: %q", c)
		}
	}
}

func TestEnumerateOrgFailsClosedOnAPIError(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "aws")
	script := "#!/bin/sh\necho 'AccessDeniedException: not authorized to perform organizations:ListRoots' >&2\nexit 254\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(AWSBinEnv, stub)

	org, err := enumerateOrg(context.Background(), OrgOptions{})
	if err == nil {
		t.Fatal("expected an error when the organizations API fails")
	}
	if org != nil {
		t.Error("no partial org may be returned on failure")
	}
	if !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Errorf("error should surface the API message, got: %v", err)
	}
}
```

Add the imports this test file now needs at the top of `internal/core/org_test.go`:

```go
import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/core/ -run TestEnumerateOrg -v`
Expected: FAIL to build, with `undefined: enumerateOrg` and `undefined: OrgOptions`.

- [ ] **Step 3: Add the environment-carrying exec helper**

Append to `internal/core/awscmd.go`:

```go
// awsExecEnv is awsExec with extra "KEY=value" entries appended to the child's
// environment. Only org enumeration uses it, because that is the only path
// that may run under temporary credentials from an assumed role. Every other
// call site uses awsExec and inherits the process environment unchanged.
func awsExecEnv(ctx context.Context, extraEnv []string, argv ...string) *exec.Cmd {
	cmd := awsExec(ctx, argv...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	return cmd
}
```

- [ ] **Step 4: Implement enumeration**

Append to `internal/core/org.go`, and extend its import block to
`bytes`, `context`, `encoding/json`, `fmt`, `path`, `sort`, `strings`, `sync`, `time`:

```go
// orgTagConcurrency bounds concurrent list-tags-for-resource calls. Tags cost
// one API call per account, so a 500-account org makes 500 of them; this is
// sized like preflightConcurrency for the same reason.
const orgTagConcurrency = 32

// OrgOptions controls enumeration.
type OrgOptions struct {
	// Profile is the base profile the organizations calls run under. Empty
	// means no --profile flag at all, letting normal AWS resolution apply.
	Profile string
	// AssumeRole is a role ARN assumed purely to enumerate. Empty means the
	// calls run directly as Profile.
	AssumeRole string
	// Refresh bypasses the cached tree.
	Refresh bool
	// WantTags fetches per-account tags, one extra API call per account.
	WantTags bool
}

// orgClient issues aws organizations calls under one credential context.
type orgClient struct {
	// profile is passed as --profile; always empty when env is set, because
	// an explicit profile overrides environment credentials and would
	// silently enumerate as the wrong principal.
	profile string
	env     []string
}

// run executes one aws organizations call and decodes its JSON into v. The
// AWS CLI paginates automatically and emits a single merged document, so no
// NextToken handling is needed here.
func (c orgClient) run(ctx context.Context, v any, argv ...string) error {
	full := append([]string{"organizations"}, argv...)
	if c.profile != "" {
		full = append(full, "--profile", c.profile)
	}
	full = append(full, "--output", "json")

	cmd := awsExecEnv(ctx, c.env, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("aws organizations %s: %s", argv[0], msg)
	}
	if err := json.Unmarshal(stdout.Bytes(), v); err != nil {
		return fmt.Errorf("parse aws organizations %s output: %w", argv[0], err)
	}
	return nil
}

// assumeRoleEnv assumes roleARN through the base profile and returns the
// credential environment for the organizations calls that follow.
//
// This is the one place awsmux holds credential material. The rule in
// discovery.go says awsmux never resolves credentials because it always shells
// out to `aws --profile <name>`; enumeration is the bounded exception. These
// credentials stay in memory, reach only the organizations calls, are never
// written to the org cache or any other file, and never touch fan-out
// execution, which stays profile-based.
//
// Worth knowing: sts assume-role is ClassMutating in awsmux's own tables, so
// this performs internally an operation awsmux would gate if a user planned
// it. That is deliberate. This is machinery, not a user-submitted plan.
func assumeRoleEnv(ctx context.Context, profile, roleARN string) ([]string, error) {
	argv := []string{"sts", "assume-role",
		"--role-arn", roleARN,
		"--role-session-name", "awsmux-org-discovery",
		"--output", "json"}
	if profile != "" {
		argv = append(argv, "--profile", profile)
	}

	cmd := awsExec(ctx, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("assume %s for org discovery: %s", roleARN, msg)
	}

	var out struct {
		Credentials struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
			SessionToken    string `json:"SessionToken"`
		} `json:"Credentials"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse sts assume-role output: %w", err)
	}
	if out.Credentials.AccessKeyID == "" || out.Credentials.SecretAccessKey == "" {
		return nil, fmt.Errorf("assume %s for org discovery: response carried no credentials", roleARN)
	}
	return []string{
		"AWS_ACCESS_KEY_ID=" + out.Credentials.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + out.Credentials.SecretAccessKey,
		"AWS_SESSION_TOKEN=" + out.Credentials.SessionToken,
	}, nil
}

// enumerateOrg walks the organization live, with no cache involved. Any
// failure returns a nil Org and an error: a partially enumerated tree would
// under-report accounts, and callers treat a missing account as "not
// selected", so partial data would silently shrink a fan-out.
func enumerateOrg(ctx context.Context, opts OrgOptions) (*Org, error) {
	var c orgClient
	if opts.AssumeRole != "" {
		env, err := assumeRoleEnv(ctx, opts.Profile, opts.AssumeRole)
		if err != nil {
			return nil, err
		}
		c.env = env
	} else {
		c.profile = opts.Profile
	}

	var desc struct {
		Organization struct {
			MasterAccountID string `json:"MasterAccountId"`
		} `json:"Organization"`
	}
	if err := c.run(ctx, &desc, "describe-organization"); err != nil {
		return nil, err
	}

	var roots struct {
		Roots []struct {
			ID string `json:"Id"`
		} `json:"Roots"`
	}
	if err := c.run(ctx, &roots, "list-roots"); err != nil {
		return nil, err
	}

	org := &Org{
		MasterAccountID: desc.Organization.MasterAccountID,
		Accounts:        make(map[string]OrgAccount),
		FetchedAt:       time.Now().UTC(),
	}
	for _, r := range roots.Roots {
		if err := c.walk(ctx, r.ID, "", org); err != nil {
			return nil, err
		}
	}

	if opts.WantTags {
		if err := c.fetchTags(ctx, org); err != nil {
			return nil, err
		}
		org.TagsFetched = true
	}
	return org, nil
}

// walk records every account directly under parentID at ouPath, then recurses
// into each child OU.
func (c orgClient) walk(ctx context.Context, parentID, ouPath string, org *Org) error {
	var accts struct {
		Accounts []struct {
			ID     string `json:"Id"`
			Name   string `json:"Name"`
			Status string `json:"Status"`
		} `json:"Accounts"`
	}
	if err := c.run(ctx, &accts, "list-accounts-for-parent", "--parent-id", parentID); err != nil {
		return err
	}
	for _, a := range accts.Accounts {
		org.Accounts[a.ID] = OrgAccount{
			ID:     a.ID,
			Name:   a.Name,
			Status: a.Status,
			OUPath: ouPath,
		}
	}

	var ous struct {
		OrganizationalUnits []struct {
			ID   string `json:"Id"`
			Name string `json:"Name"`
		} `json:"OrganizationalUnits"`
	}
	if err := c.run(ctx, &ous, "list-organizational-units-for-parent", "--parent-id", parentID); err != nil {
		return err
	}
	for _, ou := range ous.OrganizationalUnits {
		child := ou.Name
		if ouPath != "" {
			child = ouPath + "/" + ou.Name
		}
		if err := c.walk(ctx, ou.ID, child, org); err != nil {
			return err
		}
	}
	return nil
}

// fetchTags fills Tags for every account, bounded by orgTagConcurrency. Any
// failure is returned: a tag filter evaluated against silently missing tags
// would match nothing, which looks like "no accounts qualify" rather than
// "the lookup broke".
func (c orgClient) fetchTags(ctx context.Context, org *Org) error {
	ids := make([]string, 0, len(org.Accounts))
	for id := range org.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic error reporting

	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	sem := make(chan struct{}, orgTagConcurrency)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var out struct {
				Tags []struct {
					Key   string `json:"Key"`
					Value string `json:"Value"`
				} `json:"Tags"`
			}
			err := c.run(ctx, &out, "list-tags-for-resource", "--resource-id", id)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			tags := make(map[string]string, len(out.Tags))
			for _, t := range out.Tags {
				tags[t.Key] = t.Value
			}
			a := org.Accounts[id]
			a.Tags = tags
			org.Accounts[id] = a
		}()
	}
	wg.Wait()
	return firstErr
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/core/ -run TestEnumerateOrg -v`
Expected: PASS, all six tests.

If `TestEnumerateOrgBuildsOUPaths` hangs or recurses without end, the stub's shell `case` ordering has been disturbed. Shell `case` takes the first matching branch, so every specific parent id (`ou-eng-prod-db`, `ou-eng-prod`, `ou-eng-dev`) must stay above the generic `*ou-eng*` branch. Otherwise `ou-eng-dev` falls into the `ou-eng` branch, which lists `ou-eng-dev` as its own child and the walk never terminates.

- [ ] **Step 6: Verify the whole build and suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add internal/core/awscmd.go internal/core/org.go internal/core/org_test.go
git commit -m "feat(core): enumerate AWS Organizations through the aws CLI seam"
```

---

### Task 3: Cache the enumerated tree

Wraps enumeration in a TTL cache under `$AWSMUX_HOME`, so agents polling `list_aws_targets` do not re-walk the org on every call.

**Files:**
- Modify: `internal/core/org.go` (append cache helpers and `LoadOrg`)
- Modify: `internal/core/org_test.go` (append cache tests)

**Interfaces:**
- Consumes: `enumerateOrg`, `OrgOptions`, `Org`, `OrgCacheTTL` from Tasks 1 and 2; `Dir() (string, error)` from `store.go`.
- Produces: `func LoadOrg(ctx context.Context, opts OrgOptions) (*Org, error)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/org_test.go`:

```go
func TestLoadOrgCachesBetweenCalls(t *testing.T) {
	stub, logPath := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)
	t.Setenv("AWSMUX_HOME", t.TempDir())

	if _, err := LoadOrg(context.Background(), OrgOptions{}); err != nil {
		t.Fatalf("first LoadOrg: %v", err)
	}
	first := len(readCalls(t, logPath))
	if first == 0 {
		t.Fatal("first call made no API calls")
	}

	if _, err := LoadOrg(context.Background(), OrgOptions{}); err != nil {
		t.Fatalf("second LoadOrg: %v", err)
	}
	if got := len(readCalls(t, logPath)); got != first {
		t.Errorf("second LoadOrg made %d extra calls, want 0 (cache hit)", got-first)
	}
}

func TestLoadOrgRefreshBypassesCache(t *testing.T) {
	stub, logPath := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)
	t.Setenv("AWSMUX_HOME", t.TempDir())

	if _, err := LoadOrg(context.Background(), OrgOptions{}); err != nil {
		t.Fatalf("first LoadOrg: %v", err)
	}
	first := len(readCalls(t, logPath))

	if _, err := LoadOrg(context.Background(), OrgOptions{Refresh: true}); err != nil {
		t.Fatalf("refresh LoadOrg: %v", err)
	}
	if got := len(readCalls(t, logPath)); got <= first {
		t.Error("Refresh did not re-enumerate")
	}
}

func TestLoadOrgIgnoresExpiredCache(t *testing.T) {
	stub, logPath := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)
	home := t.TempDir()
	t.Setenv("AWSMUX_HOME", home)

	if _, err := LoadOrg(context.Background(), OrgOptions{}); err != nil {
		t.Fatalf("first LoadOrg: %v", err)
	}
	first := len(readCalls(t, logPath))

	// Backdate the cache past its TTL.
	path := filepath.Join(home, "org-cache.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var cached Org
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	cached.FetchedAt = time.Now().UTC().Add(-2 * OrgCacheTTL)
	data, err = json.Marshal(&cached)
	if err != nil {
		t.Fatalf("encode cache: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	if _, err := LoadOrg(context.Background(), OrgOptions{}); err != nil {
		t.Fatalf("third LoadOrg: %v", err)
	}
	if got := len(readCalls(t, logPath)); got <= first {
		t.Error("expired cache was reused")
	}
}

func TestLoadOrgTaglessCacheCannotAnswerTagQuery(t *testing.T) {
	stub, logPath := writeOrgStub(t)
	t.Setenv(AWSBinEnv, stub)
	t.Setenv("AWSMUX_HOME", t.TempDir())

	// Populate the cache with an OU-only enumeration, which fetches no tags.
	if _, err := LoadOrg(context.Background(), OrgOptions{}); err != nil {
		t.Fatalf("first LoadOrg: %v", err)
	}
	first := len(readCalls(t, logPath))

	org, err := LoadOrg(context.Background(), OrgOptions{WantTags: true})
	if err != nil {
		t.Fatalf("tag LoadOrg: %v", err)
	}
	if len(readCalls(t, logPath)) <= first {
		t.Fatal("tagless cache was reused to answer a tag query")
	}
	if got := org.Accounts["111122223333"].Tags["env"]; got != "prod" {
		t.Errorf("tag env = %q, want prod", got)
	}
}

func TestLoadOrgDoesNotCacheFailures(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "aws")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho boom >&2\nexit 254\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(AWSBinEnv, stub)
	home := t.TempDir()
	t.Setenv("AWSMUX_HOME", home)

	if _, err := LoadOrg(context.Background(), OrgOptions{}); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(home, "org-cache.json")); err == nil {
		t.Error("a failed enumeration must not be cached")
	}
}
```

Extend the test file's import block with `encoding/json` and `time`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/core/ -run TestLoadOrg -v`
Expected: FAIL to build, with `undefined: LoadOrg`.

- [ ] **Step 3: Implement the cache**

Append to `internal/core/org.go`, and extend its import block with `os` and `path/filepath`:

```go
// sourceKey returns a collision-proof key identifying the credentials this
// enumeration will use. Caches are scoped by source, so the same org is never
// returned for different credentials. A plain separator is not enough because
// profile names come from the shared-config INI parser, which permits
// arbitrary characters including pipe; the length prefix removes the
// ambiguity, since Profile is recovered by a fixed-width slice rather than by
// searching for a delimiter.
func (o OrgOptions) sourceKey() string {
	return fmt.Sprintf("%d:%s|%s", len(o.Profile), o.Profile, o.AssumeRole)
}

func orgCachePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "org-cache.json"), nil
}

// loadOrgCache reads the cached organization tree. Any problem (missing file,
// corrupt JSON, empty account map) yields nil, so the caller re-enumerates.
// Caching is best effort and must never fail a resolution.
func loadOrgCache() *Org {
	path, err := orgCachePath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var o Org
	if json.Unmarshal(data, &o) != nil || len(o.Accounts) == 0 {
		return nil
	}
	return &o
}

// saveOrgCache writes the tree atomically (temp file in the same directory,
// then rename) so concurrent readers never see partial JSON. All errors are
// swallowed; caching is best effort. The file holds account IDs, names, and
// tags, never credentials.
func saveOrgCache(o *Org) {
	path, err := orgCachePath()
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "org-cache-*.json")
	if err != nil {
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return
	}
	_ = os.Chmod(tmp.Name(), 0o600)
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
	}
}

// LoadOrg returns the organization tree, from cache when one is fresh enough
// and complete enough for the query, otherwise by enumerating live and
// caching the result. Only successful enumerations are cached, so a transient
// API failure is retried on the next call rather than remembered.
//
// A tree cached by an OU-only query carries no tags, so it is not reused to
// answer a tag filter; that check is what TagsFetched exists for. A tree
// cached under different credentials is likewise not reused, since it belongs
// to a different organization; that check is what Source exists for. Both
// mismatches behave like staleness: re-enumerate and overwrite.
func LoadOrg(ctx context.Context, opts OrgOptions) (*Org, error) {
	if !opts.Refresh {
		if o := loadOrgCache(); o != nil &&
			o.Source == opts.sourceKey() &&
			time.Since(o.FetchedAt) < OrgCacheTTL &&
			(!opts.WantTags || o.TagsFetched) {
			return o, nil
		}
	}
	o, err := enumerateOrg(ctx, opts)
	if err != nil {
		return nil, err
	}
	saveOrgCache(o)
	return o, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/core/ -run TestLoadOrg -v`
Expected: PASS, all five tests.

- [ ] **Step 5: Verify the whole build and suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/core/org.go internal/core/org_test.go
git commit -m "feat(core): cache the enumerated organization tree under AWSMUX_HOME"
```

---

### Task 4: Filter targets by org, after preflight

Wires org data into resolution. This is where the safety property lives: the filter joins on the STS-verified account ID, and a failed enumeration is an error rather than an unfiltered fan-out.

**Files:**
- Modify: `internal/core/types.go:78-90` (Selector), `internal/core/types.go:36-58` (Target)
- Modify: `internal/core/discovery.go:120-168` (ResolveTargets)
- Modify: `internal/core/discovery_test.go` (append filter tests)

**Interfaces:**
- Consumes: `LoadOrg`, `MatchAccount`, `OrgAccount`, `Org`, `OrgOptions` from Tasks 1 to 3.
- Produces: `type OrgSelection struct { Org *Org; Matched []string; Unreachable []OrgAccount }`, `func ResolveTargetsWithOrg(ctx context.Context, sel Selector) ([]Target, *OrgSelection, error)`, `func (s Selector) UsesOrg() bool`, and `Selector` fields `OU []string`, `AccountTags map[string]string`, `OrgRole string`, `OrgProfile string`, `OrgRefresh bool`, and `Target` fields `OrgAccountName string`, `OUPath string`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/discovery_test.go`:

```go
// orgFleetStub installs a stand-in aws CLI that answers both sts
// get-caller-identity (per profile, so each profile resolves to a distinct
// account) and the organizations tree, letting one test exercise the whole
// resolve path.
//
//	eng/prod -> 111122223333, 222233334444
//	eng/dev  -> 444455556666
//
// Local profiles: "alpha" -> 111122223333, "beta" -> 444455556666.
// Account 222233334444 is in the org but has no local profile.
func orgFleetStub(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub")
	}
	stub := filepath.Join(t.TempDir(), "aws")
	script := `#!/bin/sh
svc=$1
op=$2
case "$svc $op" in
  "sts get-caller-identity")
    case "$*" in
      *"--profile alpha"*) echo '{"UserId":"AIDAA","Account":"111122223333","Arn":"arn:aws:iam::111122223333:user/alpha"}' ;;
      *"--profile beta"*)  echo '{"UserId":"AIDAB","Account":"444455556666","Arn":"arn:aws:iam::444455556666:user/beta"}' ;;
      *) echo '{"UserId":"AIDAZ","Account":"999988887777","Arn":"arn:aws:iam::999988887777:user/other"}' ;;
    esac
    ;;
  "organizations describe-organization")
    echo '{"Organization":{"MasterAccountId":"999988887777"}}' ;;
  "organizations list-roots")
    echo '{"Roots":[{"Id":"r-root"}]}' ;;
  "organizations list-accounts-for-parent")
    case "$*" in
      *ou-prod*) echo '{"Accounts":[{"Id":"111122223333","Name":"prod-web","Status":"ACTIVE"},{"Id":"222233334444","Name":"prod-api","Status":"ACTIVE"}]}' ;;
      *ou-dev*)  echo '{"Accounts":[{"Id":"444455556666","Name":"dev-sandbox","Status":"ACTIVE"}]}' ;;
      *)         echo '{"Accounts":[]}' ;;
    esac
    ;;
  "organizations list-organizational-units-for-parent")
    case "$*" in
      *r-root*) echo '{"OrganizationalUnits":[{"Id":"ou-eng","Name":"eng"}]}' ;;
      *ou-eng*) echo '{"OrganizationalUnits":[{"Id":"ou-prod","Name":"prod"},{"Id":"ou-dev","Name":"dev"}]}' ;;
      *)        echo '{"OrganizationalUnits":[]}' ;;
    esac
    ;;
  "organizations list-tags-for-resource")
    case "$*" in
      *111122223333*) echo '{"Tags":[{"Key":"env","Value":"prod"}]}' ;;
      *)              echo '{"Tags":[]}' ;;
    esac
    ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return stub
}

// setupOrgFleet points discovery at two local profiles and the stub CLI.
func setupOrgFleet(t *testing.T) {
	t.Helper()
	isolateSharedFiles(t)
	cfg := writeSharedFile(t, "config", `[profile alpha]
region = us-east-1

[profile beta]
region = us-east-1
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWSMUX_HOME", t.TempDir())
	t.Setenv(AWSBinEnv, orgFleetStub(t))
}

func TestResolveTargetsFiltersByOU(t *testing.T) {
	setupOrgFleet(t)

	targets, orgSel, err := ResolveTargetsWithOrg(context.Background(), Selector{
		OU: []string{"eng/prod"},
	})
	if err != nil {
		t.Fatalf("ResolveTargetsWithOrg: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(targets), targets)
	}
	if targets[0].Profile != "alpha" {
		t.Errorf("profile = %q, want alpha", targets[0].Profile)
	}
	if targets[0].OUPath != "eng/prod" {
		t.Errorf("OUPath = %q, want eng/prod", targets[0].OUPath)
	}
	if targets[0].OrgAccountName != "prod-web" {
		t.Errorf("OrgAccountName = %q, want prod-web", targets[0].OrgAccountName)
	}
	if orgSel == nil {
		t.Fatal("OrgSelection must be non-nil when an org selector was used")
	}
	if len(orgSel.Unreachable) != 1 || orgSel.Unreachable[0].ID != "222233334444" {
		t.Errorf("Unreachable = %+v, want just 222233334444", orgSel.Unreachable)
	}
}

func TestResolveTargetsOUJoinsOnVerifiedAccountNotProfileName(t *testing.T) {
	// "beta" resolves to the dev account. A profile name suggesting prod must
	// never override what STS verified.
	isolateSharedFiles(t)
	cfg := writeSharedFile(t, "config", `[profile beta]
region = us-east-1
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWSMUX_HOME", t.TempDir())
	t.Setenv(AWSBinEnv, orgFleetStub(t))

	targets, _, err := ResolveTargetsWithOrg(context.Background(), Selector{
		OU: []string{"eng/dev"},
	})
	if err != nil {
		t.Fatalf("ResolveTargetsWithOrg: %v", err)
	}
	if len(targets) != 1 || targets[0].AccountID != "444455556666" {
		t.Fatalf("got %+v, want the dev account only", targets)
	}
}

func TestResolveTargetsOUIsRecursive(t *testing.T) {
	setupOrgFleet(t)

	targets, _, err := ResolveTargetsWithOrg(context.Background(), Selector{
		OU: []string{"eng"},
	})
	if err != nil {
		t.Fatalf("ResolveTargetsWithOrg: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want both eng/prod and eng/dev: %+v", len(targets), targets)
	}
}

func TestResolveTargetsFiltersByAccountTag(t *testing.T) {
	setupOrgFleet(t)

	targets, _, err := ResolveTargetsWithOrg(context.Background(), Selector{
		AccountTags: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("ResolveTargetsWithOrg: %v", err)
	}
	if len(targets) != 1 || targets[0].AccountID != "111122223333" {
		t.Fatalf("got %+v, want only the env=prod account", targets)
	}
}

func TestResolveTargetsOrgSelectorForcesPreflight(t *testing.T) {
	setupOrgFleet(t)

	// Preflight false in the selector must still resolve identities, because
	// the org filter has nothing to join on otherwise.
	targets, _, err := ResolveTargetsWithOrg(context.Background(), Selector{
		OU:        []string{"eng/prod"},
		Preflight: false,
	})
	if err != nil {
		t.Fatalf("ResolveTargetsWithOrg: %v", err)
	}
	if len(targets) != 1 || targets[0].AccountID == "" {
		t.Fatalf("org selector did not force preflight: %+v", targets)
	}
}

func TestResolveTargetsOrgFailureIsFatal(t *testing.T) {
	isolateSharedFiles(t)
	cfg := writeSharedFile(t, "config", `[profile alpha]
region = us-east-1
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWSMUX_HOME", t.TempDir())

	// A stub that answers STS but fails every organizations call.
	stub := filepath.Join(t.TempDir(), "aws")
	script := `#!/bin/sh
case "$1 $2" in
  "sts get-caller-identity") echo '{"UserId":"AIDAA","Account":"111122223333","Arn":"arn:aws:iam::111122223333:user/alpha"}' ;;
  *) echo 'AccessDeniedException: organizations:DescribeOrganization' >&2; exit 254 ;;
esac
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(AWSBinEnv, stub)

	targets, _, err := ResolveTargetsWithOrg(context.Background(), Selector{OU: []string{"eng/prod"}})
	if err == nil {
		t.Fatal("org enumeration failure must be fatal, never an unfiltered fan-out")
	}
	if targets != nil {
		t.Errorf("no targets may be returned on org failure, got %d", len(targets))
	}
	if !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Errorf("error should surface the API message, got: %v", err)
	}
}

func TestResolveTargetsOrgZeroMatchExplainsCoverage(t *testing.T) {
	setupOrgFleet(t)

	// eng/prod holds two accounts; drop the one profile that reaches it so
	// the message has to distinguish "empty OU" from "no local profile".
	_, _, err := ResolveTargetsWithOrg(context.Background(), Selector{
		Profiles: []string{"beta"},
		OU:       []string{"eng/prod"},
	})
	if err == nil {
		t.Fatal("expected an error when nothing matched")
	}
	msg := err.Error()
	for _, want := range []string{"eng/prod", "2 accounts matched", "0 with a local profile", "show-unreachable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

func TestResolveTargetsNoOrgSelectorSkipsOrgEntirely(t *testing.T) {
	isolateSharedFiles(t)
	cfg := writeSharedFile(t, "config", `[profile alpha]
region = us-east-1
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWSMUX_HOME", t.TempDir())

	// Fails on any organizations call: plain resolution must never make one.
	stub := filepath.Join(t.TempDir(), "aws")
	script := `#!/bin/sh
case "$1" in
  sts) echo '{"UserId":"AIDAA","Account":"111122223333","Arn":"arn:aws:iam::111122223333:user/alpha"}' ;;
  *) echo "organizations called without an org selector" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(AWSBinEnv, stub)

	targets, orgSel, err := ResolveTargetsWithOrg(context.Background(), Selector{Preflight: true})
	if err != nil {
		t.Fatalf("ResolveTargetsWithOrg: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	if orgSel != nil {
		t.Error("OrgSelection must be nil when no org selector was used")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/core/ -run 'TestResolveTargets(FiltersBy|OUJoins|OUIsRecursive|OrgSelector|OrgFailure|OrgZero|NoOrgSelector)' -v`
Expected: FAIL to build, with `undefined: ResolveTargetsWithOrg` and unknown fields `OU` and `AccountTags` in `Selector`.

- [ ] **Step 3: Add the Selector and Target fields**

In `internal/core/types.go`, extend `Selector` (currently ending at line 90) with:

```go
	// OU selects accounts by AWS Organizations OU path glob. A pattern
	// matches a prefix of the path segment by segment, so "eng/prod" also
	// selects "eng/prod/db". Implies Preflight, because the filter joins on
	// the STS-verified account ID.
	OU []string `json:"ou,omitempty"`
	// AccountTags selects accounts carrying every one of these org tag
	// pairs. Implies Preflight, for the same reason as OU.
	AccountTags map[string]string `json:"account_tags,omitempty"`
	// OrgRole is a role ARN assumed solely to enumerate the organization.
	// It never affects how targets execute.
	OrgRole string `json:"org_role,omitempty"`
	// OrgProfile is the base profile the organizations calls run under.
	OrgProfile string `json:"org_profile,omitempty"`
	// OrgRefresh bypasses the cached organization tree.
	OrgRefresh bool `json:"org_refresh,omitempty"`
```

And extend `Target` (currently ending at line 58) with:

```go
	// OrgAccountName and OUPath are filled when an org selector ran. They
	// are Organizations metadata, never a substitute for the verified
	// AccountID.
	OrgAccountName string `json:"org_account_name,omitempty"`
	OUPath         string `json:"ou_path,omitempty"`
```

- [ ] **Step 4: Implement the filter stage**

In `internal/core/discovery.go`, replace the body of `ResolveTargets` (lines 120 to 168) with a wrapper plus the new function, and add `sort` to the import block:

```go
// OrgSelection reports what AWS Organizations contributed to a resolution.
// It is nil when the selector used no org filter.
type OrgSelection struct {
	// Org is the enumerated tree the filter ran against.
	Org *Org
	// Matched holds every account ID satisfying the OU and tag filters,
	// sorted, whether or not a local profile reaches it.
	Matched []string
	// Unreachable holds the matched accounts that no local profile reaches.
	Unreachable []OrgAccount
}

// UsesOrg reports whether the selector needs AWS Organizations data.
func (s Selector) UsesOrg() bool {
	return len(s.OU) > 0 || len(s.AccountTags) > 0
}

// ResolveTargets discovers, filters, and (when asked) verifies targets.
// It is ResolveTargetsWithOrg without the org coverage report.
func ResolveTargets(ctx context.Context, sel Selector) ([]Target, error) {
	targets, _, err := ResolveTargetsWithOrg(ctx, sel)
	return targets, err
}

// ResolveTargetsWithOrg is ResolveTargets plus the OrgSelection describing
// what the org filter matched, including accounts that matched but have no
// local profile. The second return is nil when sel.UsesOrg() is false.
func ResolveTargetsWithOrg(ctx context.Context, sel Selector) ([]Target, *OrgSelection, error) {
	profiles, err := LoadProfiles()
	if err != nil {
		return nil, nil, fmt.Errorf("load profiles: %w", err)
	}

	var matched []Profile
	for _, p := range profiles {
		if MatchGlob(p.Name, sel.Profiles, true) && !MatchGlob(p.Name, sel.Exclude, false) {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		if len(profiles) == 0 {
			return nil, nil, noProfilesError()
		}
		return nil, nil, fmt.Errorf("no profiles matched selector %v", sel.Profiles)
	}

	var targets []Target
	for _, p := range matched {
		if len(sel.Regions) == 0 {
			t := NewTarget(p.Name, p.Region)
			t.Source = p.Source
			targets = append(targets, t)
			continue
		}
		for _, r := range sel.Regions {
			t := NewTarget(p.Name, r)
			t.Source = p.Source
			targets = append(targets, t)
		}
	}

	// An org filter joins on the STS-verified account ID, so it forces
	// preflight exactly the way Dedupe does.
	if sel.Preflight || sel.Dedupe || sel.UsesOrg() {
		targets = Preflight(ctx, targets)
		targets = MarkDuplicates(targets)
	}

	var orgSel *OrgSelection
	if sel.UsesOrg() {
		targets, orgSel, err = filterByOrg(ctx, sel, targets)
		if err != nil {
			return nil, nil, err
		}
	}

	if sel.Dedupe {
		kept := targets[:0]
		for _, t := range targets {
			if !t.Duplicate {
				kept = append(kept, t)
			}
		}
		targets = kept
	}
	return targets, orgSel, nil
}

// filterByOrg keeps only the targets whose verified account satisfies the OU
// and tag filters, annotating each with its org metadata, and reports which
// matching accounts no local profile reaches.
func filterByOrg(ctx context.Context, sel Selector, targets []Target) ([]Target, *OrgSelection, error) {
	org, err := LoadOrg(ctx, OrgOptions{
		Profile:    sel.OrgProfile,
		AssumeRole: sel.OrgRole,
		Refresh:    sel.OrgRefresh,
		WantTags:   len(sel.AccountTags) > 0,
	})
	if err != nil {
		// Fail closed. A selector that cannot be evaluated must never
		// degrade into "no filter", which would silently widen
		// "--ou eng/prod" into every profile on the machine.
		return nil, nil, fmt.Errorf("org discovery failed, refusing to run unfiltered: %w", err)
	}

	matchedSet := make(map[string]bool)
	var matchedIDs []string
	for _, a := range org.Accounts {
		if MatchAccount(a, sel.OU, sel.AccountTags) {
			matchedSet[a.ID] = true
			matchedIDs = append(matchedIDs, a.ID)
		}
	}
	sort.Strings(matchedIDs)

	reached := make(map[string]bool)
	kept := targets[:0]
	for _, t := range targets {
		if !matchedSet[t.AccountID] {
			continue
		}
		a := org.Accounts[t.AccountID]
		t.OUPath = a.OUPath
		t.OrgAccountName = a.Name
		reached[t.AccountID] = true
		kept = append(kept, t)
	}

	var unreachable []OrgAccount
	for _, id := range matchedIDs {
		if !reached[id] {
			unreachable = append(unreachable, org.Accounts[id])
		}
	}

	if len(kept) == 0 {
		return nil, nil, noOrgMatchError(sel, len(matchedIDs))
	}
	return kept, &OrgSelection{Org: org, Matched: matchedIDs, Unreachable: unreachable}, nil
}

// noOrgMatchError distinguishes the two situations that otherwise look
// identical: a filter that matched no account at all, and one that matched
// accounts none of your profiles can reach. That difference matters most
// immediately before a fan-out.
func noOrgMatchError(sel Selector, matchedAccounts int) error {
	var what []string
	if len(sel.OU) > 0 {
		what = append(what, "--ou "+strings.Join(sel.OU, ","))
	}
	for k, v := range sel.AccountTags {
		what = append(what, fmt.Sprintf("--account-tag %s=%s", k, v))
	}
	sort.Strings(what)
	desc := strings.Join(what, " ")

	if matchedAccounts == 0 {
		return fmt.Errorf("no targets matched %s\n  no org account matched the filter\n  hint: check the OU path with \"awsmux targets --ou '*'\"", desc)
	}
	return fmt.Errorf("no targets matched %s\n  %d accounts matched, 0 with a local profile\n  hint: run \"awsmux targets %s --show-unreachable\" to list them",
		desc, matchedAccounts, desc)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/core/ -run TestResolveTargets -v`
Expected: PASS, including the pre-existing `TestResolveTargetsNoProfiles`.

- [ ] **Step 6: Verify the whole build and suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass. `ResolveTargets` kept its pinned signature, so `cmd/` and `internal/mcpserver/` still compile untouched.

- [ ] **Step 7: Commit**

```bash
git add internal/core/types.go internal/core/discovery.go internal/core/discovery_test.go
git commit -m "feat(core): filter targets by OU path and account tags after preflight"
```

---

### Task 5: Cover the new Target fields in the plan hash

The org metadata is shown to a human at approval time, so it must be inside the integrity check. Includes the AGENTS.md safety-model update, because that document is the spec for these invariants and a reviewer reading this change needs it in the same diff.

**Files:**
- Modify: `internal/core/plan.go:63-80` (`ComputeHash` targetKey)
- Modify: `internal/core/policy.go:15-18` (PolicyVersion)
- Modify: `internal/core/plan_test.go` (append hash test)
- Modify: `AGENTS.md` (safety-model invariants 1 and 3)

**Interfaces:**
- Consumes: `Target.OUPath` and `Target.OrgAccountName` from Task 4.
- Produces: no new exported symbols. `PolicyVersion` becomes `"v3"`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/plan_test.go`:

```go
func TestComputeHashCoversOrgMetadata(t *testing.T) {
	base := func() *Plan {
		return &Plan{
			ID:        "plan-test",
			Service:   "ec2",
			Operation: "describe-instances",
			Targets: []Target{{
				ID:        "alpha@us-east-1",
				Profile:   "alpha",
				Region:    "us-east-1",
				AccountID: "111122223333",
				Principal: "arn:aws:iam::111122223333:user/alpha",
				OUPath:    "eng/prod",
			}},
			Classification: ClassReadOnly,
			PolicyVersion:  PolicyVersion,
			ExpiresAt:      time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		}
	}

	// OUPath and OrgAccountName are rendered in the plan a human approves, so
	// editing either after approval must break the hash.
	p := base()
	original := p.ComputeHash()

	moved := base()
	moved.Targets[0].OUPath = "eng/dev"
	if moved.ComputeHash() == original {
		t.Error("changing OUPath did not change the plan hash")
	}

	renamed := base()
	renamed.Targets[0].OrgAccountName = "prod-web"
	if renamed.ComputeHash() == original {
		t.Error("changing OrgAccountName did not change the plan hash")
	}

	// Same inputs must still hash identically.
	if base().ComputeHash() != original {
		t.Error("hash is not reproducible for identical plans")
	}
}

func TestPolicyVersionIsV3(t *testing.T) {
	// Bumped when the hash payload changed to cover org metadata. Plans
	// stored under an earlier version fail CheckApproval, which is the
	// intended fail-closed behavior.
	if PolicyVersion != "v3" {
		t.Errorf("PolicyVersion = %q, want v3", PolicyVersion)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/core/ -run 'TestComputeHashCoversOrgMetadata|TestPolicyVersionIsV3' -v`
Expected: FAIL. `TestComputeHashCoversOrgMetadata` reports "changing OUPath did not change the plan hash", and `TestPolicyVersionIsV3` reports `PolicyVersion = "v2"`.

- [ ] **Step 3: Add the fields to the hash payload**

In `internal/core/plan.go`, extend the `targetKey` struct inside `ComputeHash` and the value built from each target:

```go
		type targetKey struct {
			ID        string `json:"id"`
			Profile   string `json:"profile"`
			Region    string `json:"region"`
			AccountID string `json:"account_id"`
			Principal string `json:"principal"`
			// Org metadata is hashed because it is rendered in the plan a
			// human reads when approving. It does not influence
			// BuildCommand, so this is about honest presentation rather
			// than blast radius: without it, editing a stored plan's
			// OUPath would change what an approver sees without tripping
			// the mismatch check in CheckApproval.
			OUPath         string `json:"ou_path"`
			OrgAccountName string `json:"org_account_name"`
		}
```

and, in the loop that fills `keys`:

```go
			keys[i] = targetKey{
				ID:             t.ID,
				Profile:        t.Profile,
				Region:         t.Region,
				AccountID:      t.AccountID,
				Principal:      t.Principal,
				OUPath:         t.OUPath,
				OrgAccountName: t.OrgAccountName,
			}
```

Also update the `ComputeHash` doc comment (line 60) to list the new fields:

```go
// (ID, Profile, Region, AccountID, Principal, OUPath, OrgAccountName),
```

- [ ] **Step 4: Bump the policy version**

In `internal/core/policy.go`, replace the `PolicyVersion` block:

```go
// PolicyVersion is stamped into plans; bump when approval rules change.
// v2 reclassified s3 mv as destructive, s3 presign as mutating, and made
// s3 sync --delete escalate to destructive.
// v3 added the org metadata (OUPath, OrgAccountName) to the plan hash, so
// plans stored under v2 fail CheckApproval with a hash mismatch. That is
// intentional and fails closed; DefaultPlanTTL is one hour, so the window
// during which any plan is affected is narrow.
const PolicyVersion = "v3"
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/core/ -v`
Expected: PASS. Watch for any existing test that hardcodes `"v2"`; if one does, update it to `PolicyVersion` rather than to a new literal.

- [ ] **Step 6: Update the safety-model documentation**

In `AGENTS.md`, under "The safety model — invariants you must not weaken", amend invariant 3 to name the new hashed fields, and add a note to the section recording the enumeration exception. Insert after invariant 3's existing text:

```markdown
   Org metadata (`OUPath`, `OrgAccountName`) is hashed too, as of
   `PolicyVersion` v3: it does not influence `BuildCommand`, but it is
   rendered in the plan a human approves, and anything shown at approval
   time must be covered by the integrity check.
```

And add, after invariant 8:

```markdown
9. **awsmux resolves credentials in exactly one place.** Fan-out always
   shells out to `aws --profile <name>`, so the engine never holds
   credential material. The single exception is `assumeRoleEnv` in
   `internal/core/org.go`: when `--org-role` is set, awsmux assumes that
   role and passes the temporary credentials as environment to the AWS
   Organizations enumeration calls, and to nothing else. They are never
   persisted (not even to `org-cache.json`), never logged, and never reach
   execution. Keep that boundary; do not extend the exception to fan-out.
```

- [ ] **Step 7: Verify the whole build and suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add internal/core/plan.go internal/core/policy.go internal/core/plan_test.go AGENTS.md
git commit -m "feat(core): hash org metadata into plans and bump PolicyVersion to v3"
```

---

### Task 6: CLI flags, OU column, and the coverage summary

**Files:**
- Modify: `cmd/root.go:64-90` (selectorFlags, addSelectorFlags, selector)
- Modify: `cmd/targets.go` (validation and the unreachable summary)
- Modify: `internal/output/format.go:333-350` (renderTargetTable) and append `RenderUnreachable`
- Create: `internal/output/format_test.go`

**Interfaces:**
- Consumes: `core.OrgSelection`, `core.OrgAccount`, `core.ResolveTargetsWithOrg`, `Selector.UsesOrg` from Task 4.
- Produces: `func RenderUnreachable(w io.Writer, accts []core.OrgAccount, showAll bool) error` in `internal/output`, and `func (sf *selectorFlags) validate(cmd *cobra.Command) error` in `cmd`.

- [ ] **Step 1: Write the failing test**

Create `internal/output/format_test.go`:

```go
package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/0hardik1/awsmux/internal/core"
)

func TestRenderTargetTableOmitsOUColumnWithoutOrgData(t *testing.T) {
	var buf bytes.Buffer
	targets := []core.Target{{ID: "alpha", Profile: "alpha", AccountID: "111122223333"}}
	if err := RenderTargets(&buf, targets, "table"); err != nil {
		t.Fatalf("RenderTargets: %v", err)
	}
	if strings.Contains(buf.String(), "OU") {
		t.Errorf("OU column present with no org data:\n%s", buf.String())
	}
}

func TestRenderTargetTableShowsOUColumnWithOrgData(t *testing.T) {
	var buf bytes.Buffer
	targets := []core.Target{
		{ID: "alpha", Profile: "alpha", AccountID: "111122223333", OUPath: "eng/prod"},
		{ID: "beta", Profile: "beta", AccountID: "444455556666", OUPath: ""},
	}
	if err := RenderTargets(&buf, targets, "table"); err != nil {
		t.Fatalf("RenderTargets: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "OU") || !strings.Contains(out, "eng/prod") {
		t.Errorf("OU column missing:\n%s", out)
	}
	// A root-level account renders as a dash, never as blank.
	if !strings.Contains(out, "-") {
		t.Errorf("empty OU path should render as a dash:\n%s", out)
	}
}

func TestRenderUnreachableEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderUnreachable(&buf, nil, false); err != nil {
		t.Fatalf("RenderUnreachable: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for zero unreachable accounts, got:\n%s", buf.String())
	}
}

func TestRenderUnreachableTruncatesByDefault(t *testing.T) {
	accts := make([]core.OrgAccount, 12)
	for i := range accts {
		accts[i] = core.OrgAccount{ID: "10000000000" + string(rune('0'+i%10)), Name: "acct", OUPath: "eng/prod"}
	}
	var buf bytes.Buffer
	if err := RenderUnreachable(&buf, accts, false); err != nil {
		t.Fatalf("RenderUnreachable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "12 accounts") {
		t.Errorf("summary must state the full count:\n%s", out)
	}
	if !strings.Contains(out, "+9 more") {
		t.Errorf("summary must say how many were elided:\n%s", out)
	}
	if !strings.Contains(out, "--show-unreachable") {
		t.Errorf("summary must point at the flag that expands it:\n%s", out)
	}
}

func TestRenderUnreachableShowAllListsEvery(t *testing.T) {
	accts := []core.OrgAccount{
		{ID: "111122223333", Name: "prod-batch", OUPath: "eng/prod"},
		{ID: "222233334444", Name: "prod-ml", OUPath: "eng/prod"},
		{ID: "333344445555", Name: "prod-db", OUPath: "eng/prod/db"},
	}
	var buf bytes.Buffer
	if err := RenderUnreachable(&buf, accts, true); err != nil {
		t.Fatalf("RenderUnreachable: %v", err)
	}
	out := buf.String()
	for _, a := range accts {
		if !strings.Contains(out, a.ID) || !strings.Contains(out, a.Name) {
			t.Errorf("account %s missing from full listing:\n%s", a.ID, out)
		}
	}
	if strings.Contains(out, "more") {
		t.Errorf("full listing must not truncate:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/output/ -v`
Expected: FAIL to build, with `undefined: RenderUnreachable`.

- [ ] **Step 3: Implement the output changes**

In `internal/output/format.go`, replace `renderTargetTable` and append `RenderUnreachable`:

```go
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
```

Confirm `strings` is already imported in `format.go` (it is, for `shortPrincipal`).

- [ ] **Step 4: Run the output tests to verify they pass**

Run: `go test ./internal/output/ -v`
Expected: PASS, all five tests.

- [ ] **Step 5: Add the CLI flags**

In `cmd/root.go`, extend `selectorFlags` and `addSelectorFlags`, and add the validation helper:

```go
// selectorFlags are shared by every command that picks targets.
type selectorFlags struct {
	profiles        []string
	exclude         []string
	regions         []string
	preflight       bool
	dedupe          bool
	ou              []string
	accountTags     map[string]string
	orgRole         string
	orgProfile      string
	orgRefresh      bool
	showUnreachable bool
}

func addSelectorFlags(cmd *cobra.Command, sf *selectorFlags) {
	f := cmd.Flags()
	f.StringSliceVar(&sf.profiles, "profiles", nil, "profile glob(s), e.g. 'prod-*' (default: all profiles)")
	f.StringSliceVar(&sf.exclude, "exclude", nil, "profile glob(s) to exclude, e.g. '*-prod-*'")
	f.StringSliceVar(&sf.regions, "regions", nil, "region(s), one target per profile x region (default: profile default region)")
	f.BoolVar(&sf.preflight, "preflight", true, "verify each target identity via sts get-caller-identity")
	f.BoolVar(&sf.dedupe, "dedupe", false, "drop targets that resolve to a duplicate account+principal+region")
	f.StringSliceVar(&sf.ou, "ou", nil, "AWS Organizations OU path glob(s), e.g. 'eng/prod' (matches nested OUs too)")
	f.StringToStringVar(&sf.accountTags, "account-tag", nil, "org account tag filter(s), e.g. env=prod (every pair must match)")
	f.StringVar(&sf.orgRole, "org-role", "", "role ARN assumed to enumerate AWS Organizations (enumeration only, never execution)")
	f.StringVar(&sf.orgProfile, "org-profile", "", "profile for the organizations calls (default: normal AWS resolution)")
	f.BoolVar(&sf.orgRefresh, "org-refresh", false, "bypass the cached organization tree")
	f.BoolVar(&sf.showUnreachable, "show-unreachable", false, "list every matched org account that has no local profile")
}

func (sf *selectorFlags) selector() core.Selector {
	return core.Selector{
		Profiles:    sf.profiles,
		Exclude:     sf.exclude,
		Regions:     sf.regions,
		Preflight:   sf.preflight,
		Dedupe:      sf.dedupe,
		OU:          sf.ou,
		AccountTags: sf.accountTags,
		OrgRole:     sf.orgRole,
		OrgProfile:  sf.orgProfile,
		OrgRefresh:  sf.orgRefresh,
	}
}

// validate rejects flag combinations that cannot be honored. An org selector
// filters on the account ID STS verified, so it cannot run with preflight
// explicitly disabled; silently overriding the user would be worse than
// saying so.
func (sf *selectorFlags) validate(cmd *cobra.Command) error {
	if !sf.selector().UsesOrg() {
		return nil
	}
	if cmd.Flags().Changed("preflight") && !sf.preflight {
		return Exitf(core.ExitConfigError,
			"--ou and --account-tag filter on the account ID that STS verified, so they cannot run with --preflight=false")
	}
	return nil
}
```

- [ ] **Step 6: Wire the summary into `awsmux targets`**

In `cmd/targets.go`, replace `runTargets`:

```go
func runTargets(cmd *cobra.Command, args []string) error {
	if !output.ValidFormat(targetsFlags.format) {
		return Exitf(core.ExitConfigError, "invalid --format %q (want table, json, or jsonl)", targetsFlags.format)
	}
	if err := targetsFlags.sel.validate(cmd); err != nil {
		return err
	}
	sel := targetsFlags.sel.selector()
	targets, orgSel, err := core.ResolveTargetsWithOrg(cmd.Context(), sel)
	if err != nil {
		return Exitf(core.ExitConfigError, "%s", err)
	}
	if len(targets) == 0 {
		return Exitf(core.ExitConfigError, "no profiles matched the selection")
	}
	if err := output.RenderTargets(os.Stdout, targets, targetsFlags.format); err != nil {
		return fmt.Errorf("rendering targets: %w", err)
	}
	// Coverage goes to stderr so it never contaminates json or jsonl stdout,
	// which agents and CI parse.
	if orgSel != nil {
		if err := output.RenderUnreachable(os.Stderr, orgSel.Unreachable, targetsFlags.sel.showUnreachable); err != nil {
			return fmt.Errorf("rendering unreachable accounts: %w", err)
		}
	}
	if sel.Preflight || sel.Dedupe || sel.UsesOrg() {
		for _, t := range targets {
			if t.PreflightErr != "" {
				fmt.Fprintf(os.Stderr, "awsmux: preflight failed for %s: %s\n", t.ID, t.PreflightErr)
			}
		}
	}
	return nil
}
```

- [ ] **Step 7: Add the same validation to run and plan**

In `cmd/run.go` and `cmd/plan.go`, add the validation call immediately after the existing format or argument checks in each `RunE`, before the selector is used:

```go
	if err := <flagsVar>.sel.validate(cmd); err != nil {
		return err
	}
```

Replace `<flagsVar>` with the file's own flags variable (`runFlags` in `run.go`, `planFlags` in `plan.go`). Read each `RunE` first and insert the call before its `ResolveTargets` invocation.

- [ ] **Step 8: Verify the whole build and suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 9: Verify the CLI end to end against the stub**

The org path has no LocalStack coverage, so exercise it by hand with the same stub technique the tests use:

```bash
mkdir -p /tmp/awsmux-org-check && cd /tmp/awsmux-org-check
cat > aws <<'STUB'
#!/bin/sh
case "$1 $2" in
  "sts get-caller-identity") echo '{"UserId":"AIDAA","Account":"111122223333","Arn":"arn:aws:iam::111122223333:user/alpha"}' ;;
  "organizations describe-organization") echo '{"Organization":{"MasterAccountId":"999988887777"}}' ;;
  "organizations list-roots") echo '{"Roots":[{"Id":"r-root"}]}' ;;
  "organizations list-accounts-for-parent")
    case "$*" in
      *ou-prod*) echo '{"Accounts":[{"Id":"111122223333","Name":"prod-web","Status":"ACTIVE"},{"Id":"222233334444","Name":"prod-api","Status":"ACTIVE"}]}' ;;
      *) echo '{"Accounts":[]}' ;;
    esac ;;
  "organizations list-organizational-units-for-parent")
    case "$*" in
      *r-root*) echo '{"OrganizationalUnits":[{"Id":"ou-eng","Name":"eng"}]}' ;;
      *ou-eng*) echo '{"OrganizationalUnits":[{"Id":"ou-prod","Name":"prod"}]}' ;;
      *) echo '{"OrganizationalUnits":[]}' ;;
    esac ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
STUB
chmod +x aws
cat > config <<'CFG'
[profile alpha]
region = us-east-1
CFG

export AWSMUX_AWS_BIN=$PWD/aws AWS_CONFIG_FILE=$PWD/config AWSMUX_HOME=$PWD/home
cd - && make build

AWSMUX_AWS_BIN=/tmp/awsmux-org-check/aws AWS_CONFIG_FILE=/tmp/awsmux-org-check/config AWSMUX_HOME=/tmp/awsmux-org-check/home ./bin/awsmux targets --ou eng/prod
```

Expected: one row for `alpha` with an `OU` column reading `eng/prod`, and on stderr `1 accounts matched the selector but have no local profile:` naming `222233334444 (prod-api)`.

Then confirm the conflict check and the fail-closed path:

```bash
AWSMUX_AWS_BIN=/tmp/awsmux-org-check/aws AWS_CONFIG_FILE=/tmp/awsmux-org-check/config AWSMUX_HOME=/tmp/awsmux-org-check/home ./bin/awsmux targets --ou eng/prod --preflight=false; echo "exit=$?"
```

Expected: the `--preflight=false` error and `exit=2`.

- [ ] **Step 10: Commit**

```bash
git add cmd/root.go cmd/targets.go cmd/run.go cmd/plan.go internal/output/format.go internal/output/format_test.go
git commit -m "feat(cli): add --ou and --account-tag selectors with a coverage summary"
```

---

### Task 7: MCP tool schemas and compact org output

**Files:**
- Modify: `internal/mcpserver/tools.go` (schema helpers, both tool schemas, both decode structs and handlers)
- Modify: `internal/mcpserver/results.go:40-64` (compactTargets)
- Modify: `internal/mcpserver/tools_test.go` (append decode tests)

**Interfaces:**
- Consumes: `core.ResolveTargetsWithOrg`, `core.OrgSelection`, `core.Selector` org fields from Task 4.
- Produces: `func schemaStringMap(desc string) map[string]any`, and `compactTargets(targets []core.Target, orgSel *core.OrgSelection) map[string]any` (signature change, one extra parameter).

- [ ] **Step 1: Write the failing test**

Append to `internal/mcpserver/tools_test.go`:

```go
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
```

Ensure `internal/mcpserver/tools_test.go` imports `encoding/json`, `strings`, `testing`, and `github.com/0hardik1/awsmux/internal/core`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/mcpserver/ -v`
Expected: FAIL to build, with `too many arguments in call to compactTargets`.

- [ ] **Step 3: Add the map schema helper**

In `internal/mcpserver/tools.go`, append next to the other schema helpers (after `schemaInt` at line 174):

```go
func schemaStringMap(desc string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string"},
		"description":          desc,
	}
}
```

- [ ] **Step 4: Extend both tool schemas**

In the `list_aws_targets` schema properties map (around line 40), add:

```go
			"ou":           schemaStringArray("AWS Organizations OU path globs, e.g. [\"eng/prod\"]. Matches nested OUs too, so \"eng/prod\" also selects \"eng/prod/db\". Filters on the STS-verified account id, and forces preflight on."),
			"account_tags": schemaStringMap("Org account tag filter, e.g. {\"env\":\"prod\"}. Every pair must match. Forces preflight on."),
			"org_role":     schemaString("Role ARN assumed to enumerate AWS Organizations. Enumeration only; it never affects how targets execute."),
			"org_profile":  schemaString("Profile used for the organizations calls. Empty means normal AWS resolution."),
			"org_refresh":  schemaBool("Bypass the cached organization tree (cached for one hour)."),
```

Add the same five entries to the `plan_aws_operation` schema properties map (around line 68).

Also extend the `list_aws_targets` description string (line 35) with a sentence:

```
"Set ou or account_tags to select by AWS Organizations structure instead of profile name; the response then also reports matched org accounts that have no local profile."
```

- [ ] **Step 5: Extend both decode structs and handlers**

Replace `listTargetsTool` (line 195):

```go
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
```

In `planOperationTool` (line 219), add the same five fields to its decode struct and pass them into the `core.Selector` literal, leaving `Preflight: true` as it is.

- [ ] **Step 6: Extend compactTargets**

Replace `compactTargets` in `internal/mcpserver/results.go`:

```go
// unreachablePreviewIDs is how many unreachable account ids the summary names
// before eliding. The consumer is a model paying per token, so this stays a
// count plus a taste, not a roster.
const unreachablePreviewIDs = 5

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
		ids := make([]string, 0, unreachablePreviewIDs)
		for i, a := range orgSel.Unreachable {
			if i == unreachablePreviewIDs {
				break
			}
			ids = append(ids, a.ID+" ("+a.Name+")")
		}
		out["unreachable"] = map[string]any{
			"count":   len(orgSel.Unreachable),
			"sample":  ids,
			"meaning": "org accounts matching the selector that no local profile reaches; they were not targeted",
		}
	}
	return out
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/mcpserver/ -v`
Expected: PASS, all four new tests plus the existing ones.

- [ ] **Step 8: Verify the whole build and suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 9: Commit**

```bash
git add internal/mcpserver/tools.go internal/mcpserver/results.go internal/mcpserver/tools_test.go
git commit -m "feat(mcp): expose org selectors and unreachable-account coverage"
```

---

### Task 8: Documentation and LocalStack coverage check

**Files:**
- Modify: `README.md` (flag list, roadmap line)
- Modify: `docs/ARCHITECTURE.md` (engine layout, enumeration sequence, e2e coverage note)

**Interfaces:**
- Consumes: everything from Tasks 1 to 7.
- Produces: no code.

- [ ] **Step 1: Determine whether LocalStack community supports Organizations**

Run:

```bash
docker run --rm localstack/localstack:3.8 \
  sh -c 'cat /opt/code/localstack/localstack-supported-services.txt 2>/dev/null || true' | grep -i organizations
```

If that file is absent in the image, check the running fleet instead:

```bash
make fleet-up
source .tmp/fleet/env.sh
aws organizations describe-organization 2>&1 | head -5
make fleet-down
```

Record which of the two outcomes you observed. An `AccessDenied`, `InternalFailure`, or "API not implemented / not included in your license" response means the API is Pro-gated and `make e2e` cannot cover the org path.

- [ ] **Step 2: Update README**

Add the new flags to the selector flag table, and replace the roadmap sentence (currently at `README.md:269`):

```markdown
Dependencies: stdlib plus cobra. Roadmap: policy packs, Homebrew tap and
release binaries.
```

Then add a short subsection under the targets documentation:

```markdown
### Selecting by AWS Organizations

`--ou` and `--account-tag` select targets by org structure instead of profile
name. Both filter on the account id STS verified during preflight, never on a
profile name, and both match nested OUs: `--ou eng/prod` also selects
`eng/prod/db`.

```sh
awsmux targets --ou eng/prod
awsmux targets --account-tag env=prod --show-unreachable
awsmux run --ou eng/* -- ec2 describe-instances
```

awsmux enumerates the org through the AWS CLI and caches the tree for an hour
(`--org-refresh` to bypass). When your everyday credentials cannot call the
Organizations API, `--org-role <arn>` assumes a role for the enumeration only;
it never changes how targets execute, which is always
`aws --profile <name>`.

Org accounts matching the filter that no local profile reaches are reported as
unreachable rather than hidden, so `awsmux targets --ou eng/prod` tells you
both what you can reach and what you cannot. If enumeration fails, the command
errors out: an org selector never degrades into an unfiltered fan-out.
```

- [ ] **Step 3: Update docs/ARCHITECTURE.md**

Add `internal/core/org.go` to the engine file listing, describing it as org enumeration plus the TTL cache. Add a subsection recording:

- the resolution order (profiles, globs, regions, preflight, **org filter**, dedupe) and why the org filter sits after preflight;
- that `--org-role` is the single place awsmux holds credential material, bounded to enumeration, with `sts assume-role` being `ClassMutating` in awsmux's own tables;
- that `OUPath` and `OrgAccountName` are in the plan hash as of `PolicyVersion` v3, and that plans stored under v2 fail closed;
- the e2e coverage status you determined in Step 1. If Organizations is Pro-gated, state plainly that `make e2e` does not cover org discovery and that stub-binary unit tests in `internal/core/org_test.go` are the whole coverage story.

- [ ] **Step 4: Verify docs match the code**

Run:

```bash
go build ./... && ./bin/awsmux targets --help
```

Confirm every flag named in the README appears in the help output with the same name, and that no README example uses a flag that does not exist.

- [ ] **Step 5: Full verification**

Run: `go build ./... && go vet ./... && go test ./... && make lint && make check-fmt`
Expected: all pass.

- [ ] **Step 6: Commit and open the PR**

```bash
git add README.md docs/ARCHITECTURE.md
git commit -m "docs: document org-aware discovery selectors and safety boundary"
git push -u origin feat/org-aware-discovery
gh pr create --title "feat: org-aware target discovery via --ou and --account-tag" --body "$(cat <<'BODY'
Selects fan-out targets by AWS Organizations OU path and account tags,
joining on the STS-verified account id rather than the profile name.
Execution is unchanged: still one `aws --profile <p>` child per target.

## Safety model changes

Three deliberate amendments, per the approved spec:

1. `assumeRoleEnv` in `internal/core/org.go` holds credential material for
   the duration of enumeration when `--org-role` is set. Bounded to the
   organizations calls: never persisted, never logged, never near fan-out.
   This is the first exception to the rule that awsmux always shells out to
   `aws --profile <name>`, and it is documented in AGENTS.md.
2. `OUPath` and `OrgAccountName` are now covered by the plan hash. They do
   not influence `BuildCommand`, but they are rendered in the plan a human
   approves, so they belong inside the integrity check.
3. `PolicyVersion` v2 to v3. Plans stored under v2 fail `CheckApproval`
   with a hash mismatch, which fails closed. `DefaultPlanTTL` is one hour,
   so the affected window is narrow.

Unchanged: an org selector cannot widen blast radius. Every selected target
is still STS-verified, still hashed into the plan, still gated by
`RequiresApproval`. A failed enumeration is a hard `ExitConfigError` rather
than an unfiltered run.

## Spec

`docs/superpowers/specs/2026-07-25-org-aware-discovery-design.md`
BODY
)"
```

---

## Self-Review

**Spec coverage:** every section of the spec maps to a task. Data model and OU matching, Task 1. Enumeration and the assume-role boundary, Task 2. Cache with TTL and refresh, Task 3. Resolution flow, `ResolveTargetsWithOrg`, `OrgSelection`, fail-closed errors, and the zero-match message, Task 4. Plan hash and `PolicyVersion`, Task 5. CLI surface and the coverage summary, Task 6. MCP surface, Task 7. Docs and the LocalStack question, Task 8. The spec's "does not filter on SUSPENDED" rule is asserted in Task 2's `TestEnumerateOrgBuildsOUPaths`.

**Type consistency:** `OrgAccount`, `Org`, `OrgOptions`, `OrgSelection`, `LoadOrg`, `enumerateOrg`, `MatchOUPath`, `MatchAccount`, `UsesOrg`, `ResolveTargetsWithOrg`, `RenderUnreachable`, and `compactTargets(targets, orgSel)` are used with identical names and signatures across every task that references them. `compactTargets` is the one existing signature that changes, in Task 7, where both its definition and its single call site are updated together.

**Known deviation from the spec:** the spec listed `--show-unreachable` alongside the org selector flags; here it lives in `addSelectorFlags` with the others but is only consumed by `awsmux targets`, since `run` and `plan` print their own summaries. Behavior is identical; the flag is simply accepted everywhere and acted on in one place.
