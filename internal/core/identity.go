package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AGENT CONTRACT (identity): implement every stub in this file. Keep the
// signatures exactly as written; add unexported helpers in this file freely.

// IdentityCacheTTL bounds how long a preflight result is trusted.
const IdentityCacheTTL = 5 * time.Minute

// preflightConcurrency bounds concurrent STS calls during Preflight.
const preflightConcurrency = 8

// Preflight verifies each target's identity concurrently (bounded at 8 in
// flight) by shelling out to:
//
//	aws sts get-caller-identity --profile <p> --output json
//
// It fills AccountID, Principal, CredentialExpiry, or PreflightErr on each
// target and returns the same slice in the same order. Results are cached
// per profile in <Dir()>/identity-cache.json with IdentityCacheTTL so
// repeated invocations (agents poll a lot) stay fast. Never trust the
// profile name; only STS output.
func Preflight(ctx context.Context, targets []Target) []Target {
	// One STS call per unique profile; identity does not vary by region.
	var profiles []string
	seen := make(map[string]bool)
	for _, t := range targets {
		if !seen[t.Profile] {
			seen[t.Profile] = true
			profiles = append(profiles, t.Profile)
		}
	}

	cache := loadIdentityCache()
	ids := make(map[string]Identity, len(profiles))
	var todo []string
	for _, p := range profiles {
		if id, ok := cache[p]; ok && id.Err == "" && time.Since(id.CheckedAt) < IdentityCacheTTL {
			ids[p] = id
		} else {
			todo = append(todo, p)
		}
	}

	if len(todo) > 0 {
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, preflightConcurrency)
		for _, p := range todo {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				id := stsCallerIdentity(ctx, p)
				mu.Lock()
				ids[p] = id
				mu.Unlock()
			}()
		}
		wg.Wait()

		// Only successes go to the cache; failures should be retried on
		// the next invocation, not remembered for five minutes.
		dirty := false
		for _, p := range todo {
			if id := ids[p]; id.Err == "" {
				cache[p] = id
				dirty = true
			}
		}
		if dirty {
			saveIdentityCache(cache)
		}
	}

	for i := range targets {
		id := ids[targets[i].Profile]
		targets[i].AccountID = id.AccountID
		targets[i].Principal = id.ARN
		targets[i].CredentialExpiry = id.ExpiresAt
		targets[i].PreflightErr = id.Err
	}
	return targets
}

// LookupIdentity resolves one profile via the cache or a live STS call.
// ExpiresAt is best effort: for SSO profiles, scan ~/.aws/sso/cache/*.json
// for the matching token and use its "expiresAt"; leave nil when unknown.
func LookupIdentity(ctx context.Context, profile string) Identity {
	cache := loadIdentityCache()
	if id, ok := cache[profile]; ok && id.Err == "" && time.Since(id.CheckedAt) < IdentityCacheTTL {
		return id
	}
	id := stsCallerIdentity(ctx, profile)
	if id.Err == "" {
		cache[profile] = id
		saveIdentityCache(cache)
	}
	return id
}

// MarkDuplicates flags every target whose (AccountID, Principal, Region)
// tuple already appeared earlier in the slice. Targets with a preflight
// error are never marked.
func MarkDuplicates(targets []Target) []Target {
	type key struct{ account, principal, region string }
	seen := make(map[key]bool)
	for i := range targets {
		t := &targets[i]
		if t.PreflightErr != "" {
			continue
		}
		if t.AccountID == "" && t.Principal == "" {
			// Identity was never resolved; cannot judge duplication.
			continue
		}
		k := key{t.AccountID, t.Principal, t.Region}
		if seen[k] {
			t.Duplicate = true
		} else {
			seen[k] = true
		}
	}
	return targets
}

// stsCallerIdentity performs the live STS call for one profile.
func stsCallerIdentity(ctx context.Context, profile string) Identity {
	id := Identity{Profile: profile, CheckedAt: time.Now().UTC()}

	cmd := awsExec(ctx, "sts", "get-caller-identity",
		"--profile", profile, "--output", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		id.Err = msg
		return id
	}

	var out struct {
		UserID  string `json:"UserId"`
		Account string `json:"Account"`
		Arn     string `json:"Arn"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		id.Err = fmt.Sprintf("parse sts get-caller-identity output: %v", err)
		return id
	}
	id.AccountID = out.Account
	id.ARN = out.Arn
	id.UserID = out.UserID
	id.ExpiresAt = ssoExpiry(profile)
	return id
}

// ssoExpiry returns the soonest matching SSO token expiry for the profile,
// or nil when the profile is not SSO-based or nothing usable is cached.
// Strictly best effort: every failure just yields nil.
func ssoExpiry(profile string) *time.Time {
	sections, err := parseINIFile(awsConfigPath())
	if err != nil {
		return nil
	}
	var prof map[string]string
	sessions := make(map[string]map[string]string)
	for _, s := range sections {
		if name, ok := profileSectionName(s.name); ok && name == profile {
			prof = s.keys
		}
		if rest, ok := strings.CutPrefix(s.name, "sso-session "); ok {
			sessions[strings.TrimSpace(rest)] = s.keys
		}
	}
	if prof == nil {
		return nil
	}
	startURL := prof["sso_start_url"]
	session := prof["sso_session"]
	if startURL == "" && session == "" {
		return nil
	}
	if startURL == "" {
		if sess, ok := sessions[session]; ok {
			startURL = sess["sso_start_url"]
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	files, err := filepath.Glob(filepath.Join(home, ".aws", "sso", "cache", "*.json"))
	if err != nil {
		return nil
	}
	var soonest *time.Time
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var tok struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   string `json:"expiresAt"`
			StartURL    string `json:"startUrl"`
		}
		// Only files with an accessToken are SSO tokens; others in the
		// cache dir (client registrations) have no useful expiry.
		if json.Unmarshal(data, &tok) != nil || tok.AccessToken == "" || tok.ExpiresAt == "" {
			continue
		}
		if startURL != "" && tok.StartURL != "" && tok.StartURL != startURL {
			continue
		}
		exp, err := time.Parse(time.RFC3339, tok.ExpiresAt)
		if err != nil {
			continue
		}
		if soonest == nil || exp.Before(*soonest) {
			soonest = &exp
		}
	}
	return soonest
}

func identityCachePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "identity-cache.json"), nil
}

// loadIdentityCache reads the profile -> Identity cache. Any read or decode
// problem (missing file, corrupt JSON) yields an empty map; caching is best
// effort and must never fail a preflight.
func loadIdentityCache() map[string]Identity {
	path, err := identityCachePath()
	if err != nil {
		return make(map[string]Identity)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return make(map[string]Identity)
	}
	var m map[string]Identity
	if json.Unmarshal(data, &m) != nil || m == nil {
		return make(map[string]Identity)
	}
	return m
}

// saveIdentityCache writes the cache atomically (temp file in the same
// directory, then rename) so concurrent readers never see partial JSON.
// All errors are swallowed; caching is best effort.
func saveIdentityCache(m map[string]Identity) {
	path, err := identityCachePath()
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "identity-cache-*.json")
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
	// The cache holds account IDs and ARNs; keep it owner-only.
	_ = os.Chmod(tmp.Name(), 0o600)
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
	}
}
