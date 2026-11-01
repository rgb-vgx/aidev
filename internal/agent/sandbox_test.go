package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/task"
)

// OpenCode's permission rules confine only its own file tools: a shell command
// the agent runs writes wherever the user can (docs/research.md §7p). Run under
// bubblewrap with the filesystem read-only except the worktree, its git admin
// directory and OpenCode's own state, OpenCode 2.2.5 still ran `go test`,
// `git status` and its write tool, while `echo > outside` and `echo > $HOME/x`
// failed with "Read-only file system" (§7q). OpenCode.Sandbox runs a task that
// way when the configuration asks for it.

// usableBwrap returns the bubblewrap command, or skips: a runner without it,
// or one whose kernel refuses unprivileged user namespaces, cannot run these.
func usableBwrap(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is not installed")
	}
	if out, err := exec.Command(path, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap cannot run here: %v: %s", err, out)
	}
	return path
}

// sandboxFake is an opencode stand-in that answers the version and listing
// probes, then, as the agent, tries to write in each place a run may or may not
// write, and finishes the way a completed OpenCode 2 run does.
func sandboxFake(t *testing.T, outside, extra string) string {
	t.Helper()
	dir := t.TempDir()
	command := filepath.Join(dir, "opencode")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo 'opencode v2.2.5'; exit 0; fi\n" +
		"if [ \"$1\" = debug ] && [ \"$2\" = config ]; then echo '[]'; exit 0; fi\n" +
		"echo in > inside.txt\n" +
		"echo out > " + shellQuote(filepath.Join(outside, "out.txt")) + " 2>/dev/null\n" +
		"echo home > \"$HOME/.aidev-sandbox-probe\" 2>/dev/null\n" +
		"[ -n \"$OPENCODE_DB\" ] && echo db > \"$(dirname \"$OPENCODE_DB\")/db-dir-writable\" 2>/dev/null\n" +
		"echo w > " + shellQuote(filepath.Join(extra, "w.txt")) + " 2>/dev/null\n" +
		"admin=$(git rev-parse --absolute-git-dir 2>/dev/null) && echo a > \"$admin/aidev-sandbox-probe\" 2>/dev/null\n" +
		emit(fixtureStepStart, fixtureText, fixtureStepFinishStop) +
		"exit 0\n"
	if err := os.WriteFile(command, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return command
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// sandboxWorktree makes a real git worktree, as a task has, so the run's git
// admin directory is a real one outside the worktree.
func sandboxWorktree(t *testing.T) (worktree, admin string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	worktree = filepath.Join(t.TempDir(), "TASK-000001-a1")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", worktree, "-b", "aidev/TASK-000001").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	return worktree, strings.TrimSpace(string(out))
}

func TestSandboxedRunWritesOnlyWhereItMay(t *testing.T) {
	bwrap := usableBwrap(t)
	outside, extra, dbDir := t.TempDir(), t.TempDir(), t.TempDir()
	worktree, admin := sandboxWorktree(t)
	home, _ := os.UserHomeDir()
	t.Cleanup(func() { os.Remove(filepath.Join(home, ".aidev-sandbox-probe")) })

	backend := NewOpenCode(sandboxFake(t, outside, extra), "")
	backend.DBDir = dbDir
	backend.Sandbox = bwrap
	backend.SandboxWritable = []string{extra}
	req := openCodeRequest(t)
	req.WorkingDir = worktree

	res, err := backend.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerSucceeded {
		t.Errorf("status = %s (%v), want SUCCEEDED", res.Status, res.Err)
	}
	for _, c := range []struct {
		path string
		want bool
		what string
	}{
		{filepath.Join(worktree, "inside.txt"), true, "the worktree"},
		{filepath.Join(dbDir, "db-dir-writable"), true, "the task's OpenCode database directory"},
		{filepath.Join(extra, "w.txt"), true, "a configured writable directory"},
		{filepath.Join(admin, "aidev-sandbox-probe"), true, "the worktree's git admin directory"},
		{filepath.Join(outside, "out.txt"), false, "a directory outside the worktree"},
		{filepath.Join(home, ".aidev-sandbox-probe"), false, "the home directory"},
	} {
		if got := exists(c.path); got != c.want {
			t.Errorf("write to %s (%s): happened = %v, want %v", c.what, c.path, got, c.want)
		}
	}
}

// Without a sandbox nothing changes: the same shell write lands outside, which
// is what the sandbox is for.
func TestUnsandboxedRunIsUnchanged(t *testing.T) {
	outside, extra := t.TempDir(), t.TempDir()
	backend := NewOpenCode(sandboxFake(t, outside, extra), "")
	home, _ := os.UserHomeDir()
	t.Cleanup(func() { os.Remove(filepath.Join(home, ".aidev-sandbox-probe")) })
	if _, err := backend.Run(context.Background(), openCodeRequest(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !exists(filepath.Join(outside, "out.txt")) {
		t.Error("an unsandboxed run could not write outside its worktree; the fake or the test is wrong")
	}
}

// A sandbox that cannot be started is not a reason to run unconfined: the run
// does not start.
func TestMissingSandboxIsAStartupFailure(t *testing.T) {
	outside, extra := t.TempDir(), t.TempDir()
	backend := NewOpenCode(sandboxFake(t, outside, extra), "")
	backend.Sandbox = filepath.Join(t.TempDir(), "no-such-bwrap")
	req := openCodeRequest(t)
	res, err := backend.Run(context.Background(), req)
	if err == nil {
		t.Fatal("Run returned no error with a sandbox command that does not exist")
	}
	if res.Status != task.WorkerFailed || res.FailureKind != task.FailureStartup {
		t.Errorf("status = %s/%s, want FAILED/STARTUP", res.Status, res.FailureKind)
	}
	if exists(filepath.Join(req.WorkingDir, "inside.txt")) || exists(filepath.Join(outside, "out.txt")) {
		t.Error("the agent ran although its sandbox could not be started")
	}
}
