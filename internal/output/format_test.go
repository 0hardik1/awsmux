package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/0hardik1/awsmux/internal/core"
)

func TestRenderTargetTableOmitsOUColumnWithoutOrgData(t *testing.T) {
	var buf bytes.Buffer
	targets := []core.Target{{ID: "alpha", Profile: "alpha", AccountID: "111122223333"}}
	if err := RenderTargets(&buf, targets, "table"); err != nil {
		t.Fatalf("RenderTargets: %v", err)
	}
	// Check the header for a standalone "OU" column, not a raw substring: the
	// existing SOURCE column already contains the letters "OU" (S-OU-RCE),
	// so strings.Contains would report a false failure on every run.
	header, _, _ := strings.Cut(buf.String(), "\n")
	for _, col := range strings.Fields(header) {
		if col == "OU" {
			t.Errorf("OU column present with no org data:\n%s", buf.String())
		}
	}
}

func TestRenderTargetTableShowsOUColumnWithOrgData(t *testing.T) {
	var buf bytes.Buffer
	targets := []core.Target{
		{ID: "alpha", Profile: "alpha", AccountID: "111122223333", OUPath: "eng/prod"},
		{ID: "beta", Profile: "beta", AccountID: "444455556666", OUPath: ""},
	}
	if err := RenderTargets(&buf, targets, "table"); err != nil {
		t.Fatalf("RenderTargets: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "eng/prod") {
		t.Errorf("OU value missing:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected a header and two rows:\n%s", out)
	}
	// Mirror image of the SOURCE-contains-"OU" bug: strings.Contains(out, "OU")
	// would pass vacuously since "SOURCE" is always in the header, whether or
	// not an OU column exists. Require an exact "OU" header token instead.
	ouIdx := -1
	for i, col := range strings.Fields(lines[0]) {
		if col == "OU" {
			ouIdx = i
		}
	}
	if ouIdx == -1 {
		t.Fatalf("OU column missing from header:\n%s", out)
	}
	// strings.Contains(out, "-") is equally vacuous: hyphens appear in
	// region names (us-east-1) and principal ARNs regardless of the OU
	// column. Find beta's row (its OUPath is "") and check the dash lands
	// specifically in the OU column, proving the empty path renders as a
	// dash rather than blank.
	var betaCols []string
	for _, line := range lines[1:] {
		cols := strings.Fields(line)
		if len(cols) > 0 && cols[0] == "beta" {
			betaCols = cols
			break
		}
	}
	if betaCols == nil {
		t.Fatalf("beta row not found:\n%s", out)
	}
	if ouIdx >= len(betaCols) || betaCols[ouIdx] != "-" {
		t.Errorf("empty OU path should render as a dash in the OU column (index %d):\n%s", ouIdx, out)
	}
}

func TestRenderUnreachableEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderUnreachable(&buf, nil, false); err != nil {
		t.Fatalf("RenderUnreachable: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for zero unreachable accounts, got:\n%s", buf.String())
	}
}

func TestRenderUnreachableTruncatesByDefault(t *testing.T) {
	accts := make([]core.OrgAccount, 12)
	for i := range accts {
		accts[i] = core.OrgAccount{ID: "10000000000" + string(rune('0'+i%10)), Name: "acct", OUPath: "eng/prod"}
	}
	var buf bytes.Buffer
	if err := RenderUnreachable(&buf, accts, false); err != nil {
		t.Fatalf("RenderUnreachable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "12 accounts") {
		t.Errorf("summary must state the full count:\n%s", out)
	}
	if !strings.Contains(out, "+9 more") {
		t.Errorf("summary must say how many were elided:\n%s", out)
	}
	if !strings.Contains(out, "--show-unreachable") {
		t.Errorf("summary must point at the flag that expands it:\n%s", out)
	}
}

func TestRenderUnreachableShowAllListsEvery(t *testing.T) {
	accts := []core.OrgAccount{
		{ID: "111122223333", Name: "prod-batch", OUPath: "eng/prod"},
		{ID: "222233334444", Name: "prod-ml", OUPath: "eng/prod"},
		{ID: "333344445555", Name: "prod-db", OUPath: "eng/prod/db"},
	}
	var buf bytes.Buffer
	if err := RenderUnreachable(&buf, accts, true); err != nil {
		t.Fatalf("RenderUnreachable: %v", err)
	}
	out := buf.String()
	for _, a := range accts {
		if !strings.Contains(out, a.ID) || !strings.Contains(out, a.Name) {
			t.Errorf("account %s missing from full listing:\n%s", a.ID, out)
		}
	}
	if strings.Contains(out, "more") {
		t.Errorf("full listing must not truncate:\n%s", out)
	}
}
