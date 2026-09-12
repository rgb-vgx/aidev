package procexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func spec(t *testing.T, command string, args ...string) Spec {
	t.Helper()
	return Spec{
		Command:        command,
		Args:           args,
		Dir:            t.TempDir(),
		Timeout:        20 * time.Second,
		MaxOutputBytes: 64 * 1024,
	}
}

func TestSuccess(t *testing.T) {
	res, err := Run(context.Background(), spec(t, "sh", "-c", "printf out; printf err >&2; exit 0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeSucceeded || !res.Succeeded() {
		t.Errorf("outcome = %s, want SUCCEEDED", res.Outcome)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if res.Stdout != "out" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "out")
	}
	if res.Stderr != "err" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "err")
	}
	if res.Duration <= 0 || res.FinishedAt.Before(res.StartedAt) {
		t.Errorf("timing is not coherent: %+v", res)
	}
}

func TestNonZeroExitIsAnOutcomeNotAnError(t *testing.T) {
	res, err := Run(context.Background(), spec(t, "sh", "-c", "exit 7"))
	if err != nil {
		t.Fatalf("a failing process must not be an error from Run: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Errorf("outcome = %s, want FAILED", res.Outcome)
	}
	if res.ExitCode == nil || *res.ExitCode != 7 {
		t.Errorf("exit code = %v, want 7", res.ExitCode)
	}
}

func TestTimeoutIsDistinguishedFromFailure(t *testing.T) {
	s := spec(t, "sh", "-c", "sleep 30")
	s.Timeout = 300 * time.Millisecond

	start := time.Now()
	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeTimedOut {
		t.Errorf("outcome = %s, want TIMED_OUT", res.Outcome)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s; the deadline did not stop the process", elapsed)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "timeout") {
		t.Errorf("Err = %v, want it to mention the timeout", res.Err)
	}
}

// A caller's cancellation and an elapsed deadline can both be visible by the time
// the process is reaped. The caller's intent is the more meaningful of the two.
func TestCancellationTakesPrecedenceOverTimeout(t *testing.T) {
	s := spec(t, "sh", "-c", "sleep 30")
	s.Timeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	res, err := Run(ctx, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeCancelled {
		t.Errorf("outcome = %s, want CANCELLED", res.Outcome)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", res.Err)
	}
}

func TestAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := Run(ctx, spec(t, "sh", "-c", "exit 0"))
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeCancelled && res.Outcome != OutcomeStartFailed {
		t.Errorf("outcome = %s, want CANCELLED or START_FAILED", res.Outcome)
	}
}

// Signalling the process group rather than the pid is what catches helpers. A
// child that outlives its parent would otherwise keep running, and for a
// verification command (go test starts compilers and test binaries) that is the
// normal shape, not an edge case.
func TestCancellationStopsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-still-running")

	s := spec(t, "sh", "-c",
		// The parent shell exits immediately, leaving a background child that
		// would create the marker unless the group is signalled.
		"(sleep 2; touch '"+marker+"') & sleep 30")
	s.Dir = dir
	s.Timeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	if _, err := Run(ctx, s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait past the point where the orphan would have acted.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a background child survived cancellation: the process group was not signalled")
	}
}

func TestOutputIsBoundedAndFlagged(t *testing.T) {
	s := spec(t, "sh", "-c", "printf 'x%.0s' $(seq 1 5000); printf 'y%.0s' $(seq 1 5000) >&2")
	s.MaxOutputBytes = 1024

	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome = %s, want SUCCEEDED: exceeding the cap must not fail the process", res.Outcome)
	}
	if len(res.Stdout) != 1024 {
		t.Errorf("stdout length = %d, want exactly the 1024-byte cap", len(res.Stdout))
	}
	if !res.StdoutTruncated {
		t.Error("stdout was capped but not flagged as truncated")
	}
	if len(res.Stderr) != 1024 || !res.StderrTruncated {
		t.Errorf("stderr length = %d truncated = %v, want 1024 and true", len(res.Stderr), res.StderrTruncated)
	}
}

