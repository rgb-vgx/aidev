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
