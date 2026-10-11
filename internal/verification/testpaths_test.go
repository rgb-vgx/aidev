package verification

import (
	"reflect"
	"testing"
)

func TestTestPathsClassifiesTestFiles(t *testing.T) {
	cases := []struct {
		path   string
		isTest bool
		why    string
	}{
		{"internal/store/store_test.go", true, "Go test file"},
		{"tests/test_parser.py", true, "pytest-style file under tests/"},
		{"pkg/parser_test.py", true, "pytest-style name"},
		{"test_parser.py", true, "pytest-style name at the root"},
		{"conftest.py", true, "pytest conftest"},
		{"web/src/login.spec.ts", true, "spec file"},
		{"web/src/login.test.ts", true, "test file"},
		{"tests/", true, "directory entry with trailing slash"},
		{"__tests__/unit", true, "jest directory"},
		{"testdata/golden.json", true, "testdata fixtures"},
		{"Makefile", true, "often the runner of make test"},
		{"go.mod", false, "module file, not a test"},
		{"internal/store/store.go", false, "production code"},
		{"web/src/login.ts", false, "production code"},
		{"contest.py", false, "test as a substring of another word"},
		{".", false, "the worktree root"},
		{"../escape.go", false, "outside the worktree"},
	}
	for _, tc := range cases {
		if got := isTestPath(tc.path); got != tc.isTest {
			t.Errorf("isTestPath(%q) = %v, want %v (%s)", tc.path, got, tc.isTest, tc.why)
		}
	}
}

// isTestSource is the refusal half of the classifier: everything it accepts,
// isTestPath accepts too, but a fixture and the Makefile — which the report
// names — never refuse a step.
func TestTestSourceIsNarrowerThanTestPath(t *testing.T) {
	cases := []struct {
		path   string
		wide   bool
		narrow bool
		why    string
	}{
		{"internal/foo/foo_test.go", true, true, "a Go test is executed by go test"},
		{"tests/test_x.py", true, true, "a pytest file is collected by pytest"},
		{"tests/x.test.mjs", true, true, "a node test is run by node --test"},
		{"conftest.py", true, true, "pytest loads it for the tests below it"},
		{"testdata/fixture.json", true, false, "a fixture is read, not executed"},
		{"testdata/", true, false, "a fixture directory is read, not executed"},
		{"Makefile", true, false, "the project's, like its tests: reported, never refused"},
		{"makefile", true, false, "the project's, like its tests: reported, never refused"},
		{"src/plain.py", false, false, "production code"},
		{"pytest.py", false, false, "a module name, not a test"},
	}
	for _, tc := range cases {
		if got := isTestPath(tc.path); got != tc.wide {
			t.Errorf("isTestPath(%q) = %v, want %v (%s)", tc.path, got, tc.wide, tc.why)
		}
		if got := isTestSource(tc.path); got != tc.narrow {
			t.Errorf("isTestSource(%q) = %v, want %v (%s)", tc.path, got, tc.narrow, tc.why)
		}
		if isTestSource(tc.path) && !isTestPath(tc.path) {
			t.Errorf("isTestSource(%q) accepts what isTestPath rejects: the guard must never refuse what the report would not name", tc.path)
		}
	}
}

// The report is sorted and deduplicated whatever order git answered in, so a
// reviewer comparing two runs sees stable output.
func TestTestPathsIsSortedAndFiltered(t *testing.T) {
	got := TestPaths([]string{
		"go.mod",
		"b_test.go",
		"src/main.go",
		"a_test.go",
		"a_test.go",
	})
	want := []string{"a_test.go", "b_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TestPaths = %v, want %v", got, want)
	}
	if got := TestPaths([]string{"go.mod", "README.md"}); got != nil {
		t.Errorf("TestPaths with no tests = %v, want nil so the field stays omitted", got)
	}
}
