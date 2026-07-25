package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolateSharedFiles points both shared-file env vars at missing paths so a
// developer's real ~/.aws files can never leak into a test. Tests that need
// one of the files overwrite the relevant var afterwards.
func isolateSharedFiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "no-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "no-credentials"))
}

func writeSharedFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		name         string
		patterns     []string
		defaultMatch bool
		want         bool
	}{
		{"prod-eu", nil, true, true},
		{"prod-eu", nil, false, false},
		{"prod-eu", []string{}, true, true},
		{"prod-eu", []string{"prod-*"}, false, true},
		{"prod-eu", []string{"staging-*"}, false, false},
		{"prod-eu", []string{"staging-*", "prod-*"}, false, true},
		{"prod-eu", []string{"prod-eu"}, false, true},
		{"prod-eu", []string{"prod-??"}, false, true},
		{"prod-eu", []string{"[x-"}, false, false}, // malformed pattern never matches
		{"prod-eu", []string{"*"}, false, true},
	}
	for _, tt := range tests {
		if got := MatchGlob(tt.name, tt.patterns, tt.defaultMatch); got != tt.want {
			t.Errorf("MatchGlob(%q, %v, %v) = %v, want %v",
				tt.name, tt.patterns, tt.defaultMatch, got, tt.want)
		}
	}
}

func TestLoadProfiles(t *testing.T) {
	isolateSharedFiles(t)
	cfg := writeSharedFile(t, "config", `# a comment
; another comment
[default]
region = us-east-1
output = json

[profile staging]
region = eu-west-1

[profile noregion]

[sso-session corp]
sso_start_url = https://corp.awsapps.com/start

[services my-services]
endpoint_url = http://localhost
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)

	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	want := []Profile{
		{Name: "default", Region: "us-east-1", Source: SourceConfig},
		{Name: "staging", Region: "eu-west-1", Source: SourceConfig},
		{Name: "noregion", Region: "", Source: SourceConfig},
	}
	if len(profiles) != len(want) {
		t.Fatalf("got %d profiles %v, want %d", len(profiles), profiles, len(want))
	}
	for i, w := range want {
		if profiles[i] != w {
			t.Errorf("profile[%d] = %+v, want %+v", i, profiles[i], w)
		}
	}
}

func TestLoadProfilesMissingFile(t *testing.T) {
	isolateSharedFiles(t)
	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("missing shared files should not be an error, got: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("got %d profiles, want 0", len(profiles))
	}
}

func TestLoadProfilesCredentialsOnly(t *testing.T) {
	isolateSharedFiles(t)
	creds := writeSharedFile(t, "credentials", `[alpha]
aws_access_key_id = AKIAEXAMPLE
aws_secret_access_key = secret
region = ap-south-1

[external]
credential_process = /usr/local/bin/cred-helper --account 123
`)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)

	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	want := []Profile{
		{Name: "alpha", Region: "ap-south-1", Source: SourceCredentials},
		{Name: "external", Region: "", Source: SourceCredentials},
	}
	if len(profiles) != len(want) {
		t.Fatalf("got %d profiles %v, want %d", len(profiles), profiles, len(want))
	}
	for i, w := range want {
		if profiles[i] != w {
			t.Errorf("profile[%d] = %+v, want %+v", i, profiles[i], w)
		}
	}
}

func TestLoadProfilesMerge(t *testing.T) {
	isolateSharedFiles(t)
	cfg := writeSharedFile(t, "config", `[default]
region = us-east-1

[profile staging]
region = eu-west-1

[profile cfgonly]
region = us-west-1
`)
	creds := writeSharedFile(t, "credentials", `[staging]
aws_access_key_id = AKIAEXAMPLE
region = us-west-2

[credsonly]
aws_access_key_id = AKIAEXAMPLE

[credsonly]
region = sa-east-1
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)

	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	want := []Profile{
		{Name: "default", Region: "us-east-1", Source: SourceConfig},
		{Name: "staging", Region: "us-west-2", Source: SourceBoth}, // credentials region wins
		{Name: "cfgonly", Region: "us-west-1", Source: SourceConfig},
		{Name: "credsonly", Region: "sa-east-1", Source: SourceCredentials}, // repeated section, last region wins
	}
	if len(profiles) != len(want) {
		t.Fatalf("got %d profiles %v, want %d", len(profiles), profiles, len(want))
	}
	for i, w := range want {
		if profiles[i] != w {
			t.Errorf("profile[%d] = %+v, want %+v", i, profiles[i], w)
		}
	}
}

