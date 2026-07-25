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

// matchTags reports whether the account carries every required tag pair. An
// account with no tags satisfies only an empty requirement, so a tag filter
// against an untagged account fails closed.
func matchTags(acct OrgAccount, want map[string]string) bool {
	for k, v := range want {
		if acct.Tags[k] != v {
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
