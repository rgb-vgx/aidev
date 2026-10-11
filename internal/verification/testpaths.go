package verification

import (
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// TestPaths reports which of the changed paths look like the tests that judge
// the work: files under a test directory, conventional test file names, or the
// Makefile a `make test` verification step would run through.
//
// The agent editing the tests edits the evidence (docs/research.md §7b). This
// classifier is deliberately heuristic and only *reports* — the paths are
// recorded as an event and surfaced in the result so a reviewer can see that
// what passed was also written during the same attempt. Failing the run on a
// heuristic would punish honest work (a fixture updated alongside the code it
// tests), so enforcement stays a separate, explicit decision (the task's
// protected_paths, research §7b tier 2).
//
// Order follows the input, which ChangedPaths keeps sorted, so the report is
// stable across runs.
func TestPaths(changed []string) []string {
	var out []string
	for _, p := range changed {
		if isTestPath(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	// ChangedPaths already answers with unique sorted paths, but the report
	// must not depend on that: collapse repeats so the event cannot list the
	// same path twice.
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// isTestPath classifies one changed path. A false positive costs a reviewer a
// second look; a false negative hides an edited test, so the pattern set errs
// toward naming anything a test runner could plausibly read.
func isTestPath(p string) bool {
	// path.Clean also strips the trailing "/" that a directory entry from
	// git ls-files --directory carries, so "tests/" becomes "tests".
	cleaned := path.Clean(filepath.ToSlash(p))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}

	// A file under a directory tests live in, at any depth: tests/x,
	// internal/foo/tests/x, __tests__/x, testdata/x.
	for seg := range strings.SplitSeq(path.Dir(cleaned), "/") {
		switch seg {
		case "test", "tests", "__tests__", "testdata":
			return true
		}
	}

	base := path.Base(cleaned)

	// The path itself may be a test directory: git reports "tests/" for a
	// changed directory, and Clean turns that into "tests", whose Dir is "."
	// and would match nothing above.
	switch base {
	case "test", "tests", "__tests__", "testdata":
		return true
	}

	switch {
	case base == "conftest.py":
		// pytest loads it for every test in the directory, so editing it edits
		// how every neighbouring test behaves.
		return true
	case base == "Makefile" || base == "makefile":
		// Verification steps are often `make test`; a changed Makefile changes
		// what that step runs.
		return true
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.HasSuffix(base, "_test.py"):
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
		return true
	case strings.Contains(base, ".spec."):
		// foo.spec.ts and friends (Jest, Vitest, and the *.spec.* convention).
		return true
	case strings.Contains(base, ".test."):
		// foo.test.ts and friends: the other half of the JS convention.
		return true
	}
	return false
}

// isTestSource classifies one changed path for the interception guard, which
// *refuses* a verification step that would run a test the agent changed
// (matchesForStep in interception.go, runners.go). It is deliberately narrower
// than isTestPath: the report only names, so it can afford to err toward
// naming anything a runner could plausibly read; a refusal blocks the run, so
// this predicate names only files a runner executes, never the fixtures
// beside them. The two deliberately disagree on:
//
//   - a path under testdata/: the go tool ignores a directory named testdata
//     (measured: `go help test`, go1.26.8), making it available to hold
//     ancillary data the tests read. Editing a fixture edits the exam's
//     input, not the judge.
//   - Makefile/makefile: a changed Makefile changes what a `make test` step
//     runs, but refusing it would reverse the recorded decision that a
//     Makefile is the project's, like its tests (interception_test.go).
//
// Everything isTestSource accepts, isTestPath accepts: the guard never refuses
// on a path the report would not even name.
func isTestSource(p string) bool {
	if !isTestPath(p) {
		return false
	}
	cleaned := path.Clean(filepath.ToSlash(p))
	for seg := range strings.SplitSeq(path.Dir(cleaned), "/") {
		if seg == "testdata" {
			return false
		}
	}
	switch path.Base(cleaned) {
	case "testdata", "Makefile", "makefile":
		return false
	}
	return true
}
