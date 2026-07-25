package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
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
	TagsFetched bool      `json:"tags_fetched"`
	FetchedAt   time.Time `json:"fetched_at"`
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

// matchTags reports whether the account carries every required tag pair.
// A tag filter requires each key to be present and its value to match, so
// an account whose tags were never fetched never satisfies a non-empty filter.
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
