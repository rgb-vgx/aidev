package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"aidev/internal/procexec"
	"aidev/internal/task"
)

// Defaults for the OpenCode backend.
const (
	// OpenCodeName is how this backend identifies itself in audit records.
	OpenCodeName = "opencode"

	// DefaultOpenCodeCommand is the executable name, resolved through PATH.
	DefaultOpenCodeCommand = "opencode"

	// agentListTimeout bounds the agent-name lookup. It is short because the
	// call is local and a hung lookup should not delay a task.
	agentListTimeout = 30 * time.Second
)

// agentListTTL is how long a successful agent-name lookup is reused. Task
// creation validates the agent name on every task, and each lookup spawns the
// OpenCode CLI, while the list itself changes only when OpenCode is
// reconfigured — rarer than task creation. A burst of tasks therefore pays the
// spawn once, not once per task. A variable so tests can expire it.
var agentListTTL = 5 * time.Minute

// OpenCode runs tasks through the OpenCode CLI.
//
// Every flag used here was verified against the installed version (1.18.30)
// during Phase 0 and is recorded in docs/research.md; none was taken from
// documentation or guessed at. The invocation is:
//
//	opencode run --dir <worktree> --format json [--agent X] [-m model] [-s session] -- <prompt>
//
// `--dir` is what confines the agent to the worktree. `--format json` produces
// the newline-delimited event stream the result is built from. `--` guarantees a
// prompt beginning with a dash cannot be read as a flag.
type OpenCode struct {
	// Command is the executable. Empty means DefaultOpenCodeCommand.
	Command string

	// Model is passed as -m when set. Empty lets OpenCode choose, which works
	// with no credentials configured (docs/research.md §2.10).
	Model string

	// DBDir, when set, gives every task its own OpenCode database there
	// (<ref>.sqlite, through OPENCODE_DB). Concurrent runs on OpenCode's one
	// shared store die at random with "database is locked" (docs/research.md
	// §7k), so tasks running side by side need their own. Every attempt of a
	// task uses the same file: a session can only be continued from the
	// database it was created in, which is what automatic retry relies on.
	// Empty keeps OpenCode's shared store.
	DBDir string

	// agents holds the last successful lookup, guarded by agentsMu, which is
	// also held for the duration of a lookup so that concurrent validations
	// share one spawn instead of racing to start their own.
	agentsMu    sync.Mutex
	agents      []string
	agentsFresh time.Time
}

// NewOpenCode returns a backend using the given command, or the default when it
// is empty.
func NewOpenCode(command, model string) *OpenCode {
	if strings.TrimSpace(command) == "" {
		command = DefaultOpenCodeCommand
	}
	return &OpenCode{Command: strings.TrimSpace(command), Model: strings.TrimSpace(model)}
}

// Name implements Backend.
func (o *OpenCode) Name() string { return OpenCodeName }

func (o *OpenCode) command() string {
	if strings.TrimSpace(o.Command) == "" {
		return DefaultOpenCodeCommand
	}
	return o.Command
}

// effectiveModel is the model that will actually run: the request's when it
// names one, otherwise the backend's own configured model.
func (o *OpenCode) effectiveModel(req Request) string {
	if model := strings.TrimSpace(req.Model); model != "" {
		return model
	}
	return strings.TrimSpace(o.Model)
}

// buildArgs assembles the argv. It is separate from Run so that a test can assert
// the command line without launching anything.
func (o *OpenCode) buildArgs(req Request) []string {
	args := []string{"run", "--dir", req.WorkingDir, "--format", "json"}

	if agent := strings.TrimSpace(req.Agent); agent != "" {
		args = append(args, "--agent", agent)
	}
	if model := o.effectiveModel(req); model != "" {
		args = append(args, "-m", model)
	}
	if session := strings.TrimSpace(req.SessionID); session != "" {
		args = append(args, "-s", session)
	}

	// Everything after -- is the message, so a prompt starting with a dash is
	// safe (docs/research.md §7b).
	return append(args, "--", req.Prompt)
}

// TaskDBPath is where a task's OpenCode database lives under dir, or "" when
// ref cannot name a file safely. CLI cleanup uses it too, so the two cannot
// disagree about the name.
func TaskDBPath(dir, ref string) string {
	ref = strings.TrimSpace(ref)
	if dir == "" || ref == "" || !safeRef.MatchString(ref) {
		return ""
	}
	return filepath.Join(dir, ref+".sqlite")
}

var safeRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// taskDB returns the database a task's runs use, creating its directory, or
// "" to use the shared store (no DBDir, or no task reference to name it by).
func (o *OpenCode) taskDB(ref string) (string, error) {
	path := TaskDBPath(o.DBDir, ref)
	if path == "" {
		return "", nil
	}
	if err := os.MkdirAll(o.DBDir, 0o700); err != nil {
		return "", fmt.Errorf("create the opencode database directory %s: %w", o.DBDir, err)
	}
	return path, nil
}

// agentFallback matches the warning OpenCode prints when it does not recognise
// the requested agent. It then runs its default agent and exits 0
// (docs/research.md §2.5), so this text is the only evidence that the task did
// not run with the agent aidev asked for.
var agentFallback = regexp.MustCompile(`agent "([^"]*)" not found`)

// Run implements Backend.
func (o *OpenCode) Run(ctx context.Context, req Request) (Result, error) {
	result := Result{
		Backend:    o.Name(),
		WorkingDir: req.WorkingDir,
		StartedAt:  time.Now().UTC(),
	}

	if err := req.Validate(); err != nil {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = err
		result.FinishedAt = time.Now().UTC()
		return result, err
	}

	args := o.buildArgs(req)
	result.Model = o.effectiveModel(req)
	scanner := newEventScanner()

	var extraEnv []string
	if db, err := o.taskDB(req.TaskRef); err != nil {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = err
		result.FinishedAt = time.Now().UTC()
		return result, err
	} else if db != "" {
		extraEnv = append(extraEnv, "OPENCODE_DB="+db)
	}

	spec := procexec.Spec{
		Command:        o.command(),
		Args:           args,
		Dir:            req.WorkingDir,
		Timeout:        req.Timeout,
		MaxOutputBytes: req.MaxOutputBytes,
		// The agent executes instructions we do not control; it must not see
		// aidev's own configuration (AIDEV_CONFIG → database URL).
		DropEnv:  []string{"AIDEV_"},
		ExtraEnv: extraEnv,
		// The agent's commands may start applications in their own session
		// (a test that runs an app under xvfb-run); stopping the run stops
		// those too (TASK-000092).
		FollowSessions: true,
		// Parse while the process runs, so events survive even when the capture
		// cap discards the tail of a very chatty run.
		Tee: scanner,
	}

	proc, runErr := procexec.Run(ctx, spec)
	scanner.Close()
	events := scanner.Transcript()

	result.Command = proc.Command
	result.ExitCode = proc.ExitCode
	result.Stdout, result.StdoutTruncated = proc.Stdout, proc.StdoutTruncated
	result.Stderr, result.StderrTruncated = proc.Stderr, proc.StderrTruncated
	result.SessionID = events.SessionID
	result.Summary = events.Summary
	result.FinishReason = events.FinishReason
	result.ToolCalls = events.ToolCalls
	result.StartedAt = proc.StartedAt
	result.FinishedAt = proc.FinishedAt
	result.Duration = proc.Duration

	if events.Usage.Steps > 0 {
		if encoded, err := json.Marshal(events.Usage); err == nil {
			result.Tokens = encoded
		}
	}
	if events.Cost > 0 {
		cost := events.Cost
		result.Cost = &cost
	}

	// A process that could not start is aidev's problem to report, not an
	// outcome to record as the agent's failure.
	if proc.Outcome == procexec.OutcomeStartFailed {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = startupError(o.command(), proc, runErr)
		return result, result.Err
	}
	if runErr != nil {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = runErr
		return result, runErr
	}

	result.Status, result.FailureKind, result.Err = classify(proc, events, req)
	return result, nil
}

