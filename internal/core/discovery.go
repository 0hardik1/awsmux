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
// ~/.aws/config) and returns every profile with its default region.
//
// Rules:
//   - Section "[default]" is profile "default"; "[profile name]" is "name".
//   - Only the "region" key matters here; ignore everything else.
//   - A missing config file is not an error: return an empty slice.
//   - Hand-roll the INI parsing (comments start with # or ;, keys are
//     "key = value"); do not add a dependency.
func LoadProfiles() ([]Profile, error) {
	cfgPath := awsConfigPath()
	sections, err := parseINIFile(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Profile{}, nil
		}
		return nil, fmt.Errorf("read aws config %s: %w", cfgPath, err)
	}

	var profiles []Profile
	index := make(map[string]int)
	for _, s := range sections {
		name, ok := profileSectionName(s.name)
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
		profiles = append(profiles, Profile{Name: name, Region: region})
	}
	if profiles == nil {
		profiles = []Profile{}
	}
	return profiles, nil
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
			return nil, errors.New("no AWS profiles found (is ~/.aws/config present?)")
		}
		return nil, fmt.Errorf("no profiles matched selector %v", sel.Profiles)
	}

	var targets []Target
	for _, p := range matched {
		if len(sel.Regions) == 0 {
			targets = append(targets, NewTarget(p.Name, p.Region))
			continue
		}
		for _, r := range sel.Regions {
			targets = append(targets, NewTarget(p.Name, r))
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

// profileSectionName maps an INI section header to a profile name:
// "default" stays "default", "profile x" becomes "x", anything else
// (sso-session, services, ...) is not a profile.
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
