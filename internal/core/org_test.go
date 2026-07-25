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