func TestOutputUnderTheCapIsNotFlagged(t *testing.T) {
	res, err := Run(context.Background(), spec(t, "sh", "-c", "printf hello"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StdoutTruncated || res.StderrTruncated {
		t.Errorf("output under the cap was flagged as truncated: %+v", res)
	}
}

func TestTeeReceivesStdoutAndCaptureStillHappens(t *testing.T) {
	var teed strings.Builder
	s := spec(t, "sh", "-c", "printf 'line1\nline2\n'")
	s.Tee = &teed

	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if teed.String() != "line1\nline2\n" {
		t.Errorf("tee = %q", teed.String())
	}
	if res.Stdout != teed.String() {
		t.Errorf("capture (%q) and tee (%q) disagree", res.Stdout, teed.String())
	}
}

func TestWorkingDirectoryIsUsed(t *testing.T) {
	dir := t.TempDir()
	s := spec(t, "sh", "-c", "pwd")
	s.Dir = dir

	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// macOS reports /private/var for /var, so compare the resolved paths.
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(res.Stdout))
	if got != want {
		t.Errorf("working directory = %q, want %q", got, want)
	}
}

func TestMissingExecutableIsStartFailed(t *testing.T) {
	res, err := Run(context.Background(), spec(t, "aidev-no-such-program-xyz"))
	if err == nil {
		t.Fatal("a missing executable should be an error: nothing ran")
	}
	if res.Outcome != OutcomeStartFailed {
		t.Errorf("outcome = %s, want START_FAILED", res.Outcome)
	}
	if res.ExitCode != nil {
		t.Errorf("exit code = %v, want nil: the process never ran", res.ExitCode)
	}
}

// The working directory is aidev's isolation boundary, so an unusable one must be
// refused rather than defaulting to the current directory.
func TestSpecValidation(t *testing.T) {
	base := spec(t, "true")

	cases := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"no command", func(s *Spec) { s.Command = "" }, "command is empty"},
		{"no dir", func(s *Spec) { s.Dir = "" }, "working directory is required"},
		{"relative dir", func(s *Spec) { s.Dir = "relative" }, "must be absolute"},
		{"missing dir", func(s *Spec) { s.Dir = "/nonexistent/aidev/xyz" }, "not usable"},
		{"no timeout", func(s *Spec) { s.Timeout = 0 }, "timeout must be positive"},
		{"negative timeout", func(s *Spec) { s.Timeout = -time.Second }, "timeout must be positive"},
		{"no output cap", func(s *Spec) { s.MaxOutputBytes = 0 }, "max output bytes must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatalf("expected rejection mentioning %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}

			res, err := Run(context.Background(), s)
			if err == nil {
				t.Error("Run accepted an invalid spec")
			}
			if res.Outcome != OutcomeStartFailed {
				t.Errorf("outcome = %s, want START_FAILED", res.Outcome)
			}
		})
	}
}

func TestDirMustBeADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := spec(t, "true")
	s.Dir = file
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("Validate() = %v, want a 'not a directory' rejection", err)
	}
}

func TestExtraEnvIsPassedAndBaseEnvInherited(t *testing.T) {
	s := spec(t, "sh", "-c", `printf '%s|%s' "$AIDEV_TEST_VAR" "${HOME:+has-home}"`)
	s.ExtraEnv = []string{"AIDEV_TEST_VAR=present"}

	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "present|has-home" {
		t.Errorf("stdout = %q, want the extra variable set and HOME inherited", res.Stdout)
	}
}

// The rendered command goes into the audit record, so it must be readable and
// must not be something that could be fed back to a shell by accident.
func TestRender(t *testing.T) {
	cases := []struct {
		spec Spec
		want string
	}{
		{Spec{Command: "go", Args: []string{"test", "./..."}}, "go test ./..."},
		{Spec{Command: "go", Args: []string{"test", "-run", "Test A"}}, `go test -run "Test A"`},
		{Spec{Command: "tool", Args: []string{""}}, `tool ""`},
		{Spec{Command: "tool", Args: []string{`say "hi"`}}, `tool "say \"hi\""`},
	}
	for _, tc := range cases {
		if got := tc.spec.Render(); got != tc.want {
			t.Errorf("Render() = %q, want %q", got, tc.want)
		}
	}
}

func TestBoundedBufferReportsFullWrites(t *testing.T) {
	// Reporting a short write would make os/exec treat the cap as an I/O error
	// and kill the process, turning "verbose" into "failed".
	b := newBoundedBuffer(4)
	n, err := b.Write([]byte("abcdefgh"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 8 {
		t.Errorf("n = %d, want 8 (the full input length)", n)
	}
	if b.String() != "abcd" {
		t.Errorf("buffered = %q, want %q", b.String(), "abcd")
	}
	if !b.Truncated() {
		t.Error("Truncated() = false after exceeding the cap")
	}
}
