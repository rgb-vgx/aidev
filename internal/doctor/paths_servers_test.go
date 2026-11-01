package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Two things doctor did not see on 2026-11-01.
//
// A registered project whose directory is gone (.probe/fresh-demo) sat in the
// verify mode warning for good: `aidev project verify-mode clean --repo` cannot
// open a repository that is not there, so the advice could not be followed.
// A missing directory is its own finding, and the verify mode warning names
// only projects that can still be switched.
//
// More than ten `aidev mcp` processes from earlier Claude Code sessions kept
// running a binary that had since been replaced (/proc/<pid>/exe "(deleted)"),
// so tasks created through them still used the old creation checks after
// `make install`. Doctor names them so the person restarts those sessions.

func TestMissingProjectDirectoriesAreTheirOwnWarning(t *testing.T) {
	deps := healthy()
	deps.ProjectPaths = func(context.Context, string) ([]string, error) {
		return []string{"/w/app", "/w/gone"}, nil
	}
	deps.InPlaceProjects = func(context.Context, string) ([]string, error) {
		return []string{"/w/app", "/w/gone"}, nil
	}
	deps.PathExists = func(p string) bool { return p != "/w/gone" }

	results := Run(context.Background(), deps)
	got := byName(t, results)

	paths := got[CheckProjectPaths]
	if paths.Status != StatusWarn || !strings.Contains(paths.Summary, "/w/gone") || strings.Contains(paths.Summary, "/w/app") {
		t.Errorf("project paths = %+v, want a warning naming /w/gone only", paths)
	}
	if strings.TrimSpace(paths.Fix) == "" || !strings.Contains(paths.Fix, "aidev project add") {
		t.Errorf("project paths fix = %q, want it to say how to register a moved repository (aidev project add)", paths.Fix)
	}
	mode := got[CheckVerifyMode]
	if strings.Contains(mode.Summary, "/w/gone") {
		t.Errorf("verify mode summary %q names a project whose directory is gone; the advice cannot be followed there", mode.Summary)
	}
	if !strings.Contains(mode.Summary, "/w/app") {
		t.Errorf("verify mode summary %q lost the project that can still be switched", mode.Summary)
	}
	if Failed(results) {
		t.Error("a missing project directory failed doctor; it is a warning")
	}
}

func TestEveryProjectDirectoryPresentIsOK(t *testing.T) {
	deps := healthy()
	deps.ProjectPaths = func(context.Context, string) ([]string, error) { return []string{"/w/app"}, nil }
	r := byName(t, Run(context.Background(), deps))[CheckProjectPaths]
	if r.Status != StatusOK || strings.TrimSpace(r.Summary) == "" {
		t.Errorf("got %+v, want ok with a summary", r)
	}
}

func TestProjectPathsSkippedWhenTheDatabaseDoesNotAnswer(t *testing.T) {
	deps := healthy()
	deps.PingDatabase = func(context.Context, string) error { return errors.New("connection refused") }
	if r := byName(t, Run(context.Background(), deps))[CheckProjectPaths]; r.Status != StatusSkipped {
		t.Errorf("status %s, want skipped", r.Status)
	}
}

func TestServersRunningAReplacedBinaryAreAWarning(t *testing.T) {
	deps := healthy()
	deps.StaleServers = func() ([]string, error) {
		return []string{"pid 1951763 (started 2026-11-01 02:15)", "pid 2199410 (started 2026-11-01 04:01)"}, nil
	}
	results := Run(context.Background(), deps)
	r := byName(t, results)[CheckServers]
	if r.Status != StatusWarn {
		t.Fatalf("status %s, want warn (%+v)", r.Status, r)
	}
	for _, want := range []string{"1951763", "2199410", "2 aidev processes"} {
		if !strings.Contains(r.Summary, want) {
			t.Errorf("summary %q does not contain %q", r.Summary, want)
		}
	}
	if !strings.Contains(strings.ToLower(r.Fix), "restart") {
		t.Errorf("fix %q does not say to restart the sessions running them", r.Fix)
	}
	if Failed(results) {
		t.Error("stale servers failed doctor; they are a warning")
	}
}

func TestNoStaleServersIsOK(t *testing.T) {
	r := byName(t, Run(context.Background(), healthy()))[CheckServers]
	if r.Status != StatusOK || strings.TrimSpace(r.Summary) == "" {
		t.Errorf("got %+v, want ok with a summary", r)
	}
}

// Where the process table cannot be read the way the check needs (not Linux),
// the check says so instead of claiming all is well.
func TestServersCheckSkippedWhereUnsupported(t *testing.T) {
	deps := healthy()
	deps.StaleServers = func() ([]string, error) { return nil, errors.ErrUnsupported }
	if r := byName(t, Run(context.Background(), deps))[CheckServers]; r.Status != StatusSkipped {
		t.Errorf("status %s, want skipped", r.Status)
	}
	deps.StaleServers = nil
	if r := byName(t, Run(context.Background(), deps))[CheckServers]; r.Status != StatusSkipped {
		t.Errorf("nil StaleServers: status %s, want skipped", r.Status)
	}
}