// classify decides the outcome from the process result and the event stream.
//
// The order is deliberate. Cancellation and timeout come first because they are
// facts about aidev's own decision to stop. The agent's error events are checked
// before the exit code, because Phase 0 showed the exit code alone is not
// trustworthy: an unknown agent name exits 0 after silently falling back.
func classify(proc procexec.Result, events transcript, req Request) (task.WorkerRunStatus, task.FailureKind, error) {
	switch proc.Outcome {
	case procexec.OutcomeCancelled:
		return task.WorkerCancelled, task.FailureCancelled, proc.Err
	case procexec.OutcomeTimedOut:
		return task.WorkerTimedOut, task.FailureTimeout,
			fmt.Errorf("opencode exceeded the task timeout of %s", req.Timeout)
	}

	// The requested agent was not used. Reporting success here would put a
	// truthful-looking record in the database about a run that did not happen as
	// asked.
	if m := agentFallback.FindStringSubmatch(proc.Stderr); m != nil {
		return task.WorkerFailed, task.FailureStartup, fmt.Errorf(
			"opencode did not recognise agent %q and fell back to its default, so the task did not run as requested",
			m[1])
	}

	if len(events.Errors) > 0 {
		return task.WorkerFailed, task.FailureAgentError,
			fmt.Errorf("opencode reported: %s", strings.Join(events.Errors, "; "))
	}

	if !proc.Succeeded() {
		code := "unknown"
		if proc.ExitCode != nil {
			code = fmt.Sprintf("%d", *proc.ExitCode)
		}
		detail := firstLine(proc.Stderr)
		if detail == "" {
			detail = "no diagnostic output"
		}
		return task.WorkerFailed, task.FailureAgentExit,
			fmt.Errorf("opencode exited %s: %s", code, detail)
	}

	// Exit 0 with no events at all means the process ran but produced nothing.
	// That is not a success worth passing on: there is no session, no summary,
	// and no evidence the agent did anything.
	if events.Lines == 0 {
		return task.WorkerFailed, task.FailureUnknown,
			fmt.Errorf("opencode exited 0 but produced no events; stderr: %s", firstLine(proc.Stderr))
	}

	// A session that finished ends with reason "stop". Anything else means the
	// model's turn ended while work was still outstanding — it asked for a tool and
	// the loop never came back ("tool-calls"), or it ran out of room ("length").
	// Measured in TASK-000039: four steps, exit 0, no file changed, last reason
	// "tool-calls", recorded as a clean success until verification disagreed. A
	// rejected permission produces the same shape (docs/research.md 7g).
	if reason, cut := cutShort[events.FinishReason]; cut {
		// Which of the two it was is in the stream, so say it. "The agent
		// stopped halfway" and "the agent was blocked" read identically in the
		// record and have nothing in common as problems: one is a model to
		// change, the other is a task asking for something outside the worktree.
		if events.Refusals > 0 {
			return task.WorkerFailed, task.FailureAgentError,
				refusalError(events, req.WorkingDir, proc.Stderr)
		}
		return task.WorkerFailed, task.FailureAgentError, fmt.Errorf(
			"opencode ended with finish reason %q: %s, so it never stopped of its own accord",
			events.FinishReason, reason)
	}

	return task.WorkerSucceeded, task.FailureNone, nil
}

// refusalError explains a run that a refused tool call cut short, and says where
// the agent was reaching when it happened.
//
// The distinction it draws is the useful one. A refusal for a path outside the
// worktree is aidev's containment doing its job, and the remedy is the task: it
// asked for something that is not in the checkout. A refusal inside the worktree
// is a permission configuration, and the remedy is elsewhere entirely.
//
// Where it was reaching comes from OpenCode's own "permission requested" line on
// stderr when there is one: the tool event only says where a bash command ran,
// which is usually the worktree itself, while the line names what was turned
// down (TASK-000091 read /opt/kingsoft from a command run in its worktree).
func refusalError(events transcript, workingDir, stderr string) error {
	return toolRefused{refusalMessage(events, workingDir, stderr)}
}

