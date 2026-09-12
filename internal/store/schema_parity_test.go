package store

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"

	"aidev/internal/event"
	"aidev/internal/task"
	"aidev/migrations"
)

// The Go enums and the database CHECK constraints are two statements of the same
// vocabulary, and nothing but a test keeps them in agreement. Without this,
// adding a status in Go and forgetting the migration would only surface as a
// check_violation the first time that status was written, in production.
func TestEnumsMatchMigrationConstraints(t *testing.T) {
	schema := readSchema(t)

	cases := []struct {
		constraint string
		values     []string
	}{
		{"tasks_status_valid", strs(task.AllStatuses())},
		{"task_attempts_status_valid", strs(task.AllAttemptStatuses())},
		{"task_attempts_failure_valid", append([]string{""}, strs(task.AllFailureKinds())...)},
		{"worker_runs_status_valid", strs(task.AllWorkerRunStatuses())},
		{"worker_runs_failure_valid", append([]string{""}, strs(task.AllFailureKinds())...)},
		{"verification_runs_status_valid", strs(task.AllVerificationStatuses())},
		{"worktrees_status_valid", strs(task.AllWorktreeStatuses())},
		{"approvals_status_valid", strs(task.AllApprovalStatuses())},
		{"events_type_valid", strs(event.AllTypes())},
	}

	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			inSQL := constraintValues(t, schema, tc.constraint)
			inGo := normalise(tc.values)

			if missing := difference(inGo, inSQL); len(missing) > 0 {
				t.Errorf("declared in Go but not allowed by constraint %s: %v\n"+
					"add a migration that widens the constraint, or the first write of that value will be rejected",
					tc.constraint, missing)
			}
			if extra := difference(inSQL, inGo); len(extra) > 0 {
				t.Errorf("allowed by constraint %s but not declared in Go: %v\n"+
					"either the Go enum is missing a value or the constraint is stale",
					tc.constraint, extra)
			}
		})
	}
}

// The migration must stay readable as the single source of the schema, so the
// test also asserts the file is actually reachable and non-trivial.
func TestMigrationsLoad(t *testing.T) {
	loaded, err := LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("no migrations embedded")
	}

	seen := map[string]bool{}
	for _, m := range loaded {
		if m.Version == "" {
			t.Error("migration with an empty version")
		}
		if seen[m.Version] {
			t.Errorf("duplicate migration version %s", m.Version)
		}
		seen[m.Version] = true
		if len(m.Checksum) != 64 {
			t.Errorf("migration %s checksum = %q, want a hex sha256", m.Version, m.Checksum)
		}
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %s is empty", m.Version)
		}
	}

	// Lexical order must equal apply order, which is what makes zero-padded
	// filenames a requirement rather than a convention.
	versions := make([]string, len(loaded))
	for i, m := range loaded {
		versions[i] = m.Version
	}
	sorted := append([]string(nil), versions...)
	sort.Strings(sorted)
	if strings.Join(versions, ",") != strings.Join(sorted, ",") {
		t.Errorf("migrations are not in lexical order: %v", versions)
	}
}

func TestLoadMigrationsRejectsEmptyFS(t *testing.T) {
	if _, err := LoadMigrations(emptyFS{}); err == nil {
		t.Fatal("an empty filesystem should be an error, not an empty migration set")
	}
}

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func readSchema(t *testing.T) string {
	t.Helper()
	body, err := fs.ReadFile(migrations.FS, "0001_init.sql")
	if err != nil {
		t.Fatalf("read initial migration: %v", err)
	}
	return string(body)
}

var quoted = regexp.MustCompile(`'([^']*)'`)

// constraintValues extracts the quoted literals belonging to one named
// constraint. Constraints are separated by the CONSTRAINT keyword, so the slice
// between this constraint's name and the next one contains exactly its literals.
func constraintValues(t *testing.T, schema, name string) []string {
	t.Helper()

	start := strings.Index(schema, "CONSTRAINT "+name)
	if start < 0 {
		t.Fatalf("constraint %s not found in the migration", name)
	}
	rest := schema[start+len("CONSTRAINT "+name):]
	if next := strings.Index(rest, "CONSTRAINT "); next >= 0 {
		rest = rest[:next]
	}

	var values []string
	for _, m := range quoted.FindAllStringSubmatch(rest, -1) {
		values = append(values, m[1])
	}
	if len(values) == 0 {
		t.Fatalf("constraint %s lists no literal values", name)
	}
	return normalise(values)
}

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func normalise(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func difference(a, b []string) []string {
	inB := map[string]bool{}
	for _, v := range b {
		inB[v] = true
	}
	var out []string
	for _, v := range a {
		if !inB[v] {
			out = append(out, v)
		}
	}
	return out
}
