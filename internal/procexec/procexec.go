// Package procexec runs external programs under the guarantees aidev requires of
// every subprocess: a deadline, cancellation that actually stops the work,
// bounded output capture, and an exit status that is classified rather than
// guessed at.
//
// Both the agent backend and the verification runner use it, because both need
// exactly these properties and duplicating them would mean two places to get
// process cleanup wrong.
package procexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Outcome classifies how a process ended. It is deliberately separate from the
// exit code: "exited 1" and "we killed it at the deadline" are different events
// that an exit code alone cannot distinguish.
type Outcome string

const (
	// OutcomeSucceeded means the process exited 0.
	OutcomeSucceeded Outcome = "SUCCEEDED"

	// OutcomeFailed means the process ran and exited non-zero.
	OutcomeFailed Outcome = "FAILED"

	// OutcomeTimedOut means the spec's own deadline elapsed.
	OutcomeTimedOut Outcome = "TIMED_OUT"

	// OutcomeCancelled means the caller's context was cancelled.
	OutcomeCancelled Outcome = "CANCELLED"

	// OutcomeStartFailed means the process never ran: a missing executable, an
	// unusable working directory, an invalid spec.
	OutcomeStartFailed Outcome = "START_FAILED"
)

// killGrace is how long a process has to exit after its process group is asked
// to terminate, before it is killed outright. Phase 0 measured `opencode run`
// exiting promptly on SIGTERM, so this is a safety net rather than the norm.
// A variable only so that a test can shorten it.
var killGrace = 5 * time.Second

// Spec describes one process to run.
type Spec struct {
	// Command is the executable. A bare name is resolved through PATH.
	Command string

	// Args are passed verbatim. There is no shell, so no quoting or expansion
	// happens to them.
	Args []string

	// Dir is the working directory. It is required and must be an existing
	// directory: for aidev this is the isolation boundary, so defaulting it to
	// the current directory would be exactly the wrong failure mode.
	Dir string

	// ExtraEnv entries ("KEY=value") are appended to the inherited environment,
	// and override a stripped variable if one is set explicitly.
	//
	// The environment is inherited because the tools aidev runs need it —
	// OpenCode reads HOME for its configuration and credentials, and compilers
	// need PATH. Inherited values are never recorded or logged.
	//
	// OTEL_* is the exception: see stripTelemetryEnv.
	ExtraEnv []string

	// Timeout bounds the run. It is required: an unbounded subprocess is the
	// failure mode this package exists to prevent.
	Timeout time.Duration

	// MaxOutputBytes bounds each captured stream independently. Required.
	MaxOutputBytes int

	// Tee receives stdout as it arrives, for callers that parse a stream while
	// it runs. Capture still happens regardless. Optional.
	Tee io.Writer
}

// Result is what happened.
type Result struct {
	// Command is the argv rendered for display and for the audit record. It
	// contains no environment, so it is safe to persist and log.
	Command string

	Dir     string
	Outcome Outcome

	// ExitCode is nil when the process produced none, which happens when it
	// could not be started or was killed by a signal.
	ExitCode *int

	Stdout          string
	StdoutTruncated bool
	Stderr          string
	StderrTruncated bool

	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration

	// Err carries the underlying failure for StartFailed, and the signal or
	// wait error otherwise. It is informational: Outcome is the decision.
	Err error

	// OrphansKilled reports that helpers were still alive when the direct
	// child had already exited, and that Run reaped them. The caller that
	// reads it can record that the run ended with cleanup, which is the one
	// interesting fact about a success that left processes behind.
	OrphansKilled bool
}

// Succeeded reports whether the process exited 0.
func (r Result) Succeeded() bool { return r.Outcome == OutcomeSucceeded }

