package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidev/internal/task"
)

// Fixtures captured from codex-cli 0.154.0 during Phase 0 research
// (docs/research.md §7d), reproduced verbatim so the parser is tested against what
// the tool actually emits rather than an idealised shape.
const (
	codexThreadStarted = `{"type":"thread.started","thread_id":"01a09a7a-91d7-72e0-80b0-74eb23e2a0ee"}`
	codexTurnStarted   = `{"type":"turn.started"}`

	// A successful run emitted this and still exited 0 with the work done.
	// Treating it as fatal would fail every run on this configuration.
	codexWarningItem = `{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Model metadata for ` + "`oc/muse-spark-1.3-contributor-free(high)`" + ` not found. Defaulting to fallback metadata; this can degrade performance and cause issues."}}`

	codexAgentMessage = `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Got it — creating ` + "`greet.go`" + ` now."}}`
	codexCommandItem  = `{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"/bin/bash -lc \"cat > greet.go\"","exit_code":0}}`
	codexFinalMessage = `{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Created ` + "`greet.go`" + ` in the current directory."}}`

	codexTurnCompleted = `{"type":"turn.completed","usage":{"input_tokens":31169,"cached_input_tokens":24546,"cache_write_input_tokens":0,"output_tokens":724,"reasoning_output_tokens":571}}`
	codexTurnFailed    = `{"type":"turn.failed","error":{"message":"the model refused the request"}}`
)

// fakeCodex writes a stand-in executable. Its arguments are recorded so a test can
// assert the exact command line, and it writes the -o file when asked, because the
// real codex reports its final message that way.
func fakeCodex(t *testing.T, script string) (command, argsFile string) {
	t.Helper()

	dir := t.TempDir()
	command = filepath.Join(dir, "codex")
	argsFile = filepath.Join(dir, "args.txt")

	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\n" +
		script + "\n"
	if err := os.WriteFile(command, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return command, argsFile
}

func codexRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		TaskRef:        "TASK-000001",
		Prompt:         "Create greet.go with a Greet function",
		WorkingDir:     t.TempDir(),
		Timeout:        30 * time.Second,
		MaxOutputBytes: 64 * 1024,
	}
}

func TestCodexBuildsTheInvocationThatWasMeasured(t *testing.T) {
	c := NewCodex(CodexOptions{Command: "codex", Profile: "web9router", Sandbox: "workspace-write"})

	args := c.buildArgs(Request{WorkingDir: "/tmp/wt", Prompt: "do the thing"}, "/tmp/last.txt")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"exec",                 // the non-interactive subcommand
		"--json",               // JSONL events
		"-C /tmp/wt",           // the isolation hook
		"--profile web9router", // this installation needs a profile to reach a model
		"--sandbox workspace-write",
		"-o /tmp/last.txt", // the final message is written to a file
		"--skip-git-repo-check",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args = %q, want it to contain %q", joined, want)
		}
	}

	// The prompt must come last, after --, so a prompt starting with a dash is not
	// read as a flag.
	if args[len(args)-1] != "do the thing" {
		t.Errorf("last arg = %q, want the prompt", args[len(args)-1])
	}
	if args[len(args)-2] != "--" {
		t.Errorf("args = %v, want -- immediately before the prompt", args)
	}

	// --approve-for-me is mutually exclusive with --sandbox and exits 2 before
	// anything runs, so it must never be passed alongside one.
	if strings.Contains(joined, "--approve-for-me") {
		t.Error("--approve-for-me cannot be combined with --sandbox: codex exits 2")
	}
}

