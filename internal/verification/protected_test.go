package verification

import (
	"strings"
	"testing"
)

func TestNormalizeProtectedCanonicalises(t *testing.T) {
	got, err := NormalizeProtected([]string{"  docs/**  ", "ci/", "ci", "migrations/*", ".env*"})
	if err != nil {
		t.Fatalf("NormalizeProtected: %v", err)
	}
	// Sorted, deduplicated, whitespace and trailing slashes gone: "ci/" and
	// "ci" are one protection, not two that must both be written.
	want := []string{".env*", "ci", "docs/**", "migrations/*"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("NormalizeProtected = %v, want %v", got, want)
	}
}

func TestNormalizeProtectedRejectsUnusablePatterns(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"blank":      "   ",
		"just slash": "/",
		"bad glob":   "migrations/[",
	}
	for name, pattern := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeProtected([]string{pattern}); err == nil {
				t.Errorf("NormalizeProtected(%q) accepted, want an error: a protection that cannot match protects nothing", pattern)
			}
		})
	}
}

func TestNormalizeProtectedEmptyList(t *testing.T) {
	got, err := NormalizeProtected(nil)
	if err != nil || got != nil {
		t.Errorf("NormalizeProtected(nil) = %v, %v; want nil, nil", got, err)
	}
}

// The refusal the whole feature exists for: the attempt changed a path the
// task's creator ring-fenced. Every case here is a pair a reviewer should see
// in the event payload, so the table doubles as documentation of what the
// patterns mean.
func TestViolationsClaimsTouchedProtectedPaths(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		changed  []string
	}{
		{"exact file at the root", []string{"go.mod"}, []string{"go.mod"}},
		{"unanchored name at any depth", []string{".env*"}, []string{"config/.env.local"}},
		{"unanchored directory protects its contents", []string{"migrations"}, []string{"migrations/0001_init.sql"}},
		{"anchored glob on a listed file", []string{"migrations/*"}, []string{"migrations/0001_init.sql"}},
		{"anchored glob protects the subtree of a matched child", []string{"docs/*"}, []string{"docs/api/a.md"}},
		{"doublestar crosses directories", []string{"docs/**"}, []string{"docs/deep/nested/a.md"}},
		{"trailing-slash pattern matches files under it", []string{"ci"}, []string{"ci/job.yml"}},
		{"wildcard with an extension", []string{"data/*.csv"}, []string{"data/rows.csv"}},
		{"question mark is one character", []string{"ci/job-?.sh"}, []string{"ci/job-1.sh"}},
		{"bracket class", []string{"data/[ab].csv"}, []string{"data/a.csv"}},
		{
			"collapsed directory and a glob inside it",
			[]string{"migrations/*"},
			[]string{"migrations/"},
		},
		{
			"collapsed directory and an extension pattern resolving under it",
			[]string{"migrations/*.sql"},
			[]string{"migrations/"},
		},
		{
			"collapsed directory itself named by the pattern",
			[]string{"secrets"},
			[]string{"secrets/"},
		},
		{
			"one changed path claimed by two patterns",
			[]string{"migrations", "migrations/*"},
			[]string{"migrations/0001_init.sql"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Violations(tc.patterns, tc.changed)
			if err != nil {
				t.Fatalf("Violations: %v", err)
			}
			if len(got) == 0 {
				t.Fatalf("Violations(%v, %v) claimed nothing, want a violation", tc.patterns, tc.changed)
			}
			// The first claim must pair some pattern with some changed path —
			// a violation naming neither side would be useless in the log.
			v := got[0]
			if v.Pattern == "" || v.Path == "" {
				t.Errorf("violation = %+v, both pattern and path must be named", v)
			}
		})
	}
}

// Untouched paths must pass: a guard that refuses every attempt protects
// nothing because nobody can run a task with it set.
func TestViolationsLeavesOtherPathsAlone(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		changed  []string
	}{
		{"unrelated file", []string{"migrations/*"}, []string{"internal/worker/worker.go"}},
		{"pattern with an extension against another extension", []string{"data/*.csv"}, []string{"data/rows.json"}},
		{"unanchored pattern does not reach a sibling directory", []string{".env*"}, []string{"docs/readme.md"}},
		{
			// The speculated children rule stays speculation: a collapsed
			// node_modules/ must not trip ".env*", or every attempt that
			// installs dependencies would be refused.
			"collapsed directory unrelated to the pattern",
			[]string{".env*"},
			[]string{"node_modules/"},
		},
		{
			"question mark does not match a longer name",
			[]string{"ci/job-?.sh"},
			[]string{"ci/job-12.sh"},
		},
		{"anchored pattern does not match a deeper single segment", []string{"migrations/*.sql"}, []string{"migrations/sub/a.sql"}},
		{"no patterns", nil, []string{"anything.txt"}},
		{"no changes", []string{"migrations"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Violations(tc.patterns, tc.changed)
			if err != nil {
				t.Fatalf("Violations: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("Violations = %+v, want none", got)
			}
		})
	}
}

func TestViolationsIsStableAndDeduplicated(t *testing.T) {
	// The database may hold an unsorted list with a repeated pattern; the
	// report must not depend on that, because two runs of the same attempt
	// should produce the same event payload.
	got, err := Violations([]string{"migrations/*", "migrations", "migrations"}, []string{"migrations/0001.sql", "migrations/"})
	if err != nil {
		t.Fatalf("Violations: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("want violations")
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if prev.Pattern == cur.Pattern && prev.Path == cur.Path {
			t.Errorf("duplicate violation %+v", cur)
		}
		if strings.Compare(prev.Pattern, cur.Pattern) > 0 {
			t.Errorf("unsorted: %+v then %+v", prev, cur)
		}
	}
}

// A stored pattern the current binary cannot parse must fail the run rather
// than silently stop guarding (fail closed, like ChangedPaths itself).
func TestViolationsFailsClosedOnAnUnusablePattern(t *testing.T) {
	if _, err := Violations([]string{"migrations/["}, []string{"migrations/0001.sql"}); err == nil {
		t.Fatal("an unparseable stored pattern was allowed through; the guard must fail closed")
	}
	if _, err := Violations([]string{""}, []string{"a.txt"}); err == nil {
		t.Fatal("an empty stored pattern was allowed through; the guard must fail closed")
	}
}
