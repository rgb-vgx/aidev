package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"aidev/internal/procexec"
	"aidev/internal/task"
)

// CodexName is how this backend identifies itself in audit records.
const CodexName = "codex"

// DefaultCodexCommand is the executable name, resolved through PATH.
const DefaultCodexCommand = "codex"

// CodexOptions configures the Codex backend.
type CodexOptions struct {
	// Command is the executable. Empty means DefaultCodexCommand.
	Command string

	// Profile is passed as --profile when set.
	Profile string

	// Model is passed as -m when set.
	Model string

	// Sandbox is passed as --sandbox when set.
	Sandbox string
}

// Codex runs tasks through the Codex CLI.
//
// Every flag used here was measured against codex-cli 0.154.0 during Phase 0
// and is recorded in docs/research.md §7d. The invocation is:
//
//	codex exec --json --skip-git-repo-check -C <worktree> -o <file>
//	    [--profile p] [-m model] [--sandbox s] -- <prompt>
//
// `-C` is what confines the agent to the worktree. `--json` produces the
// newline-delimited event stream the result is built from. `-o` receives the
// final message, because the stream itself does not carry it reliably. `--`
// guarantees a prompt beginning with a dash cannot be read as a flag.
//
// `--approve-for-me` is never passed: codex rejects it alongside `--sandbox`
// and exits 2 before running anything.
type Codex struct {
	command string
	profile string
	model   string
	sandbox string
}

// NewCodex returns a backend using the given options.
func NewCodex(opts CodexOptions) *Codex {
	return &Codex{
		command: strings.TrimSpace(opts.Command),
		profile: strings.TrimSpace(opts.Profile),
		model:   strings.TrimSpace(opts.Model),
		sandbox: strings.TrimSpace(opts.Sandbox),
	}
}

// Name implements Backend.
func (c *Codex) Name() string { return CodexName }

func (c *Codex) executable() string {
	if c.command == "" {
		return DefaultCodexCommand
	}
	return c.command
}

