//go:build unix

package procexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SIGTERM is a request. A process group that ignores it must still be gone once
// the grace period is over: aidev promises that a cancelled or timed-out run
// leaves nothing behind, and a verification command's helpers (go test's test
// binaries) are exactly what might ignore the request. Today the escalation kills
// only the direct child, so a helper that ignores SIGTERM outlives the run.
func TestAGroupThatIgnoresSIGTERMIsKilledAfterTheGrace(t *testing.T) {
	previous := killGrace
	killGrace = 300 * time.Millisecond
	t.Cleanup(func() { killGrace = previous })

	dir := t.TempDir()
	marker := filepath.Join(dir, "child-still-running")

	// Ignored signals are inherited, so both the shell and its background child
	// ignore SIGTERM. The child acts well after the grace period has ended.
	s := spec(t, "sh", "-c",
		"trap '' TERM; (sleep 3; touch '"+marker+"') & sleep 30")
	s.Dir = dir
	s.Timeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	res, err := Run(ctx, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeCancelled {
		t.Errorf("outcome = %s, want CANCELLED", res.Outcome)
	}
	if took := time.Since(started); took > 2500*time.Millisecond {
		t.Errorf("Run took %s; the grace period is %s, so the group was not killed when it ended", took, killGrace)
	}

	// Wait past the point where the surviving child would have acted.
	time.Sleep(time.Until(started.Add(3500 * time.Millisecond)))
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child that ignored SIGTERM survived the grace period: only the direct child was killed")
	}
}

// The grace period is for the whole group. A helper that handles SIGTERM by
// cleaning up must be allowed to finish even when the direct child exits at once;
// SIGKILL is for what is still running when the grace period is over.
func TestAGroupMemberGetsTheWholeGracePeriod(t *testing.T) {
	previous := killGrace
	killGrace = 2 * time.Second
	t.Cleanup(func() { killGrace = previous })

	dir := t.TempDir()
	marker := filepath.Join(dir, "cleaned-up")

	// The shell dies on SIGTERM at once; its child traps it and takes half a
	// second to clean up. The child's stdout is detached, so the pipe does not
	// hold Wait open.
	s := spec(t, "sh", "-c",
		"(trap 'sleep 0.5; touch \""+marker+"\"; exit 0' TERM; while :; do sleep 0.05; done) >/dev/null 2>&1 & sleep 30")
	s.Dir = dir

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	if _, err := Run(ctx, s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("a group member was killed before it could finish handling SIGTERM")
}