func TestLoadProfilesCredentialsVerbatimHeader(t *testing.T) {
	isolateSharedFiles(t)
	cfg := writeSharedFile(t, "config", `[profile foo]
region = us-east-1
`)
	creds := writeSharedFile(t, "credentials", `[profile foo]
aws_access_key_id = AKIAEXAMPLE
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)

	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	// The config header names profile "foo"; the credentials header is taken
	// verbatim as "profile foo" (botocore semantics). They must coexist.
	want := []Profile{
		{Name: "foo", Region: "us-east-1", Source: SourceConfig},
		{Name: "profile foo", Region: "", Source: SourceCredentials},
	}
	if len(profiles) != len(want) {
		t.Fatalf("got %d profiles %v, want %d", len(profiles), profiles, len(want))
	}
	for i, w := range want {
		if profiles[i] != w {
			t.Errorf("profile[%d] = %+v, want %+v", i, profiles[i], w)
		}
	}
}

func TestLoadProfilesDefaultPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AWS_CONFIG_FILE", "")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "")
	awsDir := filepath.Join(home, ".aws")
	if err := os.MkdirAll(awsDir, 0o700); err != nil {
		t.Fatalf("mkdir .aws: %v", err)
	}
	if err := os.WriteFile(filepath.Join(awsDir, "config"), []byte("[profile fromconfig]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(awsDir, "credentials"), []byte("[fromcreds]\naws_access_key_id = AKIAEXAMPLE\n"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	want := []Profile{
		{Name: "fromconfig", Region: "us-east-1", Source: SourceConfig},
		{Name: "fromcreds", Region: "", Source: SourceCredentials},
	}
	if len(profiles) != len(want) {
		t.Fatalf("got %d profiles %v, want %d", len(profiles), profiles, len(want))
	}
	for i, w := range want {
		if profiles[i] != w {
			t.Errorf("profile[%d] = %+v, want %+v", i, profiles[i], w)
		}
	}
}

func TestResolveTargetsNoProfiles(t *testing.T) {
	isolateSharedFiles(t)
	_, err := ResolveTargets(context.Background(), Selector{})
	if err == nil {
		t.Fatal("expected an error with zero profiles")
	}
	msg := err.Error()
	for _, wantPart := range []string{
		os.Getenv("AWS_CONFIG_FILE"),
		os.Getenv("AWS_SHARED_CREDENTIALS_FILE"),
		"AWS_CONFIG_FILE",
		"AWS_SHARED_CREDENTIALS_FILE",
		"not found",
		"aws configure",
		"awsmux doctor",
	} {
		if !strings.Contains(msg, wantPart) {
			t.Errorf("error message missing %q:\n%s", wantPart, msg)
		}
	}
}

func TestCredentialProcessProfilePassthrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub")
	}
	isolateSharedFiles(t)
	creds := writeSharedFile(t, "credentials", `[external]
credential_process = /usr/local/bin/cred-helper --account 123
`)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWSMUX_HOME", t.TempDir())

	stub := filepath.Join(t.TempDir(), "aws")
	script := "#!/bin/sh\n" +
		`echo '{"UserId":"AIDAEXAMPLE","Account":"111122223333","Arn":"arn:aws:iam::111122223333:user/external"}'` + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(AWSBinEnv, stub)

	targets, err := ResolveTargets(context.Background(), Selector{Preflight: true})
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	tg := targets[0]
	if tg.Source != SourceCredentials {
		t.Errorf("Source = %q, want %q", tg.Source, SourceCredentials)
	}
	if tg.PreflightErr != "" {
		t.Fatalf("unexpected preflight error: %s", tg.PreflightErr)
	}
	// The profile resolves through the CLI seam untouched: awsmux never
	// interprets credential_process itself.
	if tg.AccountID != "111122223333" {
		t.Errorf("AccountID = %q, want 111122223333", tg.AccountID)
	}
}

func TestNewTarget(t *testing.T) {
	if got := NewTarget("prod", "us-east-1").ID; got != "prod@us-east-1" {
		t.Errorf("id = %q, want prod@us-east-1", got)
	}
	if got := NewTarget("prod", "").ID; got != "prod" {
		t.Errorf("id = %q, want prod", got)
	}
}

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
	_, orgSel, err := ResolveTargetsWithOrg(context.Background(), Selector{
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
	// The error's own hint tells the caller to list the unreachable accounts;
	// the selection carrying them must survive the error return, or nothing
	// can honor that hint without re-enumerating the org from scratch.
	if orgSel == nil {
		t.Fatal("OrgSelection must be populated on a zero-match error so callers can honor the --show-unreachable hint")
	}
	if len(orgSel.Unreachable) != 2 {
		t.Errorf("Unreachable = %+v, want both eng/prod accounts", orgSel.Unreachable)
	}
}

// TestResolveTargetsOrgKeepsPreflightErroredTargets verifies that a target
// whose identity preflight failed is carried through the org filter
// untouched rather than dropped. Such a target has no verified AccountID to
// join on; dropping it would turn a blocking preflight failure into a
// silently narrower fan-out, and would misreport its account as
// "unreachable" when in fact no account was ever established.
func TestResolveTargetsOrgKeepsPreflightErroredTargets(t *testing.T) {
	isolateSharedFiles(t)
	cfg := writeSharedFile(t, "config", `[profile alpha]
region = us-east-1

[profile broken]
region = us-east-1
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWSMUX_HOME", t.TempDir())

	stub := filepath.Join(t.TempDir(), "aws")
	script := `#!/bin/sh
svc=$1
op=$2
case "$svc $op" in
  "sts get-caller-identity")
    case "$*" in
      *"--profile alpha"*)  echo '{"UserId":"AIDAA","Account":"111122223333","Arn":"arn:aws:iam::111122223333:user/alpha"}' ;;
      *"--profile broken"*) echo 'ExpiredTokenException: token expired' >&2; exit 254 ;;
      *) echo "unexpected sts profile: $*" >&2; exit 1 ;;
    esac
    ;;
  "organizations describe-organization")
    echo '{"Organization":{"MasterAccountId":"999988887777"}}' ;;
  "organizations list-roots")
    echo '{"Roots":[{"Id":"r-root"}]}' ;;
  "organizations list-accounts-for-parent")
    case "$*" in
      *ou-prod*) echo '{"Accounts":[{"Id":"111122223333","Name":"prod-web","Status":"ACTIVE"}]}' ;;
      *)         echo '{"Accounts":[]}' ;;
    esac
    ;;
  "organizations list-organizational-units-for-parent")
    case "$*" in
      *r-root*) echo '{"OrganizationalUnits":[{"Id":"ou-eng","Name":"eng"}]}' ;;
      *ou-eng*) echo '{"OrganizationalUnits":[{"Id":"ou-prod","Name":"prod"}]}' ;;
      *)        echo '{"OrganizationalUnits":[]}' ;;
    esac
    ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(AWSBinEnv, stub)

	targets, _, err := ResolveTargetsWithOrg(context.Background(), Selector{OU: []string{"eng/prod"}})
	if err != nil {
		t.Fatalf("ResolveTargetsWithOrg: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2 (the matched account plus the unverified one): %+v", len(targets), targets)
	}

	var broken *Target
	for i := range targets {
		if targets[i].Profile == "broken" {
			broken = &targets[i]
		}
	}
	if broken == nil {
		t.Fatal("preflight-errored target was dropped by the org filter instead of carried through")
	}
	if broken.PreflightErr == "" {
		t.Error("errored target lost its PreflightErr")
	}

	if err := CheckVerified(targets); err == nil {
		t.Fatal("CheckVerified must still block on the unverified target; the org filter must not silently narrow the fan-out around it")
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

func TestMarkDuplicates(t *testing.T) {
	mk := func(id, account, principal, region, preflightErr string) Target {
		return Target{ID: id, Profile: id, AccountID: account, Principal: principal, Region: region, PreflightErr: preflightErr}
	}
	targets := []Target{
		mk("a", "111", "arn:role/x", "us-east-1", ""),
		mk("b", "111", "arn:role/x", "us-east-1", ""),            // dup of a
		mk("c", "111", "arn:role/x", "eu-west-1", ""),            // different region, not dup
		mk("d", "222", "arn:role/x", "us-east-1", ""),            // different account, not dup
		mk("e", "111", "arn:role/x", "us-east-1", "sso expired"), // errored, never marked
		mk("f", "", "", "us-east-1", ""),                         // unresolved, never marked
		mk("g", "", "", "us-east-1", ""),                         // unresolved, never marked
	}
	got := MarkDuplicates(targets)
	wantDup := map[string]bool{"a": false, "b": true, "c": false, "d": false, "e": false, "f": false, "g": false}
	for _, tg := range got {
		if tg.Duplicate != wantDup[tg.ID] {
			t.Errorf("target %s: Duplicate = %v, want %v", tg.ID, tg.Duplicate, wantDup[tg.ID])
		}
	}
}