// Validate checks a spec before anything is launched.
func (s Spec) Validate() error {
	var problems []string
	if strings.TrimSpace(s.Command) == "" {
		problems = append(problems, "command is empty")
	}
	switch {
	case strings.TrimSpace(s.Dir) == "":
		problems = append(problems, "working directory is required")
	case !filepath.IsAbs(s.Dir):
		problems = append(problems, fmt.Sprintf("working directory %q must be absolute", s.Dir))
	default:
		info, err := os.Stat(s.Dir)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("working directory %q is not usable: %v", s.Dir, err))
		case !info.IsDir():
			problems = append(problems, fmt.Sprintf("working directory %q is not a directory", s.Dir))
		}
	}
	if s.Timeout <= 0 {
		problems = append(problems, "timeout must be positive")
	}
	if s.MaxOutputBytes <= 0 {
		problems = append(problems, "max output bytes must be positive")
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid process spec: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Render returns the argv as a readable command line, for logs and audit
// records. It is never parsed back or handed to a shell.
func (s Spec) Render() string {
	parts := make([]string, 0, len(s.Args)+1)
	parts = append(parts, quote(s.Command))
	for _, a := range s.Args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

// Run executes the spec and waits for it to finish.
//
// The returned error is non-nil only when the process could not be started or
// the spec was invalid; a process that ran and failed is reported through
// Result.Outcome, because that is an outcome rather than an error in aidev.
func Run(ctx context.Context, spec Spec) (Result, error) {
	result := Result{Command: spec.Render(), Dir: spec.Dir}

	if err := spec.Validate(); err != nil {
		result.Outcome = OutcomeStartFailed
		result.Err = err
		return result, err
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	stdout := newBoundedBuffer(spec.MaxOutputBytes)
	stderr := newBoundedBuffer(spec.MaxOutputBytes)

	cmd := exec.CommandContext(runCtx, spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(stripTelemetryEnv(os.Environ()), spec.ExtraEnv...)

	// No stdin. A subprocess that reads stdin would block forever here, and
	// Phase 0 showed tools behave correctly when it is closed.
	cmd.Stdin = nil

	if spec.Tee != nil {
		cmd.Stdout = io.MultiWriter(stdout, spec.Tee)
	} else {
		cmd.Stdout = stdout
	}
	cmd.Stderr = stderr

	// Put the child in its own process group and signal the whole group, so a
	// process that spawns helpers cannot leave them behind. Phase 0 found
	// `opencode run` spawns no children, but verification commands routinely do
	// (`go test` starts compilers and test binaries).
	setProcessGroup(cmd)
	// terminatedAt is when the group was asked to stop; zero if it never was.
	// Cancel runs on exec's own goroutine, hence the atomic.
	var terminatedAt atomic.Int64
	cmd.Cancel = func() error {
		terminatedAt.Store(time.Now().UnixNano())
		return terminateGroup(cmd)
	}

	// After cancellation, allow a grace period before the process is killed
	// outright. This also bounds how long Wait can block on a child holding the
	// output pipes open.
	cmd.WaitDelay = killGrace

	result.StartedAt = time.Now().UTC()
	if err := cmd.Start(); err != nil {
		result.FinishedAt = time.Now().UTC()
		result.Duration = result.FinishedAt.Sub(result.StartedAt)
		result.Outcome = OutcomeStartFailed
		result.Err = fmt.Errorf("start %s: %w", spec.Command, err)
		result.Stderr, result.StderrTruncated = stderr.String(), stderr.Truncated()
		return result, result.Err
	}

	waitErr := cmd.Wait()

	// Reap whatever outlived the direct child — on every path, not only after a
	// cancellation. WaitDelay reaches only the direct child (already a corpse by
	// now), so an exit-0 run whose helpers held the pipes, or hid behind
	// redirected output, used to return with those helpers still running. Invariant
	// 6 has no exception for commands that succeeded.
	leftovers := groupAlive(cmd)
	var reapErr error
	if at := terminatedAt.Load(); at != 0 {
		// The group already got SIGTERM when cancellation landed. It gets the
		// rest of the same grace period, then SIGKILL.
		reapErr = killGroupAfter(cmd, time.Unix(0, at).Add(killGrace))
	} else if leftovers {
		_ = terminateGroup(cmd)
		reapErr = killGroupAfter(cmd, time.Now().Add(killGrace))
	}
	result.OrphansKilled = leftovers && reapErr == nil

	result.FinishedAt = time.Now().UTC()
	result.Duration = result.FinishedAt.Sub(result.StartedAt)
	result.Stdout, result.StdoutTruncated = stdout.String(), stdout.Truncated()
	result.Stderr, result.StderrTruncated = stderr.String(), stderr.Truncated()

	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		result.ExitCode = &code
	}

	// Order matters. A timeout and a parent cancellation can both be visible by
	// now; the caller's cancellation is the more meaningful of the two, because
	// it means a human or a shutdown asked for this to stop.
	switch {
	case ctx.Err() != nil:
		result.Outcome = OutcomeCancelled
		result.Err = ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		result.Outcome = OutcomeTimedOut
		result.Err = fmt.Errorf("exceeded timeout %s", spec.Timeout)
	case waitErr == nil:
		result.Outcome = OutcomeSucceeded
	case errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState.ExitCode() == 0:
		// The child exited 0 by itself; only helpers kept the output pipes open
		// until WaitDelay, and they have just been reaped above. Reporting this
		// as FAILED (the old behaviour) recorded a success as a failure — a
		// `npm run dev &` in verification, or a daemonising test runner, was
		// condemned for leaving the very processes this package now cleans up.
		result.Outcome = OutcomeSucceeded
	default:
		result.Outcome = OutcomeFailed
		result.Err = waitErr
	}
	return result, nil
}

// boundedBuffer keeps at most limit bytes: the first half as the head, the
// newest half as the tail, and in the middle a note of how much it dropped.
// The cap is what stops a runaway process from exhausting memory or the
// database. Keeping the tail is what keeps the reason a command failed — the
// assertion, the error line, always at the end — from being precisely the part
// the cap cuts, and Truncated is what stops a reader from mistaking a capped
// stream for a complete one.
type boundedBuffer struct {
	head, tail bytes.Buffer
	limit      int
	total      int
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

// Write always reports the full length as written. Returning a short count would
// make exec treat the cap as an I/O error and kill the process, which would turn
// "produced a lot of output" into "failed".
func (b *boundedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	b.total += written

	if room := b.limit/2 - b.head.Len(); room > 0 {
		if written <= room {
			b.head.Write(p)
			return written, nil
		}
		b.head.Write(p[:room])
		p = p[room:]
	}

	// The tail keeps the newest bytes that fit in its half; older ones fall out
	// of the front, which is the middle of the stream the cap gives up.
	b.tail.Write(p)
	if overflow := b.tail.Len() - (b.limit - b.limit/2); overflow > 0 {
		b.tail.Next(overflow)
	}
	return written, nil
}

func (b *boundedBuffer) String() string {
	head, tail := b.head.String(), b.tail.String()
	dropped := b.total - b.head.Len() - b.tail.Len()
	if dropped <= 0 {
		return head + tail
	}
	return fmt.Sprintf("%s…[truncated %d bytes]…%s", head, dropped, tail)
}

func (b *boundedBuffer) Truncated() bool { return b.total > b.head.Len()+b.tail.Len() }

// stripTelemetryEnv removes OTEL_* from an inherited environment.
//
// Without this, aidev's own exporter configuration is handed to every tool it
// runs, and an instrumented tool will publish its internal telemetry to the
// operator's backend under aidev's service name. That is not hypothetical: with
// tracing pointed at Langfuse, one task run produced 1536 spans from OpenCode's
// internals against 8 of aidev's, a ratio of 192 to 1, all filed under the
// operator's aidev project. The signal aidev exists to provide was buried under
// the agent's.
//
// aidev's own spans are created in-process, so nothing it needs is lost. A caller
// that genuinely wants to configure a child's exporter can still do it through
// ExtraEnv, which is appended afterwards and therefore wins.
func stripTelemetryEnv(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "OTEL_") {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"'") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
