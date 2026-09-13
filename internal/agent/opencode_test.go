package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidev/internal/task"
)

// These fixtures are the real event stream captured from OpenCode 1.18.30 during
// Phase 0, reproduced verbatim so the parser is tested against what the tool
// actually emits rather than against an idealised shape.
const (
	fixtureStepStart = `{"type":"step_start","timestamp":1789233976688,"sessionID":"ses_f69583e28ffeaKEYp21iY6h9hp","part":{"id":"prt_096a7d566001ZQAByYXuQt26qf","messageID":"msg_096a7c3260018lsBVJNaUUScSR","sessionID":"ses_f69583e28ffeaKEYp21iY6h9hp","snapshot":"151331b19a652c9365629141d205e6ed9cca3452","type":"step-start"}}`

	fixtureToolUse = `{"type":"tool_use","timestamp":1789233980042,"sessionID":"ses_f69583e28ffeaKEYp21iY6h9hp","part":{"type":"tool","tool":"write","callID":"call-8d365897-684f-41ae-80f0-c0dadb8d335c","state":{"status":"completed","input":{"filePath":"/tmp/wt/greet.go","content":"package main"},"output":"Wrote file successfully.","title":"greet.go","time":{"start":1789233980024,"end":1789233980039}},"id":"prt_096a7e267001FxL1M3meU1uBgn","sessionID":"ses_f69583e28ffeaKEYp21iY6h9hp","messageID":"msg_096a7c3260018lsBVJNaUUScSR"}}`

	fixtureStepFinishTools = `{"type":"step_finish","timestamp":1789233980095,"sessionID":"ses_f69583e28ffeaKEYp21iY6h9hp","part":{"id":"prt_096a7e2bb001ZnmTewN2CP97Sk","reason":"tool-calls","snapshot":"8c3b0b127c09133a3ce4b4d34e117c62490bebfd","messageID":"msg_096a7c3260018lsBVJNaUUScSR","type":"step-finish","tokens":{"total":8583,"input":8421,"output":76,"reasoning":86,"cache":{"write":0,"read":0}},"cost":0}}`

	fixtureText = `{"type":"text","timestamp":1789233986643,"sessionID":"ses_f69583e28ffeaKEYp21iY6h9hp","part":{"id":"prt_096a7fab8001cUSEL6H6DKDI2M","messageID":"msg_096a7e2d30012TXQFeFSy7WF0W","type":"text","text":"Done. Created ` + "`greet.go`" + ` with package main and Greet function.","time":{"start":1789233986232,"end":1789233986635}}}`

	fixtureStepFinishStop = `{"type":"step_finish","timestamp":1789233986700,"sessionID":"ses_f69583e28ffeaKEYp21iY6h9hp","part":{"id":"prt_096a7fb00001aaa","reason":"stop","messageID":"msg_096a7e2d30012TXQFeFSy7WF0W","type":"step-finish","tokens":{"total":100,"input":90,"output":10,"reasoning":4,"cache":{"write":1,"read":2}},"cost":0.0025}}`

	// An invalid model produced this, with exit status 1 (docs/research.md §2.5).
	fixtureError = `{"type":"error","timestamp":1789234012112,"sessionID":"ses_f6957a19bffey1BF0BPX8sr90D","error":{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details.","ref":"err_08b666ce"}}}`
)

// fakeOpenCode writes an executable that stands in for the opencode binary. Its
// arguments are recorded so a test can assert the exact command line, and its
// stdout, stderr and exit status are scripted.
func fakeOpenCode(t *testing.T, script string) (command string, argsFile string) {
	t.Helper()

	dir := t.TempDir()
	command = filepath.Join(dir, "opencode")
	argsFile = filepath.Join(dir, "args.txt")

	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\n" +
		script + "\n"
	if err := os.WriteFile(command, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return command, argsFile
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	var args []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			args = append(args, line)
		}
	}
	return args
}

func emit(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "cat <<'AIDEV_EOF'\n%s\nAIDEV_EOF\n", l)
	}
	return b.String()
}

func openCodeRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		TaskRef:        "TASK-000001",
		Prompt:         "Create greet.go with a Greet function",
		WorkingDir:     t.TempDir(),
		Agent:          "build",
		Timeout:        30 * time.Second,
		MaxOutputBytes: 64 * 1024,
	}
}

