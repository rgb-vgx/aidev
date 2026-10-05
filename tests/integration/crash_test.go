package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"aidev/internal/task"
	"aidev/internal/worker"
)

// Crash consistency (production review, 2026-10-04): a real `aidev task run`
// process is killed with SIGKILL at each boundary of a run, and the state it
// leaves must be one recovery can take back — the task cancelled, the partial
// work retained, and the branch holding nothing that was not recorded.
//
// The other recovery tests seed the state a kill would leave; these produce
// it. The agent is a shell script standing in for opencode, so the run is the
// real binary end to end.

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// aidevBinary builds the CLI once per test binary.
func aidevBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "aidev-crash-bin-")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "aidev")
		build := exec.Command("go", "build", "-o", binPath, "./cmd/aidev")
		build.Dir = filepath.Join("..", "..")
		if out, err := build.CombinedOutput(); err != nil {
			binErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

// crashRig is a harness plus a configuration for the subprocess: the same
// database and workspace, and a fake opencode whose run writes marker.txt,
// waits agentDelay, then reports a finished session.
type crashRig struct {
	*harness
	conf    string
	pidFile string
}

func newCrashRig(t *testing.T, agentDelay time.Duration) *crashRig {
	t.Helper()
	h := newHarness(t, nil)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "agent.pid")

	opencode := filepath.Join(dir, "opencode")
	stub := fmt.Sprintf(`#!/bin/sh
if [ "$1" = agent ] && [ "$2" = list ]; then printf 'build (primary)\n'; exit 0; fi
echo $$ > %q
wt=""
while [ $# -gt 0 ]; do
  if [ "$1" = --dir ]; then wt="$2"; shift; fi
  shift
done
echo done > "$wt/marker.txt"
sleep %d
printf '%%s\n' '{"type":"text","sessionID":"ses_crash","part":{"type":"text","text":"Done."}}'
printf '%%s\n' '{"type":"step_finish","sessionID":"ses_crash","part":{"reason":"stop","tokens":{"input":1,"output":1}}}'
`, pidFile, int(agentDelay.Seconds()))
	if err := os.WriteFile(opencode, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(map[string]any{
		"database":       map[string]any{"url": os.Getenv(envDatabaseURL)},
		"workspace_root": h.workspace,
		"log_level":      "error",
		"agent":          map[string]any{"opencode": map[string]any{"command": opencode}},
	})
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "conf.json")
	if err := os.WriteFile(conf, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &crashRig{harness: h, conf: conf, pidFile: pidFile}
	t.Cleanup(r.killAgent)
	return r
}

// killAgent stops the fake agent a killed run leaves behind: SIGKILL does
// not reach the agent's own process group.
func (r *crashRig) killAgent() {
	raw, err := os.ReadFile(r.pidFile)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err == nil && pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// start runs `aidev task run ref` as its own process.
func (r *crashRig) start(t *testing.T, ref string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(aidevBinary(t), "task", "run", ref)
	cmd.Env = append(os.Environ(), "AIDEV_CONFIG="+r.conf)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start aidev task run: %v", err)
	}
	return cmd
}

// kill sends SIGKILL, the crash no code of aidev's can react to.
func kill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *crashRig) status(t *testing.T, id task.Task) task.Status {
	t.Helper()
	got, err := r.store.GetTask(context.Background(), id.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status
}

// expireLeases does what waiting out the lease would, without the wait.
func expireLeases(t *testing.T, taskID string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), os.Getenv(envDatabaseURL))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(),
		`UPDATE task_attempts SET lease_expires_at = now() - interval '1 minute' WHERE task_id = $1`, taskID); err != nil {
		t.Fatal(err)
	}
}

// holdRowLock opens a transaction holding a row lock until release is called,
// to freeze the run at a precise point.
func holdRowLock(t *testing.T, query string, args ...any) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv(envDatabaseURL))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if err := tx.QueryRow(ctx, query, args...).Scan(&one); err != nil {
		t.Fatalf("take row lock: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			_ = conn.Close(ctx)
		})
	}
	t.Cleanup(release)
	return release
}

