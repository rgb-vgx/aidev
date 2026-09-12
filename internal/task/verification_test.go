package task

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseVerificationStep(t *testing.T) {
	cases := []struct {
		in      string
		command string
		args    []string
	}{
		{"go test ./...", "go", []string{"test", "./..."}},
		{"  go   vet   ./...  ", "go", []string{"vet", "./..."}},
		{"go", "go", nil},
		{`go test -run "TestA TestB"`, "go", []string{"test", "-run", "TestA TestB"}},
		{`echo 'single quoted'`, "echo", []string{"single quoted"}},
		{`make lint`, "make", []string{"lint"}},
		{`/usr/bin/env go test ./pkg/...`, "/usr/bin/env", []string{"go", "test", "./pkg/..."}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			step, err := ParseVerificationStep(tc.in)
			if err != nil {
				t.Fatalf("ParseVerificationStep(%q): %v", tc.in, err)
			}
			if step.Command != tc.command {
				t.Errorf("command = %q, want %q", step.Command, tc.command)
			}
			if !reflect.DeepEqual(step.Args, tc.args) {
				t.Errorf("args = %#v, want %#v", step.Args, tc.args)
			}
		})
	}
}

// Shell operators are rejected rather than silently passed through as literal
// arguments, because aidev runs commands without a shell and a silently
// misinterpreted pipe is much harder to debug than a refusal.
func TestParseRejectsShellOperators(t *testing.T) {
	for _, in := range []string{
		"go test ./... | tee out.log",
		"go test ./... > out.log",
		"go build && go test ./...",
		"go test ./...; go vet ./...",
		"echo $HOME",
		"cat <file",
		"echo `whoami`",
		"rm -rf /tmp/*",
	} {
		t.Run(in, func(t *testing.T) {
			_, err := ParseVerificationStep(in)
			if err == nil {
				t.Fatalf("%q was accepted; a shell operator must be refused", in)
			}
			if !strings.Contains(err.Error(), "without a shell") {
				t.Errorf("error should explain that there is no shell, got: %v", err)
			}
		})
	}
}

func TestQuotingAllowsLiteralMetacharacters(t *testing.T) {
	step, err := ParseVerificationStep(`go test -run "Test|Other"`)
	if err != nil {
		t.Fatalf("a quoted metacharacter should be allowed as a literal: %v", err)
	}
	want := []string{"test", "-run", "Test|Other"}
	if !reflect.DeepEqual(step.Args, want) {
		t.Errorf("args = %#v, want %#v", step.Args, want)
	}
}

func TestParseRejectsUnterminatedQuote(t *testing.T) {
	if _, err := ParseVerificationStep(`go test -run "Unclosed`); err == nil {
		t.Fatal("an unterminated quote should be rejected")
	} else if !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("error = %v, want it to mention the unterminated quote", err)
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		if _, err := ParseVerificationStep(in); err == nil {
			t.Errorf("%q was accepted as a command", in)
		}
	}
}

func TestEmptyQuotesAreARealArgument(t *testing.T) {
	step, err := ParseVerificationStep(`mytool --flag ""`)
	if err != nil {
		t.Fatalf("ParseVerificationStep: %v", err)
	}
	want := []string{"--flag", ""}
	if !reflect.DeepEqual(step.Args, want) {
		t.Errorf("args = %#v, want %#v: an explicitly empty argument must survive", step.Args, want)
	}
}

func TestParseVerificationStepsSkipsBlanksAndReportsPosition(t *testing.T) {
	steps, err := ParseVerificationSteps([]string{"go test ./...", "  ", "go vet ./..."})
	if err != nil {
		t.Fatalf("ParseVerificationSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2 (blank lines skipped)", len(steps))
	}

	_, err = ParseVerificationSteps([]string{"go test ./...", "bad | pipe"})
	if err == nil {
		t.Fatal("expected the second command to be rejected")
	}
	if !strings.Contains(err.Error(), "command 2") {
		t.Errorf("error = %v, want it to name which command failed", err)
	}
}

func TestStepStringRoundTrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"go test ./...", "go test ./..."},
		{`go test -run "TestA TestB"`, `go test -run "TestA TestB"`},
		{`mytool --flag ""`, `mytool --flag ""`},
	}
	for _, tc := range cases {
		step, err := ParseVerificationStep(tc.in)
		if err != nil {
			t.Fatalf("ParseVerificationStep(%q): %v", tc.in, err)
		}
		if got := step.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestStepValidate(t *testing.T) {
	if err := (VerificationStep{Command: "go"}).Validate(); err != nil {
		t.Errorf("a bare command should be valid: %v", err)
	}
	if err := (VerificationStep{Command: " "}).Validate(); err == nil {
		t.Error("a blank command should be invalid")
	}
	if err := (VerificationStep{Command: "go", TimeoutSeconds: -1}).Validate(); err == nil {
		t.Error("a negative timeout should be invalid")
	}
}
