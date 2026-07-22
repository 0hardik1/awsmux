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
