package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
// Every flag used here was verified against the installed version and is
// recorded in docs/research.md; none was taken from documentation or guessed
// at. The invocation differs by major version (docs/research.md §2.12):
//
//	1.x: opencode run --dir <worktree> --format json [--agent X] [-m model] [-s session] -- <prompt>
//	2.x: opencode run --standalone --format json [--agent X] [-m model] [-s session] -- <prompt>
//
// `--dir` is what confines a 1.x run to the worktree. 2.x removed it and
// connects to a shared background service by default, so `--standalone` starts
// a private server in the process working directory instead. `--format json`
// produces the newline-delimited event stream the result is built from. `--`
// guarantees a prompt beginning with a dash cannot be read as a flag.
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

	// ProviderKeyEnv maps an OpenCode provider id to the environment variable
	// holding its API key. OpenCode 2 keeps keys added with `opencode auth` in
	// its database, which a task's own OPENCODE_DB does not have, so such a
	// provider fails with provider.no-route (docs/research.md §7n). A key named
	// in OpenCode's configuration does not depend on the database. aidev passes
	// only "{env:NAME}" and OpenCode reads the variable itself: the key never
	// passes through aidev.
	ProviderKeyEnv map[string]string

	// agents holds the last successful lookup, guarded by agentsMu, which is
	// also held for the duration of a lookup so that concurrent validations
	// share one spawn instead of racing to start their own.
	agentsMu    sync.Mutex
	agents      []string
	agentsFresh time.Time

	// versionMu guards the detected CLI major version, cached after the first
	// successful probe. Without the cache every Run would spawn an extra
	// `opencode --version`, and a run cancelled while that probe is in flight
	// would be reported as a startup failure instead of a cancellation.
	versionMu    sync.Mutex
	versionKnown bool
	v2           bool
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

// isV2 reports whether the installed OpenCode is the 2.x line. The answer is
// cached after a successful probe, so a burst of tasks pays for `--version`
// once. A failed or unparseable probe is treated as 2.x for that call — the
// current line, where `--standalone` is the safer default — but is not
// remembered, so a transient failure does not pin the backend to a guess.
func (o *OpenCode) isV2(ctx context.Context) bool {
	o.versionMu.Lock()
	defer o.versionMu.Unlock()

	if o.versionKnown {
		return o.v2
	}

	raw, err := o.Version(ctx)
	if err != nil {
		return true
	}
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "opencode"))
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	major, err := strconv.Atoi(strings.SplitN(raw, ".", 2)[0])
	if err != nil {
		return true
	}
	o.v2, o.versionKnown = major >= 2, true
	return o.v2
}

