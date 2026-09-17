package task

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// stepsThatNeedQuoting hold arguments a shell would act on, or that the parser
// would split or reject if they were printed bare. Each was typed quoted, which is
// the only way ParseVerificationStep accepts them.
var stepsThatNeedQuoting = []VerificationStep{
	{Command: "go", Args: []string{"test", "./tests/integration/", "-run", "TestA|TestB", "-count=1"}},
	{Command: "env", Args: []string{"TEST_DATABASE_URL=postgres://u:p@127.0.0.1:5434/db?sslmode=disable", "go", "test", "./..."}},
	{Command: "go", Args: []string{"test", "-run", "TestA TestB"}},
	{Command: "mytool", Args: []string{"--flag", ""}},
	{Command: "echo", Args: []string{"it's"}},
	{Command: "echo", Args: []string{`say "hi"`}},
	{Command: "echo", Args: []string{`both ' and "`}},
	{Command: "echo", Args: []string{"$HOME", "`id`", "a;b", "a&&b", "x>y", "*.go", "[ab]", "{a,b}", "!x", "#c", "~", `back\slash`, "(sub)"}},
	{Command: "echo", Args: []string{"two\nlines", "tab\there"}},
	{Command: "my tool", Args: []string{"x"}},
}

// String is what aidev shows wherever a step is displayed, and above all in the
// prompt that tells the agent how it will be judged. It must read back as the same
// step: TASK-000041's prompt said `-run TestA|TestB`, which parses as a rejected
// pipe, not as the regular expression the task declared.
func TestStepStringParsesBackToTheSameStep(t *testing.T) {
	for _, step := range stepsThatNeedQuoting {
		printed := step.String()
		got, err := ParseVerificationStep(printed)
		if err != nil {
			t.Errorf("%#v printed as %q, which does not parse: %v", step, printed, err)
			continue
		}
		if got.Command != step.Command || !reflect.DeepEqual(got.Args, step.Args) {
			t.Errorf("%q parses as %q %q, want %q %q", printed, got.Command, got.Args, step.Command, step.Args)
		}
	}
}

// An agent reads the printed command and runs it in a shell. The shell must
// receive the same arguments aidev will pass, or the agent checks something other
// than what judges it: `-run TestA|TestB` pasted into bash pipes go test into a
// command named TestB.
func TestStepStringMeansTheSameInAShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not installed")
	}
	const sep = "\x1f"
	for _, step := range stepsThatNeedQuoting {
		// Replace the command with printf, keeping the arguments exactly as String
		// quotes them; the format prints each argument followed by a separator.
		probe := VerificationStep{Command: "printf", Args: append([]string{"%s" + sep}, step.Command)}
		probe.Args = append(probe.Args, step.Args...)
		printed := probe.String()

		// A wrong quoting runs redirections and commands, so keep it away from
		// the source tree.
		cmd := exec.Command("sh", "-c", printed)
		cmd.Dir = t.TempDir()
		out, err := cmd.Output()
		if err != nil {
			t.Errorf("sh -c %q: %v", printed, err)
			continue
		}
		got := strings.Split(strings.TrimSuffix(string(out), sep), sep)
		want := append([]string{step.Command}, step.Args...)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("sh -c %q received %q, want %q", printed, got, want)
		}
	}
}
