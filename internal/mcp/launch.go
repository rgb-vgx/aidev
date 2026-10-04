package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"aidev/internal/procexec"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Launcher starts a run for t and reports how to wait for it.
//
// wait blocks until the run's driver is done — the child process exited, or an
// in-process orchestrator call returned — and carries that driver's error, if
// any. It says nothing about what the run did: the database is the only source
// of truth about the task, because a child that exits non-zero may simply be a
// task that failed (research C2). logPath is the file the driver's stderr went
// to, empty when there is none.
type Launcher func(ctx context.Context, orch *worker.Orchestrator, t task.Task) (wait func() error, logPath string, err error)

// runLogDirname is where a detached run's stderr log lives, under
// workspace_root. It sits beside the worktrees because that is the one
// workspace aidev owns on disk, and it is where an operator looks after a run
// dies.
const runLogDirname = "run-logs"

// runLogTailBytes is how much of a run's stderr comes back in an error. The
// tail is what the operator needs to see why the child stopped; a bounded
// number keeps a runaway log from flooding a tool response.
const runLogTailBytes = 2000

// detachedLaunch starts `aidev task run <ref>` as a separate process in its own
// session, so the run outlives this server (research C2).
//
// The child is the ordinary CLI command: it applies its own total deadline —
// the task's timeout plus the verification budget plus a margin — which is how
// invariant 6 holds for a process this server no longer waits on. stdout goes
// to /dev/null because in this server stdout is the JSON-RPC channel and
// nothing else may touch it (invariant 3); stderr goes to a log file of its
// own, so a child that died can be diagnosed after the fact. If this process
// dies, the child keeps its lease alive; if the child dies, the lease runs out
// and `aidev task recover` finds it (research C1).
func (s *Server) detachedLaunch(_ context.Context, orch *worker.Orchestrator, t task.Task) (func() error, string, error) {
	exe := s.exePath
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, "", fmt.Errorf("locate the aidev executable to start the run for %s: %w", t.Identifier(), err)
		}
	}

	logDir := filepath.Join(orch.Config.WorkspaceRoot, runLogDirname)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create the run log directory %s: %w", logDir, err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s-%d.log", t.Identifier(), time.Now().UnixNano()))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("create the run log %s: %w", logPath, err)
	}

	cmd, closeStdout, err := newRunChild(exe, t.Identifier(), logFile)
	if err != nil {
		_ = logFile.Close()
		_ = os.Remove(logPath)
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		closeStdout()
		_ = logFile.Close()
		_ = os.Remove(logPath)
		return nil, "", fmt.Errorf("start the run process for %s: %w", t.Identifier(), err)
	}
	// The child holds its own copies of both descriptors; keeping ours open
	// would leak a file per run. cmd.Wait still works: it only reaps the
	// process.
	closeStdout()
	_ = logFile.Close()
	return cmd.Wait, logPath, nil
}

// inProcessLaunch runs the orchestrator inside this server instead of spawning
// anything. The tests use it: they built the orchestrator themselves and there
// is no binary to start. Production keeps detachedLaunch, so a run is never
// bound to the lifetime of the session that asked for it.
func inProcessLaunch(ctx context.Context, orch *worker.Orchestrator, t task.Task) (func() error, string, error) {
	wait := func() error {
		_, err := orch.RunTask(ctx, t.ID.String())
		return err
	}
	return wait, "", nil
}

// newRunChild builds the command for a detached run, isolated from this server
// on every inherited channel. It returns the command without starting it, so a
// test can assert the isolation without spawning anything; the second result
// releases the child's stdout file, which must happen after Start.
func newRunChild(exe, ref string, logFile *os.File) (*exec.Cmd, func(), error) {
	// Opened rather than left nil so the redirect is visible in the code: this
	// server's stdout belongs to the protocol, and the child must get the null
	// device rather than a share in it.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s for the run process's stdout: %w", os.DevNull, err)
	}
	cmd := exec.Command(exe, "task", "run", ref)
	cmd.Stdout = devNull
	cmd.Stderr = logFile
	// Stdin stays nil, which exec wires to the null device: the child never
	// reads this server's stdin, which in this mode is the JSON-RPC channel.
	procexec.Detach(cmd)
	return cmd, func() { _ = devNull.Close() }, nil
}

// awaitOutcome waits for a run's driver to finish and decides whether how it
// stopped is an error.
//
// The driver's own error is not the answer — see classifyRun. What ends the
// wait is either the driver finishing or the server shutting down: the watcher
// stops in the second case instead of holding the connection open for the
// length of a run that outlives it anyway.
func (s *Server) awaitOutcome(ctx context.Context, st *store.Store, taskID uuid.UUID, wait func() error, logPath string) error {
	waitCh := make(chan error, 1)
	go func() { waitCh <- wait() }()

	select {
	case <-ctx.Done():
		// The server is stopping. Stop watching rather than wait: a detached
		// child keeps running with its own deadline and lease, and waiting for
		// it here would hold the client's connection open for exactly as long
		// as the run — the opposite of why it was detached (research C2).
		return ctx.Err()
	case waitErr := <-waitCh:
		return classifyRun(ctx, st, taskID, waitErr, logPath)
	}
}

// classifyRun turns "the driver stopped" into the tool's error, if any, by
// asking the database what the run actually did.
//
// A recorded ending answers it: a task that ran and failed exits the child
// non-zero too, and an approval gate exits zero, so the exit code alone would
// report both as crashes. The driver's error only matters when the task never
// reached an ending — then the child died mid-run, and the tail of its log
// says why.
func classifyRun(ctx context.Context, st *store.Store, taskID uuid.UUID, waitErr error, logPath string) error {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	t, err := st.GetTask(readCtx, taskID)
	if err != nil {
		if waitErr != nil {
			return fmt.Errorf("the run stopped and %s could not be re-read: %v: %w", taskID, waitErr, err)
		}
		return fmt.Errorf("the run stopped and %s could not be re-read: %w", taskID, err)
	}

	if t.Status.Terminal() || t.Status == task.StatusWaitingApproval {
		// One error survives a recorded ending: a refusal to run at all, from
		// the race where the task was claimed elsewhere between our check and
		// the run's own.
		if waitErr != nil && errors.Is(waitErr, worker.ErrNotRunnable) {
			return waitErr
		}
		return nil
	}

	msg := fmt.Sprintf("the run stopped while %s was still %s", t.Identifier(), t.Status)
	if waitErr != nil {
		msg = fmt.Sprintf("%s: %v", msg, waitErr)
	}
	if tail := tailFile(logPath, runLogTailBytes); tail != "" {
		msg += "; stderr of the run process (last bytes):\n" + tail
	}
	return errors.New(msg)
}

// tailFile returns the last max bytes of a file, or "" if it cannot be read.
// Reading from the end keeps the bounded read independent of how large the file
// grew: the end is where the explanation of a crash is.
func tailFile(path string, max int64) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	offset := int64(0)
	if info.Size() > max {
		offset = info.Size() - max
	}
	buf := make([]byte, info.Size()-offset)
	// A short read means the file changed under us; whatever arrived is still
	// worth reporting, and there is nothing useful to do about the rest.
	n, _ := f.ReadAt(buf, offset)
	return string(buf[:n])
}