// buildArgs assembles the argv. It is separate from Run so that a test can assert
// the command line without launching anything.
//
// OpenCode 2 removed `run --dir` and connects to a shared background service by
// default; `--standalone` starts a private server so the process working
// directory (and, through OPENCODE_CONFIG_CONTENT, the directory rules) apply.
// OpenCode 1.18 needs `--dir` instead, has no `--standalone`, and reads those
// rules from OPENCODE_PERMISSION instead (docs/research.md §7p).
func (o *OpenCode) buildArgs(req Request, v2 bool) []string {
	args := []string{"run"}
	if v2 {
		args = append(args, "--standalone")
	} else {
		args = append(args, "--dir", req.WorkingDir)
	}
	args = append(args, "--format", "json")

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

	// The version probe happens before the run starts, so it is the first
	// thing a cancellation can interrupt. A run cancelled there was still
	// cancelled by aidev; reporting a startup failure would blame the binary
	// for aidev's own decision to stop.
	result.Model = o.effectiveModel(req)
	v2 := o.isV2(ctx)
	if err := ctx.Err(); err != nil {
		result.Status = task.WorkerCancelled
		result.FailureKind = task.FailureCancelled
		result.Err = err
		result.FinishedAt = time.Now().UTC()
		return result, nil
	}
	args := o.buildArgs(req, v2)
	scanner := newEventScanner()
	// The scanner sees each tool call as the process writes it, so progress
	// can be reported during the run rather than reconstructed afterwards
	// (TASK-000143: 20 minutes with no record of what the agent did).
	scanner.progress = req.OnProgress
	scanner.progressEvery = req.ProgressInterval
	scanner.started = result.StartedAt

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
	// 1.x reads these rules from OPENCODE_PERMISSION; 2.x ignores it and reads
	// the same rules from OPENCODE_CONFIG_CONTENT below (docs/research.md §7p).
	// Passing both keeps a 1.x run confined and is harmless to a 2.x one.
	directoryRules := directoryPermissionRules(req.ReadDirs)
	extraEnv = append(extraEnv, "OPENCODE_PERMISSION="+externalDirectoryPermission(directoryRules))

	// The agent runs the task's own checks while it works, so it needs the
	// same AIDEV_TASK_REF they get: a check can then keep its resources, such
	// as a test database, apart per task (TASK-000166's agent broke the
	// shared one). Added after the drop below, so it is the only AIDEV_
	// variable that reaches the run.
	if req.TaskRef != "" {
		extraEnv = append(extraEnv, "AIDEV_TASK_REF="+req.TaskRef)
	}

	// OPENCODE_CONFIG_CONTENT is what this run builds below; an inherited one
	// could name servers, providers or directory rules this run does not
	// control. Drop it for both versions: a 1.x run builds no replacement, so
	// it stays unset there as it was before.
	dropEnv := []string{"AIDEV_", "OPENCODE_CONFIG_CONTENT"}

	// OpenCode 2 starts every MCP server its configuration names, including
	// the person's global servers and every opencode.json found walking up
	// from the worktree; a task's agent must not inherit them (docs/research.md
	// §7o). List them from the worktree first, so the replacement names exactly
	// what this run would start. A listing aidev cannot read is a startup
	// failure: a run with unknown tools is worse than no run.
	if v2 {
		disabled, err := o.listMCPServers(ctx, req.WorkingDir, dropEnv, extraEnv)
		if err != nil {
			// A cancellation during the listing is aidev's own decision to
			// stop, not a failure of the binary's configuration.
			if ctxErr := ctx.Err(); ctxErr != nil {
				result.Status = task.WorkerCancelled
				result.FailureKind = task.FailureCancelled
				result.Err = ctxErr
				result.FinishedAt = time.Now().UTC()
				return result, nil
			}
			result.Status = task.WorkerFailed
			result.FailureKind = task.FailureStartup
			result.Err = err
			result.FinishedAt = time.Now().UTC()
			return result, err
		}
		result.DisabledMCP = disabled
	}

	// Only 2.x understands the configuration form of the directory rules, and
	// every 2.x run carries them: without them it is not confined at all. A
	// 1.x run still gets them through OPENCODE_PERMISSION (docs/research.md §7p).
	var configRules map[string]string
	if v2 {
		configRules = directoryRules
	}
	if content := openCodeConfigContent(o.ProviderKeyEnv, result.DisabledMCP, configRules); content != "" {
		extraEnv = append(extraEnv, "OPENCODE_CONFIG_CONTENT="+content)
	}

	spec := procexec.Spec{
		Command:        o.command(),
		Args:           args,
		Dir:            req.WorkingDir,
		Timeout:        req.Timeout,
		MaxOutputBytes: req.MaxOutputBytes,
		// The agent executes instructions we do not control; it must not see
		// aidev's own configuration (AIDEV_CONFIG → database URL).
		DropEnv:  dropEnv,
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
	// The prompt tells the agent to leave no process behind; this is the
	// record that aidev had to stop one it left.
	result.OrphansKilled = proc.OrphansKilled
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
		// A model the run cannot reach fails identically on every attempt
		// (docs/research.md §7n): retrying only repeats the same failure, so it
		// is a startup failure, which the worker never retries (TASK-000149).
		// Every other reported error may be transient and stays an agent error.
		kind := task.FailureAgentError
		if events.ProviderNoRoute {
			kind = task.FailureStartup
		}
		return task.WorkerFailed, kind,
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
		// OpenCode 2 ends a completed run with the assistant's final text and
		// no closing step_finish "stop" (docs/research.md §2.12). The last
		// reason then belongs to an earlier tool-call step, and the text is the
		// agent stopping of its own accord. Verification still decides whether
		// the work passes; this only keeps a completed turn from being recorded
		// as a model that quit halfway.
		if events.LastEvent == "text" {
			return task.WorkerSucceeded, task.FailureNone, nil
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

	// The lookup needs a working directory; the process's own is fine, since
	// nothing is written and no repository is involved.
	dir, err := currentDir()
	if err != nil {
		return nil, err
	}

	// OpenCode 2 renamed `agent list` to `debug agents` and prints JSON.
	// Try v2 first, then fall back to v1.
	names, v2Err := o.listAgentsV2(ctx, dir)
	if v2Err != nil || len(names) == 0 {
		var v1Err error
		names, v1Err = o.listAgentsV1(ctx, dir)
		if v1Err != nil {
			if v2Err != nil {
				return nil, fmt.Errorf("list opencode agents: v2: %v; v1: %v", v2Err, v1Err)
			}
			return nil, v1Err
		}
	}

	o.agents, o.agentsFresh = names, time.Now()
	return names, nil
}

// listAgentsV2 parses `opencode debug agents`, which prints a JSON array.
func (o *OpenCode) listAgentsV2(ctx context.Context, dir string) ([]string, error) {
	// OpenCode 2 answers the first `debug agents` in a directory it has not
	// seen with an empty array while it creates the project store; the next
	// call lists the built-in agents. Measured on 2.1.3 (docs/research.md
	// §2.12). Retry once rather than falling through to the v1 command, which
	// no longer exists.
	names, err := o.debugAgents(ctx, dir)
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		return names, nil
	}
	return o.debugAgents(ctx, dir)
}

// debugAgents runs `opencode debug agents` once and extracts the names.
func (o *OpenCode) debugAgents(ctx context.Context, dir string) ([]string, error) {
	// Do not point OPENCODE_DB at :memory: here: in OpenCode 2 the agent
	// configuration lives in that database, so an in-memory store lists no
	// agents at all. The v1 command keeps the in-memory override below.
	proc, err := procexec.Run(ctx, procexec.Spec{
		Command:        o.command(),
		Args:           []string{"debug", "agents"},
		Dir:            dir,
		Timeout:        agentListTimeout,
		MaxOutputBytes: 1 << 20,
	})
	if err != nil {
		return nil, err
	}
	if !proc.Succeeded() {
		return nil, fmt.Errorf("%s", firstLine(proc.Stderr))
	}

	// Tolerate a log line before or after the JSON payload.
	out := proc.Stdout
	start, end := strings.Index(out, "["), strings.LastIndex(out, "]")
	if start < 0 || end < start {
		return nil, errors.New("no JSON array in `debug agents` output")
	}
	var agents []struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), &agents); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.ID != "" && (a.Mode == "primary" || a.Mode == "subagent") {
			names = append(names, a.ID)
		}
	}
	return names, nil
}