// buildArgs assembles the argv. It is separate from Run so that a test can
// assert the command line without launching anything.
func (c *Codex) buildArgs(req Request, lastMessagePath string) []string {
	args := []string{"exec", "--json", "--skip-git-repo-check", "-C", req.WorkingDir, "-o", lastMessagePath}

	if c.profile != "" {
		args = append(args, "--profile", c.profile)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.model
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	if c.sandbox != "" {
		args = append(args, "--sandbox", c.sandbox)
	}

	// Everything after -- is the message, so a prompt starting with a dash is
	// safe (docs/research.md §7d).
	return append(args, "--", req.Prompt)
}

// Run implements Backend.
func (c *Codex) Run(ctx context.Context, req Request) (Result, error) {
	result := Result{
		Backend:    c.Name(),
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

	// The final message arrives through a file rather than the stream, so one
	// is created before running and read afterwards.
	lastMessage, err := os.CreateTemp("", "codex-last-message-*")
	if err != nil {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = fmt.Errorf("create codex message file: %w", err)
		result.FinishedAt = time.Now().UTC()
		return result, result.Err
	}
	lastMessagePath := lastMessage.Name()
	_ = lastMessage.Close()
	// Removed whether the run succeeded or failed, so a worktree never
	// accumulates message files from previous attempts.
	defer func() { _ = os.Remove(lastMessagePath) }()

	args := c.buildArgs(req, lastMessagePath)
	scanner := newCodexScanner()

	spec := procexec.Spec{
		Command:        c.executable(),
		Args:           args,
		Dir:            req.WorkingDir,
		Timeout:        req.Timeout,
		MaxOutputBytes: req.MaxOutputBytes,
		// Parse while the process runs, so events survive even when the
		// capture cap discards the tail of a very chatty run.
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
	result.ToolCalls = events.ToolCalls
	result.StartedAt = proc.StartedAt
	result.FinishedAt = proc.FinishedAt
	result.Duration = proc.Duration

	if len(events.Tokens) > 0 {
		result.Tokens = events.Tokens
	}

	// The -o file is authoritative; the last agent_message is only a fallback
	// for when it is empty or missing.
	if data, err := os.ReadFile(lastMessagePath); err == nil && strings.TrimSpace(string(data)) != "" {
		result.Summary = strings.TrimSpace(string(data))
	} else {
		result.Summary = events.Summary
	}

	// A process that could not start is aidev's problem to report, not an
	// outcome to record as the agent's failure.
	if proc.Outcome == procexec.OutcomeStartFailed {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = codexStartupError(c.executable(), proc, runErr)
		return result, result.Err
	}
	if runErr != nil {
		result.Status = task.WorkerFailed
		result.FailureKind = task.FailureStartup
		result.Err = runErr
		return result, runErr
	}

	result.Status, result.FailureKind, result.Err = classifyCodex(proc, events, req)
	return result, nil
}

// classifyCodex decides the outcome from the process result and the event stream.
//
// The order is deliberate. Cancellation and timeout come first because they
// are facts about aidev's own decision to stop. A turn.failed event is checked
// before the exit code, because an error item alone is not a failure on the
// measured configuration while a failed turn is. stderr is never consulted:
// every run on the measured profile carries a large harmless model-catalogue
// error there (docs/research.md §7d).
func classifyCodex(proc procexec.Result, events codexTranscript, req Request) (task.WorkerRunStatus, task.FailureKind, error) {
	switch proc.Outcome {
	case procexec.OutcomeCancelled:
		return task.WorkerCancelled, task.FailureCancelled, proc.Err
	case procexec.OutcomeTimedOut:
		return task.WorkerTimedOut, task.FailureTimeout,
			fmt.Errorf("codex exceeded the task timeout of %s", req.Timeout)
	}

	if len(events.Errors) > 0 {
		return task.WorkerFailed, task.FailureAgentError,
			fmt.Errorf("codex reported: %s", strings.Join(events.Errors, "; "))
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
			fmt.Errorf("codex exited %s: %s", code, detail)
	}

	// Exit 0 with no events at all means the process ran but produced nothing.
	// That is not a success worth passing on: there is no session, no summary,
	// and no evidence the agent did anything.
	if events.Lines == 0 {
		return task.WorkerFailed, task.FailureUnknown,
			fmt.Errorf("codex exited 0 but produced no events; stderr: %s", firstLine(proc.Stderr))
	}

	return task.WorkerSucceeded, task.FailureNone, nil
}

// codexStartupError explains a failure to launch in terms a user can act on.
func codexStartupError(command string, proc procexec.Result, runErr error) error {
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

// codexEvent is one line of `codex exec --json` output: newline-delimited JSON
// with a type discriminator (docs/research.md §7d).
type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     *codexItem      `json:"item"`
	Usage    json.RawMessage `json:"usage"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// codexItem is the payload of an item.completed event. Only the fields aidev
// uses are modelled; the rest is ignored.
type codexItem struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Message string `json:"message"`
	Command string `json:"command"`
}

// codexTranscript is what aidev extracts from the Codex event stream.
type codexTranscript struct {
	SessionID string

	// Summary is the last agent_message: only a fallback, because the
	// authoritative final message arrives through the -o file.
	Summary string

	ToolCalls int
	Tokens    json.RawMessage
	Errors    []string
	Unknown   map[string]int

	// Lines counts parsed lines, which distinguishes "the agent said nothing"
	// from "the agent was never reached".
	Lines int
}

// codexScanner parses the NDJSON stream as it arrives.
//
// It is wired as a Tee on stdout rather than parsing the captured buffer
// afterwards, so that events are still extracted when output exceeds the
// capture cap and the tail is discarded.
type codexScanner struct {
	buf    bytes.Buffer
	result codexTranscript
}

func newCodexScanner() *codexScanner {
	return &codexScanner{result: codexTranscript{Unknown: map[string]int{}}}
}

// Write implements io.Writer. Partial lines are held until their newline
// arrives, because a process write can split a JSON object anywhere.
func (s *codexScanner) Write(p []byte) (int, error) {
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadBytes('\n')
		if err != nil {
			// No complete line yet: put the fragment back and wait.
			s.buf.Write(line)
			return len(p), nil
		}
		s.consume(line)
	}
}

// Close processes a trailing line with no newline, which happens when a
// process is killed mid-write.
func (s *codexScanner) Close() {
	if s.buf.Len() > 0 {
		s.consume(s.buf.Bytes())
		s.buf.Reset()
	}
}

// Transcript returns what has been parsed so far.
func (s *codexScanner) Transcript() codexTranscript { return s.result }

func (s *codexScanner) consume(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}

	var ev codexEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		// A line that is not JSON is noise, not a reason to fail the task. It
		// is counted so that a malformed stream is visible in the record.
		s.result.Unknown["unparseable"]++
		return
	}
	s.result.Lines++

	if ev.ThreadID != "" {
		s.result.SessionID = ev.ThreadID
	}

	switch ev.Type {
	case "thread.started":
		// The session id above is the whole payload.

	case "turn.started":
		// Nothing to extract; named so it is not counted as unknown.

	case "item.completed":
		if ev.Item == nil {
			break
		}
		switch ev.Item.Type {
		case "agent_message":
			if strings.TrimSpace(ev.Item.Text) != "" {
				// Later messages supersede earlier ones; kept only as a
				// fallback for when the -o file is empty or missing.
				s.result.Summary = strings.TrimSpace(ev.Item.Text)
			}
		case "command_execution":
			s.result.ToolCalls++
		case "error":
			// Every run on the measured configuration emits one about model
			// metadata and still exits 0 having done the work, so this is a
			// warning rather than a failure (docs/research.md §7d).
		default:
			s.result.Unknown[ev.Item.Type]++
		}

	case "turn.completed":
		if len(ev.Usage) > 0 {
			s.result.Tokens = append(json.RawMessage(nil), ev.Usage...)
		}

	case "turn.failed":
		message := "the agent reported an error"
		if ev.Error != nil && strings.TrimSpace(ev.Error.Message) != "" {
			message = strings.TrimSpace(ev.Error.Message)
		}
		s.result.Errors = append(s.result.Errors, message)

	default:
		// Unknown event types are counted and ignored. Codex may add events,
		// and failing a task because its agent learned a new trick would be
		// the wrong response.
		s.result.Unknown[ev.Type]++
	}
}
