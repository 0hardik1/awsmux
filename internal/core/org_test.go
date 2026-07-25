package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
		"999988887777": "", // directly under the root
		"111122223333": "eng/prod",
		"222233334444": "eng/prod",
		"333344445555": "eng/prod/db", // nested one deeper
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