// recoverAfterCrash checks what every crash must leave: a task recovery can
// take back, cancelled, with its worktree retained on disk.
func (r *crashRig) recoverAfterCrash(t *testing.T, created task.Task) {
	t.Helper()
	if s := r.status(t, created); s.Terminal() {
		t.Fatalf("status after the kill = %s; a killed run cannot have recorded an ending", s)
	}
	expireLeases(t, created.ID.String())
	recovered, err := r.orchestrator.Recover(r.ctx, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Ref != created.Ref {
		t.Fatalf("Recover = %+v, want exactly %s", recovered, created.Ref)
	}
	if s := r.status(t, created); s != task.StatusCancelled {
		t.Errorf("status after recovery = %s, want CANCELLED", s)
	}
	attempt, err := r.store.LatestAttempt(r.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != task.AttemptCancelled {
		t.Errorf("attempt = %s, want CANCELLED", attempt.Status)
	}
	wt, err := r.store.GetWorktreeByAttempt(r.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree = %s, want RETAINED", wt.Status)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "marker.txt")); err != nil {
		t.Errorf("the agent's work is gone from the retained worktree: %v", err)
	}
}

func (r *crashRig) branchHasMarker(ref string) bool {
	return strings.Contains(r.branchFiles("aidev/"+ref), "marker.txt")
}

func verifySlowly(seconds int) func(*worker.CreateTaskInput) {
	return func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{shell(fmt.Sprintf("sleep %d && test -f marker.txt", seconds))}
	}
}

func TestCrashWhileTheAgentRuns(t *testing.T) {
	r := newCrashRig(t, 10*time.Second)
	created := r.createTask(nil)
	cmd := r.start(t, created.Ref)
	waitFor(t, "the agent to start", func() bool { _, err := os.Stat(r.pidFile); return err == nil })

	kill(t, cmd)
	r.recoverAfterCrash(t, created)
	if r.branchHasMarker(created.Ref) {
		t.Error("the branch holds work from a run killed while its agent ran")
	}
}

func TestCrashWhileVerificationRuns(t *testing.T) {
	r := newCrashRig(t, 0)
	created := r.createTask(verifySlowly(10))
	cmd := r.start(t, created.Ref)
	waitFor(t, "VERIFYING", func() bool { return r.status(t, created) == task.StatusVerifying })

	kill(t, cmd)
	r.recoverAfterCrash(t, created)
	if r.branchHasMarker(created.Ref) {
		t.Error("the branch holds work from a run killed during verification")
	}
}

// Killed while waiting for the fence to record the success: the commit is
// taken under that fence, so nothing reached the branch.
func TestCrashBeforeTheSuccessIsRecordedLeavesTheBranchAlone(t *testing.T) {
	r := newCrashRig(t, 0)
	created := r.createTask(verifySlowly(1))
	cmd := r.start(t, created.Ref)
	waitFor(t, "VERIFYING", func() bool { return r.status(t, created) == task.StatusVerifying })

	release := holdRowLock(t, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, created.ID)
	attempt, err := r.store.LatestAttempt(r.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the checks to finish", func() bool {
		runs, err := r.store.ListVerificationRuns(r.ctx, attempt.ID)
		return err == nil && len(runs) > 0
	})
	time.Sleep(300 * time.Millisecond) // the run is now queued behind the lock

	kill(t, cmd)
	release()
	if r.branchHasMarker(created.Ref) {
		t.Error("the branch moved although the run died before it held the fence")
	}
	r.recoverAfterCrash(t, created)
}

// The narrowest window left: the branch has moved inside the success
// transaction and the process dies before that transaction commits. The
// database rolls back, so the task is not SUCCEEDED and recovery cancels it;
// the branch keeps the verified commit, which is the documented outcome — a
// commit the record does not claim, never a claim without its commit.
func TestCrashBetweenBranchUpdateAndCommitIsRecoverable(t *testing.T) {
	r := newCrashRig(t, 0)
	created := r.createTask(verifySlowly(1))
	cmd := r.start(t, created.Ref)
	waitFor(t, "VERIFYING", func() bool { return r.status(t, created) == task.StatusVerifying })

	attempt, err := r.store.LatestAttempt(r.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	release := holdRowLock(t, `SELECT 1 FROM worktrees WHERE attempt_id = $1 FOR UPDATE`, attempt.ID)
	waitFor(t, "the branch to move", func() bool { return r.branchHasMarker(created.Ref) })

	kill(t, cmd)
	release()
	r.recoverAfterCrash(t, created)
	if !r.branchHasMarker(created.Ref) {
		t.Error("the verified commit vanished from the branch")
	}
	if contains(r.eventTypes(created.ID), "task.succeeded") {
		t.Error("a success was recorded by a run that died before its transaction committed")
	}
}