func TestBuildArgs(t *testing.T) {
	o := NewOpenCode("", "")
	req := Request{WorkingDir: "/tmp/wt", Prompt: "do the thing"}

	got := strings.Join(o.buildArgs(req), " ")
	want := "run --dir /tmp/wt --format json -- do the thing"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	// An agent, a model and a session are added only when present.
	req.Agent = "build"
	req.Model = "opencode/nemotron-3.5-lightning-free"
	req.SessionID = "ses_abc"
	got = strings.Join(o.buildArgs(req), " ")
	want = "run --dir /tmp/wt --format json --agent build -m opencode/nemotron-3.5-lightning-free -s ses_abc -- do the thing"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	// The backend's configured model is the fallback.
	o2 := NewOpenCode("", "backend/model")
	args := o2.buildArgs(Request{WorkingDir: "/tmp/wt", Prompt: "x"})
	if !contains(args, "backend/model") {
		t.Errorf("args = %v, want the backend's model used when the request has none", args)
	}
}

// A prompt beginning with a dash must not be read as a flag, which is what the
// -- separator is for.
func TestPromptStartingWithDashIsNotAFlag(t *testing.T) {
	o := NewOpenCode("", "")
	args := o.buildArgs(Request{WorkingDir: "/tmp/wt", Prompt: "--version"})

	last := args[len(args)-1]
	if last != "--version" {
		t.Errorf("last arg = %q, want the prompt verbatim", last)
	}
	if args[len(args)-2] != "--" {
		t.Errorf("args = %v, want -- immediately before the prompt", args)
	}
}

func TestSuccessfulRunIsParsedFromTheEventStream(t *testing.T) {
	command, argsFile := fakeOpenCode(t, emit(
		fixtureStepStart, fixtureToolUse, fixtureStepFinishTools, fixtureText, fixtureStepFinishStop,
	)+"exit 0")

	o := NewOpenCode(command, "")
	req := openCodeRequest(t)

	res, err := o.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerSucceeded {
		t.Fatalf("status = %s (%v), want SUCCEEDED", res.Status, res.Err)
	}
	if res.FailureKind != task.FailureNone {
		t.Errorf("failure kind = %s, want none", res.FailureKind)
	}
	if res.SessionID != "ses_f69583e28ffeaKEYp21iY6h9hp" {
		t.Errorf("session id = %q, want it taken from the stream", res.SessionID)
	}
	if !strings.HasPrefix(res.Summary, "Done. Created") {
		t.Errorf("summary = %q, want the agent's last text message", res.Summary)
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want the final step's reason", res.FinishReason)
	}
	if res.ToolCalls != 1 {
		t.Errorf("tool calls = %d, want 1", res.ToolCalls)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if res.Backend != OpenCodeName {
		t.Errorf("backend = %q", res.Backend)
	}

	// Usage is summed across steps: neither the first nor the last step is the
	// total, so the record must not simply pass one of them through.
	var usage map[string]int
	if err := json.Unmarshal(res.Tokens, &usage); err != nil {
		t.Fatalf("tokens is not JSON: %v (%s)", err, res.Tokens)
	}
	if usage["steps"] != 2 {
		t.Errorf("steps = %d, want 2", usage["steps"])
	}
	if usage["input_tokens"] != 8421+90 {
		t.Errorf("input_tokens = %d, want the sum across steps", usage["input_tokens"])
	}
	if usage["output_tokens"] != 76+10 {
		t.Errorf("output_tokens = %d, want the sum across steps", usage["output_tokens"])
	}
	if usage["cache_read_tokens"] != 2 || usage["cache_write_tokens"] != 1 {
		t.Errorf("cache token sums = %d/%d, want 2/1", usage["cache_read_tokens"], usage["cache_write_tokens"])
	}
	if res.Cost == nil || *res.Cost != 0.0025 {
		t.Errorf("cost = %v, want 0.0025 summed across steps", res.Cost)
	}

	// The agent must have been pointed at the worktree and nowhere else.
	args := readArgs(t, argsFile)
	if !contains(args, "--dir") || !contains(args, req.WorkingDir) {
		t.Errorf("args = %v, want --dir %s", args, req.WorkingDir)
	}
	if !contains(args, "--format") || !contains(args, "json") {
		t.Errorf("args = %v, want --format json", args)
	}
}

// An error event is the failure signal even when the process exits 0, because
// Phase 0 showed the exit status is not reliable on its own.
func TestErrorEventIsAFailureRegardlessOfExitStatus(t *testing.T) {
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprintf("exit %d", exit), func(t *testing.T) {
			command, _ := fakeOpenCode(t, emit(fixtureError)+fmt.Sprintf("exit %d", exit))
			o := NewOpenCode(command, "")

			res, err := o.Run(context.Background(), openCodeRequest(t))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != task.WorkerFailed {
				t.Errorf("status = %s, want FAILED", res.Status)
			}
			if res.FailureKind != task.FailureAgentError {
				t.Errorf("failure kind = %s, want AGENT_ERROR", res.FailureKind)
			}
			if res.Err == nil || !strings.Contains(res.Err.Error(), "UnknownError") {
				t.Errorf("Err = %v, want the reported error name", res.Err)
			}
			if res.Err == nil || !strings.Contains(res.Err.Error(), "err_08b666ce") {
				t.Errorf("Err = %v, want the error reference preserved for diagnosis", res.Err)
			}
		})
	}
}

