package cli

import (
	"bytes"
	"context"
	"flag"
	"strings"
	"testing"

	"aidev/internal/task"
)

func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = Run(context.Background(), "test", args, &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestNoArgumentsIsAUsageError(t *testing.T) {
	_, stderr, err := runCLI(t)
	if err == nil {
		t.Fatal("running with no command was accepted")
	}
	var usage *UsageError
	if !asUsage(err, &usage) {
		t.Fatalf("err = %T, want *UsageError so main can exit 2", err)
	}
	if !strings.Contains(stderr, "commands:") {
		t.Errorf("stderr does not list the commands:\n%s", stderr)
	}
}

func TestUnknownCommand(t *testing.T) {
	_, stderr, err := runCLI(t, "frobnicate")
	if err == nil {
		t.Fatal("an unknown command was accepted")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("err = %v, want it to name the unknown command", err)
	}
	if !strings.Contains(stderr, "task") {
		t.Errorf("stderr should list the real commands:\n%s", stderr)
	}
}

func TestHelpAndVersion(t *testing.T) {
	stdout, _, err := runCLI(t, "help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(stdout, "usage: aidev") {
		t.Errorf("help output = %q", stdout)
	}

	stdout, _, err = runCLI(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(stdout, "aidev test") {
		t.Errorf("version output = %q, want the injected version", stdout)
	}

	stdout, _, err = runCLI(t, "--version")
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if !strings.Contains(stdout, "test") {
		t.Errorf("--version output = %q", stdout)
	}
}

func TestTaskSubcommandUsage(t *testing.T) {
	_, stderr, err := runCLI(t, "task")
	if err == nil {
		t.Fatal("`aidev task` with no subcommand was accepted")
	}
	for _, want := range []string{"create", "run", "list", "get", "result", "cancel", "approve", "events"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("subcommand list is missing %q:\n%s", want, stderr)
		}
	}

	_, _, err = runCLI(t, "task", "frobnicate")
	if err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("err = %v", err)
	}

	// Asking for help is not an error, so a script checking the exit status is
	// not misled.
	if _, _, err := runCLI(t, "task", "--help"); err != nil {
		t.Errorf("task --help returned an error: %v", err)
	}
}

// Commands that take a task must insist on exactly one, so that a typo like
// `aidev task get TASK-1 TASK-2` is refused rather than half-obeyed.
func TestExactlyOneTaskIdentifierIsRequired(t *testing.T) {
	for _, sub := range []string{"get", "run", "result", "cancel", "approve", "events"} {
		t.Run(sub, func(t *testing.T) {
			_, _, err := runCLI(t, "task", sub)
			if err == nil {
				t.Fatalf("`task %s` with no identifier was accepted", sub)
			}
			if !strings.Contains(err.Error(), "required") {
				t.Errorf("err = %v, want it to say an identifier is required", err)
			}

			_, _, err = runCLI(t, "task", sub, "TASK-000001", "TASK-000002")
			if err == nil {
				t.Fatalf("`task %s` with two identifiers was accepted", sub)
			}
			if !strings.Contains(err.Error(), "expected one task") {
				t.Errorf("err = %v, want it to complain about the count", err)
			}
		})
	}
}

// Verification commands are parsed before the database is touched, so a bad one
// fails immediately rather than after connecting.
func TestCreateRejectsShellOperatorsBeforeConnecting(t *testing.T) {
	_, _, err := runCLI(t, "task", "create", "--title", "x", "--verify", "go test ./... | tee log")
	if err == nil {
		t.Fatal("a shell pipe in a verification command was accepted")
	}
	if !strings.Contains(err.Error(), "without a shell") {
		t.Errorf("err = %v, want the explanation about there being no shell", err)
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	_, _, err := runCLI(t, "task", "list", "--nonexistent")
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	var usage *UsageError
	if !asUsage(err, &usage) {
		t.Errorf("err = %T, want *UsageError", err)
	}
}

func TestListRejectsAnInvalidStatusFilter(t *testing.T) {
	_, _, err := runCLI(t, "task", "list", "--status", "NONSENSE")
	if err == nil {
		t.Fatal("an invalid status filter was accepted")
	}
	if !strings.Contains(err.Error(), "PENDING") {
		t.Errorf("err = %v, want the valid statuses listed", err)
	}
}

func TestRepeatableFlag(t *testing.T) {
	var r repeatable
	for _, v := range []string{"go test ./...", "go vet ./..."} {
		if err := r.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(r) != 2 {
		t.Fatalf("collected %d values, want 2", len(r))
	}
	if !strings.Contains(r.String(), "go vet") {
		t.Errorf("String() = %q", r.String())
	}
}

func TestTaskDetailMentionsWhoRunsVerification(t *testing.T) {
	var buf bytes.Buffer
	writeTaskDetail(&buf, task.Task{
		Ref:    "TASK-000001",
		Title:  "Add a Greet function",
		Status: task.StatusPending,
		Agent:  "build",
		Verification: []task.VerificationStep{
			{Command: "go", Args: []string{"test", "./..."}},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "go test ./...") {
		t.Errorf("detail does not show the verification command:\n%s", out)
	}
	// The label matters: a reader must not think the agent self-reports.
	if !strings.Contains(out, "run by aidev") {
		t.Errorf("detail does not say who runs verification:\n%s", out)
	}
}

func TestExitErrorCarriesAStatus(t *testing.T) {
	err := &exitError{code: 1}
	if err.Code() != 1 {
		t.Errorf("Code() = %d, want 1", err.Code())
	}
	if err.Error() == "" {
		t.Error("Error() is empty")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("truncate should leave short input alone, got %q", got)
	}
}

func asUsage(err error, target **UsageError) bool {
	u, ok := err.(*UsageError)
	if ok {
		*target = u
	}
	return ok
}

// `aidev task result TASK-000001 --json` is the natural argument order, and Go's
// flag package stops at the first non-flag argument, so flags must be parsed
// around positionals rather than only before them.
func TestFlagsAreAcceptedAfterThePositionalArgument(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	asJSON := fs.Bool("json", false, "")
	reason := fs.String("reason", "", "")

	positionals, err := parseInterspersed(fs, []string{"TASK-000001", "--json", "--reason", "because"})
	if err != nil {
		t.Fatalf("parseInterspersed: %v", err)
	}
	if len(positionals) != 1 || positionals[0] != "TASK-000001" {
		t.Errorf("positionals = %v, want just the task identifier", positionals)
	}
	if !*asJSON {
		t.Error("--json after the positional was not applied")
	}
	if *reason != "because" {
		t.Errorf("--reason = %q, want it parsed after the positional", *reason)
	}
}

func TestFlagsBeforeAndBetweenPositionals(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	asJSON := fs.Bool("json", false, "")

	positionals, err := parseInterspersed(fs, []string{"--json", "A", "B"})
	if err != nil {
		t.Fatalf("parseInterspersed: %v", err)
	}
	if !*asJSON {
		t.Error("a leading flag was not applied")
	}
	// Two positionals must still be reported, so `get A B` is refused rather than
	// half-obeyed.
	if len(positionals) != 2 {
		t.Errorf("positionals = %v, want both collected so the caller can reject them", positionals)
	}
}
