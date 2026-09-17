package task

import (
	"fmt"
	"strings"
)

// VerificationStep is one command aidev runs itself to decide whether a task
// actually succeeded. It is stored as argv, never as a shell string: aidev
// executes it directly, with no shell in between.
type VerificationStep struct {
	// Command is the executable name or path.
	Command string `json:"command"`

	// Args are passed to Command verbatim.
	Args []string `json:"args,omitempty"`

	// TimeoutSeconds bounds this step. Zero means "use the configured
	// verification default".
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// String renders the step the way a human would type it. The output parses
// back to the same step via ParseVerificationStep and passes the same
// arguments when run by a POSIX shell.
func (s VerificationStep) String() string {
	parts := make([]string, 0, len(s.Args)+1)
	parts = append(parts, quoteIfNeeded(s.Command))
	for _, a := range s.Args {
		parts = append(parts, quoteIfNeeded(a))
	}
	return strings.Join(parts, " ")
}

// Validate checks that the step is runnable.
func (s VerificationStep) Validate() error {
	if strings.TrimSpace(s.Command) == "" {
		return fmt.Errorf("verification step has an empty command")
	}
	if s.TimeoutSeconds < 0 {
		return fmt.Errorf("verification step %q has a negative timeout", s.String())
	}
	return nil
}

// shellMetacharacters are rejected by ParseVerificationStep.
//
// This is not an injection defence: aidev never invokes a shell, so these
// characters would simply become literal arguments. It is a defence against
// silent misinterpretation. Someone writing "go test ./... | tee out.log"
// expects a pipe; passing "|" and "tee" to go test as arguments would fail in a
// confusing way, so aidev refuses the input and says why.
const shellMetacharacters = "|&;<>()$`\n\r*?[]{}!#~"

// ParseVerificationStep turns a human-typed command line such as
// `go test ./...` into a step. Single and double quotes group arguments; there
// is no variable expansion, globbing, or escaping beyond quoting, because there
// is no shell to provide them.
func ParseVerificationStep(raw string) (VerificationStep, error) {
	fields, err := splitFields(raw)
	if err != nil {
		return VerificationStep{}, err
	}
	if len(fields) == 0 {
		return VerificationStep{}, fmt.Errorf("verification command is empty")
	}
	step := VerificationStep{Command: fields[0]}
	if len(fields) > 1 {
		step.Args = fields[1:]
	}
	return step, step.Validate()
}

// ParseVerificationSteps parses several command lines, reporting which one
// failed rather than only that something did.
func ParseVerificationSteps(raws []string) ([]VerificationStep, error) {
	steps := make([]VerificationStep, 0, len(raws))
	for i, raw := range raws {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		step, err := ParseVerificationStep(raw)
		if err != nil {
			return nil, fmt.Errorf("verification command %d (%q): %w", i+1, raw, err)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// splitFields tokenises a command line on whitespace, honouring single and
// double quotes, and rejects unquoted shell metacharacters.
func splitFields(raw string) ([]string, error) {
	var (
		fields  []string
		current strings.Builder
		inField bool
		quote   rune
	)
	flush := func() {
		if inField {
			fields = append(fields, current.String())
			current.Reset()
			inField = false
		}
	}

	for _, r := range raw {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			inField = true // "" is a real, empty argument
		case r == ' ' || r == '\t':
			flush()
		case strings.ContainsRune(shellMetacharacters, r):
			// "./..." must stay legal, so '.' and '/' are not metacharacters;
			// the set above is limited to characters a shell would act on.
			return nil, fmt.Errorf(
				"unquoted %q is not allowed: aidev runs verification commands directly without a shell, "+
					"so shell operators have no effect; quote it to pass it as a literal argument",
				string(r))
		default:
			inField = true
			current.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %s quote", string(quote))
	}
	flush()
	return fields, nil
}

func quoteIfNeeded(s string) string {
	if isBareSafe(s) {
		return s
	}
	// POSIX single quotes keep everything literal, including newlines and
	// tabs; a single quote itself cannot appear inside them, so close the
	// quote, emit it inside double quotes, and reopen.
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// isBareSafe reports whether s needs no quoting: it is non-empty and made
// only of characters that are safe both for splitFields and for a POSIX
// shell.
func isBareSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			return false
		}
	}
	return true
}
