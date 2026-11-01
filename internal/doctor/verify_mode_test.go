package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// v0.10.0 made new projects verify in a clean checkout of what would be
// committed, but projects registered earlier keep in_place, where an ignored
// file the agent made or changed can decide the checks without being in the
// commit. On 2026-11-01 11 of 13 projects were in_place, including every one
// in daily use (aidev, Axiom-Office, Zenford), and 127 of 160 tasks had run
// that way, TASK-000143 among them. Nothing told anyone.
//
// The check is a warning: in_place is a choice a project may need (a
// repository that pins submodules cannot verify clean), so it never fails
// doctor, but it names the projects and the command that changes one.

func TestInPlaceProjectsAreAWarningThatNamesThem(t *testing.T) {
	deps := healthy()
	deps.InPlaceProjects = func(context.Context, string) ([]string, error) {
		return []string{"/home/you/work/app", "/home/you/work/site"}, nil
	}
	results := Run(context.Background(), deps)
	r := byName(t, results)[CheckVerifyMode]

	if r.Status != StatusWarn {
		t.Fatalf("status %s, want warn (summary %q)", r.Status, r.Summary)
	}
	for _, path := range []string{"/home/you/work/app", "/home/you/work/site"} {
		if !strings.Contains(r.Summary, path) {
			t.Errorf("summary %q does not name %s", r.Summary, path)
		}
	}
	if !strings.Contains(r.Summary, "2 projects") {
		t.Errorf("summary %q does not count the projects", r.Summary)
	}
	for _, want := range []string{"aidev project verify-mode", "clean"} {
		if !strings.Contains(r.Fix, want) {
			t.Errorf("fix %q does not mention %q", r.Fix, want)
		}
	}
	if Failed(results) {
		t.Error("an in_place project failed doctor; it is a warning")
	}
}

func TestOneInPlaceProjectIsCountedInTheSingular(t *testing.T) {
	deps := healthy()
	deps.InPlaceProjects = func(context.Context, string) ([]string, error) { return []string{"/w/app"}, nil }
	r := byName(t, Run(context.Background(), deps))[CheckVerifyMode]
	if !strings.Contains(r.Summary, "1 project ") {
		t.Errorf("summary %q, want \"1 project ...\"", r.Summary)
	}
}

func TestEveryProjectVerifyingCleanIsOK(t *testing.T) {
	r := byName(t, Run(context.Background(), healthy()))[CheckVerifyMode]
	if r.Status != StatusOK || strings.TrimSpace(r.Summary) == "" {
		t.Errorf("got %+v, want ok with a summary", r)
	}
}

// A lookup that fails says so as a warning: the check is advice, and a
// database that answered the other checks is not broken because of it.
func TestInPlaceLookupErrorIsAWarning(t *testing.T) {
	deps := healthy()
	deps.InPlaceProjects = func(context.Context, string) ([]string, error) {
		return nil, errors.New("connect to " + secretURL + ": refused")
	}
	results := Run(context.Background(), deps)
	r := byName(t, results)[CheckVerifyMode]
	if r.Status != StatusWarn {
		t.Errorf("status %s, want warn", r.Status)
	}
	if strings.Contains(r.Summary, "hunter2") {
		t.Errorf("summary leaks the database password: %q", r.Summary)
	}
	if Failed(results) {
		t.Error("a failed lookup failed doctor")
	}
}

func TestVerifyModeIsSkippedWhenTheDatabaseDoesNotAnswer(t *testing.T) {
	deps := healthy()
	deps.PingDatabase = func(context.Context, string) error { return errors.New("connection refused") }
	called := false
	deps.InPlaceProjects = func(context.Context, string) ([]string, error) { called = true; return nil, nil }
	r := byName(t, Run(context.Background(), deps))[CheckVerifyMode]
	if r.Status != StatusSkipped {
		t.Errorf("status %s, want skipped", r.Status)
	}
	if called {
		t.Error("projects were listed from a database that does not answer")
	}
}
