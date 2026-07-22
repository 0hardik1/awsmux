package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// AGENT CONTRACT (discovery): implement every stub in this file. Keep the
// signatures exactly as written; add unexported helpers in this file freely.

// LoadProfiles parses the AWS shared config file (AWS_CONFIG_FILE or
// ~/.aws/config) and the shared credentials file (AWS_SHARED_CREDENTIALS_FILE
// or ~/.aws/credentials) and returns every profile with its default region
// and source.
//
// Rules:
//   - Config file: section "[default]" is profile "default"; "[profile name]"
//     is "name"; anything else (sso-session, services, ...) is skipped.
//   - Credentials file: every section name is a profile name, verbatim
//     (botocore semantics, so "[profile foo]" there is literally
//     "profile foo").
//   - One profile per name. Config-file profiles come first in file order;
//     credentials-only profiles are appended in file order. A profile in both
//     files has Source "both" and a non-empty credentials-file region
//     overrides the config one (AWS CLI precedence).
//   - Only the "region" key matters here; every other key, including
//     credential material and credential_process, is ignored because awsmux
//     never resolves credentials itself: it always shells out to
//     `aws --profile <name>`.
//   - A missing file is not an error: it simply contributes no profiles.
//   - Hand-roll the INI parsing (comments start with # or ;, keys are
//     "key = value"); do not add a dependency.
func LoadProfiles() ([]Profile, error) {
	cfg, err := loadProfilesFromFile(awsConfigPath(), "config", SourceConfig, profileSectionName)
	if err != nil {
		return nil, err
	}
	creds, err := loadProfilesFromFile(awsCredentialsPath(), "credentials", SourceCredentials, credentialsSectionName)
	if err != nil {
		return nil, err
	}
	return mergeProfiles(cfg, creds), nil
}

// loadProfilesFromFile parses one shared AWS file into profiles in first-
// appearance order, mapping section headers to profile names via mapName.
// Repeated sections for the same profile collapse into one entry with the
// last non-empty region winning. A missing file yields (nil, nil); any other
// read error is returned wrapped with the file's label and path.
func loadProfilesFromFile(path, label string, src ProfileSource, mapName func(string) (string, bool)) ([]Profile, error) {
	sections, err := parseINIFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read aws %s file %s: %w", label, path, err)
	}

	var profiles []Profile
	index := make(map[string]int)
	for _, s := range sections {
		name, ok := mapName(s.name)
		if !ok {
			continue
		}
		region := s.keys["region"]
		if i, dup := index[name]; dup {
			// Repeated section for the same profile: last region wins.
			if region != "" {
				profiles[i].Region = region
			}
			continue
		}
		index[name] = len(profiles)
		profiles = append(profiles, Profile{Name: name, Region: region, Source: src})
	}
	return profiles, nil
}

// mergeProfiles combines config-file and credentials-file profiles into one
// list: config order first, credentials-only profiles appended. A name in
// both files becomes a single profile with Source "both", and a non-empty
// credentials region overrides the config one. Never returns nil.
func mergeProfiles(cfg, creds []Profile) []Profile {
	merged := make([]Profile, 0, len(cfg)+len(creds))
	index := make(map[string]int, len(cfg))
	for _, p := range cfg {
		index[p.Name] = len(merged)
		merged = append(merged, p)
	}
	for _, c := range creds {
		if i, ok := index[c.Name]; ok {
			merged[i].Source = SourceBoth
			if c.Region != "" {
				merged[i].Region = c.Region
			}
			continue
		}
		index[c.Name] = len(merged)
		merged = append(merged, c)
	}
	return merged
}