func TestCodexParsesASuccessfulRun(t *testing.T) {
	command, argsFile := fakeCodex(t, emit(
		codexThreadStarted, codexWarningItem, codexTurnStarted,
		codexAgentMessage, codexCommandItem, codexFinalMessage, codexTurnCompleted,
	)+"\n# the real codex writes its last message to the -o file\n"+
		`while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then printf 'Created greet.go in the current directory.' > "$2"; fi; shift; done`+"\nexit 0")

	c := NewCodex(CodexOptions{Command: command, Profile: "web9router"})
	res, err := c.Run(context.Background(), codexRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Status != task.WorkerSucceeded {
		t.Fatalf("status = %s (%v), want SUCCEEDED", res.Status, res.Err)
	}
	if res.Backend != CodexName {
		t.Errorf("backend = %q, want %q", res.Backend, CodexName)
	}
	if res.SessionID != "01a09a7a-91d7-72e0-80b0-74eb23e2a0ee" {
		t.Errorf("session id = %q, want the thread_id", res.SessionID)
	}
	if !strings.Contains(res.Summary, "Created") {
		t.Errorf("summary = %q, want the agent's final message", res.Summary)
	}
	if res.ToolCalls != 1 {
		t.Errorf("tool calls = %d, want 1 command_execution", res.ToolCalls)
	}

	var usage map[string]int
	if err := json.Unmarshal(res.Tokens, &usage); err != nil {
		t.Fatalf("tokens is not JSON: %v (%s)", err, res.Tokens)
	}
	if usage["input_tokens"] != 31169 || usage["output_tokens"] != 724 {
		t.Errorf("usage = %v, want the counts from turn.completed", usage)
	}

	args := readArgs(t, argsFile)
	if !contains(args, "-C") {
		t.Errorf("args = %v, want the working directory passed", args)
	}
}

// The trap this backend has to avoid. A run that emits an error item and still exits
// 0 has succeeded: on the measured configuration every run emits one, and failing on
// it would mean nothing ever succeeds.
func TestCodexErrorItemAloneIsNotAFailure(t *testing.T) {
	command, _ := fakeCodex(t, emit(
		codexThreadStarted, codexWarningItem, codexTurnStarted, codexFinalMessage, codexTurnCompleted,
	)+"\nexit 0")

	c := NewCodex(CodexOptions{Command: command})
	res, err := c.Run(context.Background(), codexRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: an error item accompanied by exit 0 is a warning, "+
			"and every run on the measured configuration emits one", res.Status)
	}
}

func TestCodexClassifiesRealFailures(t *testing.T) {
	cases := []struct {
		name   string
		script string
		status task.WorkerRunStatus
		kind   task.FailureKind
		detail string
	}{
		{
			name:   "turn failed",
			script: emit(codexThreadStarted, codexTurnStarted, codexTurnFailed) + "\nexit 0",
			status: task.WorkerFailed, kind: task.FailureAgentError, detail: "refused",
		},
		{
			name:   "non-zero exit",
			script: "echo 'error: the argument --sandbox cannot be used with --approve-for-me' >&2\nexit 2",
			status: task.WorkerFailed, kind: task.FailureAgentExit, detail: "2",
		},
		{
			name:   "exit zero with no events",
			script: "exit 0",
			status: task.WorkerFailed, kind: task.FailureUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command, _ := fakeCodex(t, tc.script)
			c := NewCodex(CodexOptions{Command: command})

			res, err := c.Run(context.Background(), codexRequest(t))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != tc.status || res.FailureKind != tc.kind {
				t.Errorf("result = %s/%s, want %s/%s (%v)", res.Status, res.FailureKind, tc.status, tc.kind, res.Err)
			}
			if tc.detail != "" && (res.Err == nil || !strings.Contains(res.Err.Error(), tc.detail)) {
				t.Errorf("Err = %v, want it to mention %q", res.Err, tc.detail)
			}
		})
	}
}

