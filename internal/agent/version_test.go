package agent

import (
	"context"
	"strings"
	"testing"
)

// The installed agent's version is recorded with every worker run, so two runs
// of one task that behave differently can be told apart when the agent was
// upgraded in between. Each backend asks its own executable.

func TestOpenCodeReportsItsVersion(t *testing.T) {
	command, argsFile := fakeOpenCode(t, `echo "1.18.35"`)
	got, err := NewOpenCode(command, "").Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "1.18.35" {
		t.Errorf("Version = %q, want 1.18.35", got)
	}
	if args := readArgs(t, argsFile); len(args) != 1 || args[0] != "--version" {
		t.Errorf("opencode was called with %q, want only --version", args)
	}
}

func TestCodexReportsItsVersion(t *testing.T) {
	command, argsFile := fakeCodex(t, `echo "codex-cli 0.155.1"`)
	got, err := NewCodex(CodexOptions{Command: command}).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "codex-cli 0.155.1" {
		t.Errorf("Version = %q, want codex-cli 0.155.1", got)
	}
	if args := readArgs(t, argsFile); len(args) != 1 || args[0] != "--version" {
		t.Errorf("codex was called with %q, want only --version", args)
	}
}

// Only the first line counts: some tools print a banner or an update notice
// after the version.
func TestVersionKeepsTheFirstNonEmptyLine(t *testing.T) {
	command, _ := fakeOpenCode(t, `printf '\n  1.18.35  \nA new version is available\n'`)
	got, err := NewOpenCode(command, "").Version(context.Background())
	if err != nil || got != "1.18.35" {
		t.Errorf("Version = %q, %v; want 1.18.35", got, err)
	}
}

func TestVersionFailsOnABrokenCommand(t *testing.T) {
	cases := map[string]string{
		"exits non-zero": `echo "unknown option" >&2; exit 3`,
		"prints nothing": `exit 0`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			command, _ := fakeOpenCode(t, script)
			got, err := NewOpenCode(command, "").Version(context.Background())
			if err == nil {
				t.Fatalf("Version = %q, want an error", got)
			}
			if name == "exits non-zero" && !strings.Contains(err.Error(), "unknown option") {
				t.Errorf("error %q does not carry the command's own message", err)
			}
		})
	}
}

// Both real backends offer it; a backend need not.
var (
	_ Versioner = (*OpenCode)(nil)
	_ Versioner = (*Codex)(nil)
)
