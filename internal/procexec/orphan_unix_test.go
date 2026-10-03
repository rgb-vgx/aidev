//go:build unix

package procexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func shortenKillGrace(t *testing.T, d time.Duration) {
	t.Helper()
	previous := killGrace
	killGrace = d
	t.Cleanup(func() { killGrace = previous })
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse pid from %q: %v", raw, err)
	}
	return pid
}

// A run that exits 0 while helpers are still going is a success, not a failure —
// and the helpers must not outlive Run. WaitDelay used to make this shape
// FAILED (`npm run dev &`, a daemonising test runner), and the group was only
// reaped when a cancellation landed, so the very processes cleanup existed for
// were the ones left running (invariant 6).
func TestExitZeroWithHelpersIsSucceededAndGroupIsReaped(t *testing.T) {
	shortenKillGrace(t, 300*time.Millisecond)

	dir := t.TempDir()
	pidfile := filepath.Join(dir, "sh.pid")
	marker := filepath.Join(dir, "helper-finished")

	// $$ is the shell's own pid, which setProcessGroup makes the pgid. The
	// helper inherits stdout, so Wait cannot finish until WaitDelay fires and
	// cuts the pipes — the exact shape that used to be misclassified.
	s := spec(t, "sh", "-c",
		"echo $$ > '"+pidfile+"'; (sleep 1; touch '"+marker+"') & echo started")
	s.Dir = dir
	s.Timeout = 10 * time.Second

	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeSucceeded {
		t.Errorf("outcome = %s, want SUCCEEDED: exit 0 with helpers is a success", res.Outcome)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil: reaped helpers are not an error", res.Err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "started") {
		t.Errorf("stdout = %q, want it to contain %q", res.Stdout, "started")
	}
	if !res.OrphansKilled {
		t.Error("OrphansKilled = false although helpers were running when Wait returned")
	}

	// The group must be gone outright: kill(-pgid, 0) reports ESRCH.
	pgid := readPid(t, pidfile)
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still exists after Run returned (kill -0: %v)", pgid, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// And the helper must never get to act.
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the background helper survived Run and wrote the marker")
	}
}

// A helper behind redirected output holds no pipe, so Wait returns promptly with
// a clean exit — and used to leave the helper running with no signal sent at all.
// The group probe, not WaitDelay, is what catches this case.
func TestRedirectedBackgroundHelperIsReaped(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "helper-finished")

	s := spec(t, "sh", "-c",
		"(sleep 1; touch '"+marker+"') >/dev/null 2>&1 & echo started")
	s.Dir = dir
	s.Timeout = 10 * time.Second

	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeSucceeded {
		t.Errorf("outcome = %s, want SUCCEEDED", res.Outcome)
	}
	if !res.OrphansKilled {
		t.Error("OrphansKilled = false although a redirected helper was running")
	}

	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the redirected helper survived Run and wrote the marker")
	}
}

// The probe must not fire on a clean run: a command that leaves nothing behind
// is not credited with a cleanup it did not perform.
func TestCleanRunReportsNoOrphans(t *testing.T) {
	res, err := Run(context.Background(), spec(t, "sh", "-c", "echo done"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeSucceeded {
		t.Errorf("outcome = %s, want SUCCEEDED", res.Outcome)
	}
	if res.OrphansKilled {
		t.Error("OrphansKilled = true on a run that left nothing behind")
	}
}