// stderr carries a large harmless model-catalogue error on every run with the measured
// profile, so it must not be used to decide the outcome.
func TestCodexIgnoresHarmlessStderr(t *testing.T) {
	noise := "failed to refresh available models: stream disconnected before completion: missing field models"
	command, _ := fakeCodex(t, "echo "+shellQuote(noise)+" >&2\n"+
		emit(codexThreadStarted, codexTurnStarted, codexFinalMessage, codexTurnCompleted)+"\nexit 0")

	c := NewCodex(CodexOptions{Command: command})
	res, err := c.Run(context.Background(), codexRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerSucceeded {
		t.Errorf("status = %s, want SUCCEEDED: stderr noise must not decide a Codex run", res.Status)
	}
	if !strings.Contains(res.Stderr, "failed to refresh") {
		t.Error("stderr was not captured, so it could not be diagnosed later")
	}
}

func TestCodexTimeoutAndCancellation(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		command, _ := fakeCodex(t, "sleep 30")
		req := codexRequest(t)
		req.Timeout = 300 * time.Millisecond

		res, err := NewCodex(CodexOptions{Command: command}).Run(context.Background(), req)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Status != task.WorkerTimedOut || res.FailureKind != task.FailureTimeout {
			t.Errorf("result = %s/%s, want TIMED_OUT/TIMEOUT", res.Status, res.FailureKind)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		command, _ := fakeCodex(t, "sleep 30")
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(200 * time.Millisecond); cancel() }()

		res, err := NewCodex(CodexOptions{Command: command}).Run(ctx, codexRequest(t))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Status != task.WorkerCancelled || res.FailureKind != task.FailureCancelled {
			t.Errorf("result = %s/%s, want CANCELLED/CANCELLED", res.Status, res.FailureKind)
		}
	})
}

func TestCodexMissingExecutableIsStartupFailure(t *testing.T) {
	c := NewCodex(CodexOptions{Command: filepath.Join(t.TempDir(), "not-installed")})

	res, err := c.Run(context.Background(), codexRequest(t))
	if err == nil {
		t.Fatal("a missing executable must be an error: nothing ran")
	}
	if res.FailureKind != task.FailureStartup {
		t.Errorf("failure kind = %s, want STARTUP", res.FailureKind)
	}
}

func TestCodexSatisfiesTheBackendInterface(t *testing.T) {
	var b Backend = NewCodex(CodexOptions{})
	if b.Name() != CodexName {
		t.Errorf("Name() = %q, want %q", b.Name(), CodexName)
	}
	if b.Name() == OpenCodeName {
		t.Error("the two backends must be distinguishable in an audit record")
	}
}

// Codex reports its final message through a file rather than in the stream, so the
// backend must create one, read it, and not leave it behind.
func TestCodexCleansUpItsMessageFile(t *testing.T) {
	command, argsFile := fakeCodex(t, emit(codexThreadStarted, codexTurnStarted, codexTurnCompleted)+
		"\n"+`while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then printf 'from the file' > "$2"; fi; shift; done`+"\nexit 0")

	c := NewCodex(CodexOptions{Command: command})
	res, err := c.Run(context.Background(), codexRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "from the file" {
		t.Errorf("summary = %q, want it read from the -o file", res.Summary)
	}

	args := readArgs(t, argsFile)
	var path string
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			path = args[i+1]
		}
	}
	if path == "" {
		t.Fatal("no -o argument was passed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists: the temporary message file must be removed", path)
	}
}

// An opt-in check against the installed codex, off by default so `make check` never
// needs a model or a network.
func TestAgainstRealCodex(t *testing.T) {
	if os.Getenv("AIDEV_TEST_CODEX") == "" {
		t.Skip("set AIDEV_TEST_CODEX=1 to exercise the installed codex")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewCodex(CodexOptions{
		Command: os.Getenv("CODEX_COMMAND"),
		Profile: os.Getenv("CODEX_PROFILE"),
		Sandbox: "workspace-write",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := c.Run(ctx, Request{
		TaskRef:        "TASK-PROBE",
		Prompt:         "Reply with exactly OK and do nothing else.",
		WorkingDir:     dir,
		Timeout:        4 * time.Minute,
		MaxOutputBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("Run against the real codex: %v", err)
	}
	if res.Status != task.WorkerSucceeded {
		t.Fatalf("status = %s: %v\nstderr: %s", res.Status, res.Err, res.Stderr)
	}
	t.Logf("summary=%q session=%s tokens=%s", res.Summary, res.SessionID, res.Tokens)
	if res.SessionID == "" {
		t.Error("no thread_id was recorded")
	}
}
