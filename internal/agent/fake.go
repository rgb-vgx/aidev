package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"aidev/internal/task"
)

// Fake is a scripted Backend for tests.
//
// It exists so that the orchestration layer can be tested deterministically and
// without a network, an API key, or a multi-second model call. Tests that need to
// prove aidev works against the real agent use the OpenCode backend explicitly;
// everything else uses this.
//
// A Fake is safe for concurrent use.
type Fake struct {
	// BackendName is reported by Name. Defaults to "fake".
	BackendName string

	// Work simulates what the agent does: a test uses it to write files into
	// req.WorkingDir. It runs before the configured outcome is applied. Writing
	// through req.WorkingDir rather than a captured path is what lets a test
	// prove the orchestrator passed the worktree and not somewhere else.
	Work func(ctx context.Context, req Request) error

	// Status is the outcome to report. Defaults to task.WorkerSucceeded.
	Status task.WorkerRunStatus

	// FailureKind accompanies a non-success Status.
	FailureKind task.FailureKind

	// Model is the backend's configured model, reported when the request names
	// none. It has the same meaning as Result.Model.
	Model string

	// Summary, SessionID, FinishReason, Tokens and Cost populate the result, so
	// a test can assert that the orchestrator persists what the backend reports.
	Summary      string
	SessionID    string
	FinishReason string
	Tokens       json.RawMessage
	Cost         *float64

	// ExitCode is reported when set.
	ExitCode *int

	Stdout string
	Stderr string

	// Delay simulates a slow agent. It respects ctx and the request timeout, so
	// a test can exercise cancellation and deadlines without a real subprocess.
	Delay time.Duration

	// Err makes Run fail at aidev's level, as a missing executable would.
	Err error

	// KnownAgents restricts which agent names ValidateAgentName accepts. Empty
	// means "accept any non-empty name", which is what most tests want.
	KnownAgents []string

	mu    sync.Mutex
	calls []Request
}

// Name implements Backend.
func (f *Fake) Name() string {
	if f.BackendName != "" {
		return f.BackendName
	}
	return "fake"
}

// Calls returns the requests received so far, for assertions about what the
// orchestrator asked for.
func (f *Fake) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.calls...)
}

// LastCall returns the most recent request and whether there was one.
func (f *Fake) LastCall() (Request, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return Request{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// Run implements Backend.
func (f *Fake) Run(ctx context.Context, req Request) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	started := time.Now().UTC()
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(f.Model)
	}
	result := Result{
		Backend:      f.Name(),
		Model:        model,
		Command:      fmt.Sprintf("fake-agent --dir %s", req.WorkingDir),
		WorkingDir:   req.WorkingDir,
		Stdout:       f.Stdout,
		Stderr:       f.Stderr,
		SessionID:    f.SessionID,
		Summary:      f.Summary,
		FinishReason: f.FinishReason,
		Tokens:       f.Tokens,
		Cost:         f.Cost,
		ExitCode:     f.ExitCode,
		StartedAt:    started,
	}
	finish := func() Result {
		result.FinishedAt = time.Now().UTC()
		result.Duration = result.FinishedAt.Sub(result.StartedAt)
		return result
	}

	if err := req.Validate(); err != nil {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = err
		return finish(), err
	}

	if f.Err != nil {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = f.Err
		return finish(), f.Err
	}

	if f.Delay > 0 {
		timer := time.NewTimer(f.Delay)
		defer timer.Stop()

		deadline := time.NewTimer(req.Timeout)
		defer deadline.Stop()

		select {
		case <-timer.C:
		case <-deadline.C:
			result.Status = task.WorkerTimedOut
			result.FailureKind = task.FailureTimeout
			result.Err = fmt.Errorf("exceeded timeout %s", req.Timeout)
			return finish(), nil
		case <-ctx.Done():
			result.Status = task.WorkerCancelled
			result.FailureKind = task.FailureCancelled
			result.Err = ctx.Err()
			return finish(), nil
		}
	}

	if f.Work != nil {
		if err := f.Work(ctx, req); err != nil {
			result.Status = task.WorkerFailed
			result.FailureKind = task.FailureAgentError
			result.Err = err
			return finish(), nil
		}
	}

	// Cancellation between the work and the result must still be reported as
	// cancelled: the orchestrator should not treat a stopped run as a success.
	if ctx.Err() != nil {
		result.Status = task.WorkerCancelled
		result.FailureKind = task.FailureCancelled
		result.Err = ctx.Err()
		return finish(), nil
	}

	result.Status = f.Status
	if result.Status == "" {
		result.Status = task.WorkerSucceeded
	}
	result.FailureKind = f.FailureKind
	if result.Status == task.WorkerSucceeded {
		result.FailureKind = task.FailureNone
		if result.ExitCode == nil {
			zero := 0
			result.ExitCode = &zero
		}
	}
	return finish(), nil
}

// ValidateAgentName implements Validator.
func (f *Fake) ValidateAgentName(_ context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("agent name is empty")
	}
	if len(f.KnownAgents) == 0 {
		return nil
	}
	for _, known := range f.KnownAgents {
		if known == name {
			return nil
		}
	}
	return fmt.Errorf("no agent named %q; available agents: %s", name, strings.Join(f.KnownAgents, ", "))
}