// permissionRequest is the line OpenCode prints when a headless run turns a
// permission down: `permission requested: external_directory (/opt/x/*);
// auto-rejecting`, behind colour codes that ansiCode strips first.
var (
	permissionRequest = regexp.MustCompile(`permission requested: ([a-z_]+) \(([^)]*)\); auto-rejecting`)
	ansiCode          = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

// refusalMessage words the refusal; refusalError marks it as one.
func refusalMessage(events transcript, workingDir, stderr string) error {
	also := ""
	if events.Refusals > 1 {
		also = fmt.Sprintf(" (%d refused in all)", events.Refusals)
	}

	path, permission := events.RefusedPath, ""
	if m := permissionRequest.FindStringSubmatch(ansiCode.ReplaceAllString(stderr, "")); m != nil {
		permission, path = m[1], m[2]
	}
	if permission != "" {
		permission = " (" + permission + ")"
	}

	switch {
	case path == "":
		return fmt.Errorf("opencode stopped because a tool call was refused%s; a refusal ends "+
			"the session, so nothing after it ran", also)
	case outside(workingDir, path):
		return fmt.Errorf("opencode stopped because the agent reached outside its worktree, for %s%s%s; "+
			"a refusal ends the session, so nothing after it ran. The task is asking for something "+
			"that is not in the checkout: put what the agent needs into the repository (the planner "+
			"can gather it and commit it with the specification) rather than widening what the agent "+
			"may reach", path, permission, also)
	default:
		return fmt.Errorf("opencode stopped because a tool call for %s was refused%s; a refusal ends "+
			"the session, so nothing after it ran", path, also)
	}
}

// ErrToolRefused marks an agent run that a refused tool call cut short. A
// refusal ends the session, and the same request in the same session would be
// refused again, so automatic retry leaves these runs alone; errors.Is finds
// it without changing the wording of the message.
var ErrToolRefused = errors.New("a tool call was refused")

// toolRefused carries a refusal's message and answers errors.Is for
// ErrToolRefused.
type toolRefused struct{ err error }

func (e toolRefused) Error() string        { return e.err.Error() }
func (e toolRefused) Unwrap() error        { return e.err }
func (e toolRefused) Is(target error) bool { return target == ErrToolRefused }

// outside reports whether path is not under dir. It compares the paths as
// written: this decides the wording of a message, not access to anything, and
// the guarantee that an agent stays in its worktree is enforced in internal/git
// against resolved paths.
func outside(dir, path string) bool {
	if dir == "" || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// cutShort maps a finish reason that is not a completion to what it means. A reason
// outside it is left alone: a new one should not become a failure aidev invents.
var cutShort = map[string]string{
	"tool-calls": "the session ended while the agent was still calling tools",
	"length":     "the agent ran out of output space mid-answer",
}

// startupError explains a failure to launch in terms a user can act on.
func startupError(command string, proc procexec.Result, runErr error) error {
	stderr := firstLine(proc.Stderr)
	switch {
	case stderr != "":
		return fmt.Errorf("could not run %s: %s", command, stderr)
	case runErr != nil:
		return fmt.Errorf("could not run %s: %w", command, runErr)
	default:
		return fmt.Errorf("could not run %s", command)
	}
}

// agentLine matches a line of `opencode agent list`: the name at column zero
// followed by its mode in parentheses, with indented JSON beneath
// (docs/research.md §7b).
var agentLine = regexp.MustCompile(`^(\S+) \((primary|subagent)\)\s*$`)

// ListAgents returns the agent names OpenCode knows about. A successful
// lookup is served from cache for agentListTTL; a failure is never cached, so
// a transient one does not become the standing answer.
func (o *OpenCode) ListAgents(ctx context.Context) ([]string, error) {
	o.agentsMu.Lock()
	defer o.agentsMu.Unlock()

	if o.agents != nil && time.Since(o.agentsFresh) < agentListTTL {
		return o.agents, nil
	}

	// `opencode agent list` needs a working directory; the process's own is
	// fine, since nothing is written and no repository is involved.
	dir, err := currentDir()
	if err != nil {
		return nil, err
	}

	proc, err := procexec.Run(ctx, procexec.Spec{
		Command:        o.command(),
		Args:           []string{"agent", "list"},
		Dir:            dir,
		Timeout:        agentListTimeout,
		MaxOutputBytes: 1 << 20,
		// Listing agents needs no history; an in-memory database keeps the
		// lookup off the shared store a running task may hold
		// (docs/research.md §7k).
		ExtraEnv: []string{"OPENCODE_DB=:memory:"},
	})
	if err != nil {
		return nil, fmt.Errorf("list opencode agents: %w", err)
	}
	if !proc.Succeeded() {
		return nil, fmt.Errorf("list opencode agents: %s", firstLine(proc.Stderr))
	}

	var names []string
	for _, line := range strings.Split(proc.Stdout, "\n") {
		if m := agentLine.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("list opencode agents: no agents found in the output of `%s agent list`", o.command())
	}
	o.agents, o.agentsFresh = names, time.Now()
	return names, nil
}

// ValidateAgentName implements Validator.
//
// This check exists because OpenCode will not perform it: an unknown name warns,
// falls back, and exits 0. Validating here turns a silently wrong run into a
// clear refusal before any work starts.
func (o *OpenCode) ValidateAgentName(ctx context.Context, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("agent name is empty")
	}

	names, err := o.ListAgents(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range names {
		if candidate == trimmed {
			return nil
		}
	}
	return fmt.Errorf("opencode has no agent named %q; available agents: %s",
		trimmed, strings.Join(names, ", "))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// currentDir is separated so tests can rely on a real directory without caring
// which one it is.
func currentDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine current directory: %w", err)
	}
	return dir, nil
}
