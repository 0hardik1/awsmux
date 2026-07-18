package core

import (
	"os"
	"path/filepath"
	"testing"
)

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
	cfg := filepath.Join(t.TempDir(), "config")
	content := `# a comment
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
`
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfg)

	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	want := []Profile{
		{Name: "default", Region: "us-east-1"},
		{Name: "staging", Region: "eu-west-1"},
		{Name: "noregion", Region: ""},
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
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("missing config file should not be an error, got: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("got %d profiles, want 0", len(profiles))
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