func TestNonZeroExitWithoutEventsIsAgentExit(t *testing.T) {
	command, _ := fakeOpenCode(t, "echo 'Error: Failed to change directory to /nope' >&2\nexit 1")
	o := NewOpenCode(command, "")

	res, err := o.Run(context.Background(), openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerFailed || res.FailureKind != task.FailureAgentExit {
		t.Errorf("result = %s/%s, want FAILED/AGENT_EXIT", res.Status, res.FailureKind)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "exited 1") {
		t.Errorf("Err = %v, want the exit status named", res.Err)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "Failed to change directory") {
		t.Errorf("Err = %v, want stderr included for diagnosis", res.Err)
	}
}

// Exit 0 with an empty stream means the process ran but did nothing observable.
// Passing that on as success would put an empty, confident record in the database.
func TestExitZeroWithNoEventsIsNotSuccess(t *testing.T) {
	command, _ := fakeOpenCode(t, "exit 0")
	o := NewOpenCode(command, "")

	res, err := o.Run(context.Background(), openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status == task.WorkerSucceeded {
		t.Error("a run that produced no events was reported as success")
	}
	if res.FailureKind != task.FailureUnknown {
		t.Errorf("failure kind = %s, want UNKNOWN: the reason genuinely is not known", res.FailureKind)
	}
}

// The measured hazard: an unknown agent warns on stderr, runs the default agent,
// and exits 0. Recording that as success would make the audit record claim the
// task ran with an agent it never used.
func TestSilentAgentFallbackIsRefused(t *testing.T) {
	command, _ := fakeOpenCode(t,
		"echo '! agent \"reviewer\" not found. Falling back to default agent' >&2\n"+
			emit(fixtureStepStart, fixtureText, fixtureStepFinishStop)+"exit 0")

	o := NewOpenCode(command, "")
	req := openCodeRequest(t)
	req.Agent = "reviewer"

	res, err := o.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerFailed {
		t.Fatalf("status = %s, want FAILED: the requested agent was not used", res.Status)
	}
	if res.FailureKind != task.FailureStartup {
		t.Errorf("failure kind = %s, want STARTUP", res.FailureKind)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "reviewer") {
		t.Errorf("Err = %v, want the rejected agent name", res.Err)
	}
}

func TestMissingExecutableIsStartupFailure(t *testing.T) {
	o := NewOpenCode(filepath.Join(t.TempDir(), "not-installed"), "")

	res, err := o.Run(context.Background(), openCodeRequest(t))
	if err == nil {
		t.Fatal("a missing executable must be an error: nothing ran")
	}
	if res.FailureKind != task.FailureStartup {
		t.Errorf("failure kind = %s, want STARTUP", res.FailureKind)
	}
	if !strings.Contains(err.Error(), "could not run") {
		t.Errorf("err = %v, want it to say the command could not be run", err)
	}
}

func TestTimeoutIsClassifiedAsTimeout(t *testing.T) {
	command, _ := fakeOpenCode(t, "sleep 30")
	o := NewOpenCode(command, "")
	req := openCodeRequest(t)
	req.Timeout = 300 * time.Millisecond

	res, err := o.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerTimedOut || res.FailureKind != task.FailureTimeout {
		t.Errorf("result = %s/%s, want TIMED_OUT/TIMEOUT", res.Status, res.FailureKind)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "300ms") {
		t.Errorf("Err = %v, want the timeout value named", res.Err)
	}
}

func TestCancellationIsClassifiedAsCancelled(t *testing.T) {
	command, _ := fakeOpenCode(t, "sleep 30")
	o := NewOpenCode(command, "")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	res, err := o.Run(ctx, openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerCancelled || res.FailureKind != task.FailureCancelled {
		t.Errorf("result = %s/%s, want CANCELLED/CANCELLED", res.Status, res.FailureKind)
	}
}

// Events are parsed from the live stream, so a run whose output exceeds the
// capture cap still yields a session id, a summary and usage.
func TestEventsSurviveOutputTruncation(t *testing.T) {
	padding := strings.Repeat("z", 4000)
	command, _ := fakeOpenCode(t, emit(
		fixtureStepStart,
		fixtureToolUse,
		`{"type":"text","sessionID":"ses_pad","part":{"type":"text","text":"`+padding+`"}}`,
		fixtureText,
		fixtureStepFinishStop,
	)+"exit 0")

	o := NewOpenCode(command, "")
	req := openCodeRequest(t)
	req.MaxOutputBytes = 1024

	res, err := o.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.StdoutTruncated {
		t.Fatal("expected the capture to be truncated for this test to mean anything")
	}
	if res.Status != task.WorkerSucceeded {
		t.Errorf("status = %s (%v), want SUCCEEDED", res.Status, res.Err)
	}
	if res.SessionID == "" {
		t.Error("session id was lost to truncation")
	}
	if !strings.HasPrefix(res.Summary, "Done. Created") {
		t.Errorf("summary = %q, want the final message despite truncation", res.Summary)
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want it parsed despite truncation", res.FinishReason)
	}
}

func TestRunRejectsInvalidRequest(t *testing.T) {
	o := NewOpenCode("opencode", "")
	res, err := o.Run(context.Background(), Request{})
	if err == nil {
		t.Fatal("an invalid request was accepted")
	}
	if res.FailureKind != task.FailureStartup {
		t.Errorf("failure kind = %s, want STARTUP", res.FailureKind)
	}
}

func TestListAgentsParsesTheCliOutput(t *testing.T) {
	// The shape is from the installed version: names at column zero, indented
	// JSON permissions beneath (docs/research.md §7b).
	command, _ := fakeOpenCode(t, `cat <<'AIDEV_EOF'
build (primary)
  [
  {
    "permission": "*",
    "action": "allow"
  }
  ]
explore (subagent)
plan (primary)
AIDEV_EOF
exit 0`)

	o := NewOpenCode(command, "")
	names, err := o.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	want := []string{"build", "explore", "plan"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestValidateAgentName(t *testing.T) {
	command, _ := fakeOpenCode(t, "printf 'build (primary)\\nplan (primary)\\n'\nexit 0")
	o := NewOpenCode(command, "")

	if err := o.ValidateAgentName(context.Background(), "build"); err != nil {
		t.Errorf("a known agent was rejected: %v", err)
	}
	err := o.ValidateAgentName(context.Background(), "reviewer")
	if err == nil {
		t.Fatal("an unknown agent was accepted")
	}
	if !strings.Contains(err.Error(), "build, plan") {
		t.Errorf("err = %v, want the available agents listed so the user can correct it", err)
	}
	if err := o.ValidateAgentName(context.Background(), ""); err == nil {
		t.Error("an empty agent name was accepted")
	}
}

func TestListAgentsReportsFailures(t *testing.T) {
	command, _ := fakeOpenCode(t, "echo 'boom' >&2\nexit 1")
	if _, err := NewOpenCode(command, "").ListAgents(context.Background()); err == nil {
		t.Error("a failing agent list was accepted")
	}

	command, _ = fakeOpenCode(t, "echo 'unexpected output'\nexit 0")
	if _, err := NewOpenCode(command, "").ListAgents(context.Background()); err == nil {
		t.Error("output with no agents was accepted")
	}
}

func TestNewOpenCodeDefaultsTheCommand(t *testing.T) {
	if got := NewOpenCode("  ", "").command(); got != DefaultOpenCodeCommand {
		t.Errorf("command = %q, want %q", got, DefaultOpenCodeCommand)
	}
	if got := NewOpenCode("/opt/opencode", "").command(); got != "/opt/opencode" {
		t.Errorf("command = %q", got)
	}
}

func TestOpenCodeSatisfiesBackendAndValidator(t *testing.T) {
	var b Backend = NewOpenCode("", "")
	if b.Name() != OpenCodeName {
		t.Errorf("Name() = %q", b.Name())
	}
	if _, ok := b.(Validator); !ok {
		t.Error("OpenCode does not implement Validator")
	}
}

// A process write can split a JSON object anywhere, so the scanner must hold
// partial lines rather than discarding them.
func TestEventScannerHandlesSplitWrites(t *testing.T) {
	s := newEventScanner()
	full := fixtureText + "\n"
	for i := 0; i < len(full); i += 7 {
		end := min(i+7, len(full))
		if _, err := s.Write([]byte(full[i:end])); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	s.Close()

	got := s.Transcript()
	if !strings.HasPrefix(got.Summary, "Done. Created") {
		t.Errorf("summary = %q, want the event reassembled from fragments", got.Summary)
	}
	if got.Lines != 1 {
		t.Errorf("lines = %d, want 1", got.Lines)
	}
}

func TestEventScannerHandlesUnterminatedFinalLine(t *testing.T) {
	s := newEventScanner()
	// No trailing newline, as happens when a process is killed mid-write.
	if _, err := s.Write([]byte(fixtureText)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if s.Transcript().Summary != "" {
		t.Error("an incomplete line was parsed before Close")
	}
	s.Close()
	if s.Transcript().Summary == "" {
		t.Error("Close did not process the trailing line")
	}
}

// OpenCode may add event types. Failing a task because its agent learned a new
// trick would be the wrong response, so unknown types are counted and ignored.
func TestEventScannerToleratesUnknownAndMalformedLines(t *testing.T) {
	s := newEventScanner()
	input := strings.Join([]string{
		`{"type":"brand_new_event","sessionID":"ses_x","part":{}}`,
		`not json at all`,
		``,
		fixtureText,
	}, "\n") + "\n"

	if _, err := s.Write([]byte(input)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Close()

	got := s.Transcript()
	if got.Summary == "" {
		t.Error("a malformed line prevented later events from being parsed")
	}
	if got.UnknownEvents["brand_new_event"] != 1 {
		t.Errorf("unknown events = %v, want brand_new_event counted", got.UnknownEvents)
	}
	if got.UnknownEvents["unparseable"] != 1 {
		t.Errorf("unknown events = %v, want the unparseable line counted", got.UnknownEvents)
	}
}

func TestEventScannerCountsFailedTools(t *testing.T) {
	s := newEventScanner()
	input := fixtureToolUse + "\n" +
		`{"type":"tool_use","sessionID":"s","part":{"type":"tool","tool":"bash","state":{"status":"error"}}}` + "\n"
	if _, err := s.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	got := s.Transcript()
	if got.ToolCalls != 2 {
		t.Errorf("tool calls = %d, want 2", got.ToolCalls)
	}
	if got.FailedTools != 1 {
		t.Errorf("failed tools = %d, want 1", got.FailedTools)
	}
}

// An opt-in check against the installed OpenCode. It is off by default so that
// `make check` never depends on an agent, a model, or a network.
func TestAgainstRealOpenCode(t *testing.T) {
	if os.Getenv("AIDEV_TEST_OPENCODE") == "" {
		t.Skip("set AIDEV_TEST_OPENCODE=1 to exercise the installed opencode")
	}

	o := NewOpenCode(os.Getenv("OPENCODE_COMMAND"), os.Getenv("OPENCODE_MODEL"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	names, err := o.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents against the real opencode: %v", err)
	}
	if !contains(names, "build") {
		t.Errorf("agents = %v, want the build agent present", names)
	}
	if err := o.ValidateAgentName(ctx, "definitely-not-an-agent"); err == nil {
		t.Error("the real opencode accepted a nonexistent agent name")
	}
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
