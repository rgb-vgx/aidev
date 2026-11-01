package task

import (
	"strings"
	"testing"
)

// Quoting a shell operator passes it as a literal argument, which is right
// for a regular expression (`go test -run 'A|B'`) and wrong for command
// substitution: nothing ever runs `$(...)` or `...` when there is no shell,
// so the step judges the literal text. TASK-000154 was judged by
// `test -z '$(gofmt -l internal)'` — test given a non-empty literal string
// is always false — and spent both its attempts on a check that could never
// pass. The old refusal message ("quote it to pass it as a literal
// argument") is what led there.

func TestParseRejectsCommandSubstitutionWithoutAShell(t *testing.T) {
	for _, raw := range []string{
		`test -z '$(gofmt -l internal)'`,
		`test -z "$(gofmt -l internal)"`,
		"echo '`date`'",
		`go test -run 'Test$(whoami)'`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseVerificationStep(raw)
			if err == nil {
				t.Fatalf("ParseVerificationStep(%q) succeeded; command substitution without a shell is a literal that never runs", raw)
			}
			if !strings.Contains(err.Error(), "sh -c") {
				t.Errorf("error %q does not say how to run it through a shell (sh -c '...')", err)
			}
		})
	}
}

// Through a shell, the same text is a script the shell runs: allowed.
func TestParseAllowsCommandSubstitutionForAShell(t *testing.T) {
	for _, raw := range []string{
		`sh -c 'test -z "$(gofmt -l internal)"'`,
		`bash -c 'test -z "$(gofmt -l internal)"'`,
		`/bin/sh -c 'test -z "$(gofmt -l internal)"'`,
		"sh -c 'echo `date`'",
		// A shell started through a wrapper still runs it.
		`env FOO=1 sh -c 'test -z "$(gofmt -l internal)"'`,
		`timeout 60 bash -c 'echo $(date)'`,
	} {
		if _, err := ParseVerificationStep(raw); err != nil {
			t.Errorf("ParseVerificationStep(%q): %v; a shell does run command substitution", raw, err)
		}
	}
}

// A dollar sign that is not a substitution stays a literal, as regular
// expressions need it.
func TestParseKeepsQuotedDollarAnchors(t *testing.T) {
	for _, raw := range []string{
		`go test ./... -run 'TestA$'`,
		`go test ./... -run '^Test(A|B)$'`,
		`grep -q 'price: $5' notes.txt`,
	} {
		if _, err := ParseVerificationStep(raw); err != nil {
			t.Errorf("ParseVerificationStep(%q): %v; a quoted $ that substitutes nothing is a literal", raw, err)
		}
	}
}

// The refusal of an unquoted operator names both ways out: quote it as a
// literal, or run the line through a shell when the operator was meant.
func TestUnquotedOperatorRefusalSuggestsAShell(t *testing.T) {
	_, err := ParseVerificationStep(`go test ./... | tee out.log`)
	if err == nil {
		t.Fatal("an unquoted pipe was accepted")
	}
	if !strings.Contains(err.Error(), "sh -c") {
		t.Errorf("error %q does not mention sh -c for an operator that was meant", err)
	}
}