// ResolveTargets expands a Selector into concrete targets:
//  1. LoadProfiles, filter with MatchGlob (Profiles include, Exclude remove).
//  2. Expand profiles x sel.Regions; empty Regions means one target per
//     profile with the profile's default region.
//  3. If sel.Preflight or sel.Dedupe, run Preflight then MarkDuplicates.
//  4. If sel.Dedupe, drop targets with Duplicate == true.
//
// Returns ExitConfigError-worthy errors (no profiles matched, config
// unreadable) as plain errors; callers map them to exit codes.
func ResolveTargets(ctx context.Context, sel Selector) ([]Target, error) {
	profiles, err := LoadProfiles()
	if err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}

	var matched []Profile
	for _, p := range profiles {
		if MatchGlob(p.Name, sel.Profiles, true) && !MatchGlob(p.Name, sel.Exclude, false) {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		if len(profiles) == 0 {
			return nil, noProfilesError()
		}
		return nil, fmt.Errorf("no profiles matched selector %v", sel.Profiles)
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

	if sel.Preflight || sel.Dedupe {
		targets = Preflight(ctx, targets)
		targets = MarkDuplicates(targets)
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
	return targets, nil
}

// NewTarget builds a Target with its stable ID ("profile@region", or just
// "profile" when region is empty).
func NewTarget(profile, region string) Target {
	id := profile
	if region != "" {
		id = profile + "@" + region
	}
	return Target{ID: id, Profile: profile, Region: region}
}

// MatchGlob reports whether name matches any of the shell-style glob
// patterns (use path.Match semantics). An empty pattern list returns
// defaultMatch.
func MatchGlob(name string, patterns []string, defaultMatch bool) bool {
	if len(patterns) == 0 {
		return defaultMatch
	}
	for _, p := range patterns {
		// Malformed patterns simply never match.
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// awsConfigPath returns AWS_CONFIG_FILE when set, else ~/.aws/config.
func awsConfigPath() string {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws", "config")
}

// awsCredentialsPath returns AWS_SHARED_CREDENTIALS_FILE when set, else
// ~/.aws/credentials.
func awsCredentialsPath() string {
	if p := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws", "credentials")
}

// noProfilesError explains exactly which files discovery checked, whether
// each path came from an env override, and what to do next. Kept short: it
// also reaches MCP agents as a domain error string.
func noProfilesError() error {
	var b strings.Builder
	b.WriteString("no AWS profiles found\n")
	for _, f := range []struct{ path, envVar string }{
		{awsConfigPath(), "AWS_CONFIG_FILE"},
		{awsCredentialsPath(), "AWS_SHARED_CREDENTIALS_FILE"},
	} {
		origin := "default path"
		if os.Getenv(f.envVar) != "" {
			origin = f.envVar
		}
		status := "no profiles"
		if _, err := os.Stat(f.path); err != nil {
			status = "not found"
		}
		fmt.Fprintf(&b, "  checked %s (%s; %s)\n", f.path, origin, status)
	}
	b.WriteString(`hint: run "aws configure" or "aws configure sso" to create a profile, then "awsmux doctor" to verify the setup`)
	return errors.New(b.String())
}

// profileSectionName maps a config-file INI section header to a profile
// name: "default" stays "default", "profile x" becomes "x", anything else
// (sso-session, services, ...) is not a profile. Applies to the shared
// config file only; the credentials file uses credentialsSectionName.
func profileSectionName(section string) (string, bool) {
	if section == "default" {
		return "default", true
	}
	if rest, ok := strings.CutPrefix(section, "profile "); ok {
		name := strings.TrimSpace(rest)
		return name, name != ""
	}
	return "", false
}

// credentialsSectionName maps a credentials-file section header to a profile
// name: the section name is the profile name, verbatim (botocore semantics).
// Only empty names are rejected.
func credentialsSectionName(section string) (string, bool) {
	return section, section != ""
}

// iniSection is one "[header]" block of an AWS shared config file, in file
// order, with keys lowercased.
type iniSection struct {
	name string
	keys map[string]string
}

func parseINIFile(path string) ([]iniSection, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var sections []iniSection
	var cur *iniSection
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			sections = append(sections, iniSection{name: name, keys: make(map[string]string)})
			cur = &sections[len(sections)-1]
			continue
		}
		if cur == nil {
			// Key before any section header: not valid AWS config, skip.
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		if key != "" {
			cur.keys[key] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sections, nil
}