// listAgentsV1 parses the pre-2.0 `opencode agent list` output.
func (o *OpenCode) listAgentsV1(ctx context.Context, dir string) ([]string, error) {
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
		return nil, err
	}
	if !proc.Succeeded() {
		return nil, fmt.Errorf("%s", firstLine(proc.Stderr))
	}

	var names []string
	for _, line := range strings.Split(proc.Stdout, "\n") {
		if m := agentLine.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no agents found in the output of `%s agent list`", o.command())
	}
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

// mcpListTimeout bounds the `opencode debug config` lookup. It reads
// configuration only, but a cold OpenCode may create its project store first;
// generous, still bounded (invariant 6).
const mcpListTimeout = 60 * time.Second

// listMCPServers names every MCP server the OpenCode configuration reachable
// from dir would start. The listing is taken in dir because OpenCode walks up
// from the working directory for opencode.json (docs/research.md §7o).
func (o *OpenCode) listMCPServers(ctx context.Context, dir string, dropEnv, env []string) ([]string, error) {
	proc, err := procexec.Run(ctx, procexec.Spec{
		Command:        o.command(),
		Args:           []string{"debug", "config"},
		Dir:            dir,
		Timeout:        mcpListTimeout,
		MaxOutputBytes: 1 << 20,
		// The listing runs OpenCode's configuration code, so it gets the same
		// environment a run does: no AIDEV_ secrets, the task's database and
		// permission rules. OPENCODE_CONFIG_CONTENT is deliberately absent —
		// it is what this listing is used to build.
		DropEnv:  dropEnv,
		ExtraEnv: env,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot list opencode's MCP servers: %w",
			startupError(o.command(), proc, err))
	}
	if !proc.Succeeded() {
		detail := firstLine(proc.Stderr)
		if detail == "" {
			detail = string(proc.Outcome)
		}
		return nil, fmt.Errorf("cannot list opencode's MCP servers: %s", detail)
	}

	var sources []struct {
		Info struct {
			MCP struct {
				Servers map[string]json.RawMessage `json:"servers"`
			} `json:"mcp"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(proc.Stdout), &sources); err != nil {
		return nil, fmt.Errorf("cannot list opencode's MCP servers: %w", err)
	}

	seen := make(map[string]struct{})
	for _, source := range sources {
		for name := range source.Info.MCP.Servers {
			seen[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// openCodeConfigContent is the OPENCODE_CONFIG_CONTENT for a run: the directory
// rules, the provider keys and a complete disabled entry for every MCP server
// the configuration would otherwise start. All travel in the one variable
// OpenCode reads, so they must be merged. A partial MCP entry is ignored and
// the server still starts; the whole entry is replaced (docs/research.md §7o).
// nil rules leave the permission setting out, which is what a 1.x run gets.
// "" means leave the variable unset.
func openCodeConfigContent(keys map[string]string, disabledMCP []string, rules map[string]string) string {
	if len(keys) == 0 && len(disabledMCP) == 0 && len(rules) == 0 {
		return ""
	}
	content := make(map[string]any, 3)
	if len(rules) > 0 {
		content["permission"] = map[string]any{"external_directory": rules}
	}
	if len(keys) > 0 {
		providers := make(map[string]any, len(keys))
		for provider, variable := range keys {
			providers[provider] = map[string]any{"options": map[string]any{"apiKey": "{env:" + variable + "}"}}
		}
		content["provider"] = providers
	}
	if len(disabledMCP) > 0 {
		servers := make(map[string]any, len(disabledMCP))
		for _, name := range disabledMCP {
			// Nothing from the original entry is repeated, in particular not
			// its environment: aidev's own configuration may live there.
			servers[name] = map[string]any{
				"type":     "local",
				"command":  []string{"true"},
				"disabled": true,
			}
		}
		content["mcp"] = map[string]any{"servers": servers}
	}
	encoded, _ := json.Marshal(content)
	return string(encoded)
}

// directoryPermissionRules is what both forms of the directory rules carry:
// every directory outside the working directory denied, except the ones the
// project lets the agent read.
//
// The default is "ask", and a headless run cannot answer a question: it
// rejects the call, and the rejection ends the session (TASK-000091 lost its
// run to one read of /opt/kingsoft). A rule that denies comes back to the
// agent as a failed tool call instead, and the session goes on — measured on
// OpenCode 1.18.35 (docs/research.md §7m). OpenCode's own /tmp/opencode/*
// keeps the allow it has by default; each read dir is allowed as itself and
// everything under it ("**"; a single "*" matches one level only).
func directoryPermissionRules(readDirs []string) map[string]string {
	rules := map[string]string{"*": "deny", "/tmp/opencode/*": "allow"}
	for _, dir := range readDirs {
		dir = strings.TrimRight(filepath.Clean(dir), "/")
		if dir == "" {
			continue
		}
		rules[dir] = "allow"
		rules[dir+"/**"] = "allow"
	}
	return rules
}

// externalDirectoryPermission encodes the rules for OPENCODE_PERMISSION, which
// OpenCode 1.x reads and merges into the user's configuration, later rules
// winning, so this only changes the rules it names. OpenCode 2.x ignores the
// variable and takes the same rules through OPENCODE_CONFIG_CONTENT instead
// (docs/research.md §7p).
func externalDirectoryPermission(rules map[string]string) string {
	// Map order would not matter to OpenCode's last-match rule for these
	// patterns, except "*": it must come before the allows. JSON objects are
	// written in key order by encoding/json, and "*" sorts before "/".
	encoded, _ := json.Marshal(map[string]any{"external_directory": rules})
	return string(encoded)
}
