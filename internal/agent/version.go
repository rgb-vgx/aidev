package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"aidev/internal/procexec"
)

// versionTimeout bounds a `--version` call: it only prints a line, so an agent
// that takes longer is hung, and a hung lookup must not hold up the record of
// the run it describes.
const versionTimeout = 10 * time.Second

// Version implements Versioner by running `opencode --version`.
func (o *OpenCode) Version(ctx context.Context) (string, error) {
	// No history is needed to print a version; an in-memory database keeps
	// the call off the shared store a running task may hold
	// (docs/research.md §7k).
	return commandVersion(ctx, o.command(), []string{"OPENCODE_DB=:memory:"})
}

// Version implements Versioner by running `codex --version`.
func (c *Codex) Version(ctx context.Context) (string, error) {
	return commandVersion(ctx, c.executable(), nil)
}

// commandVersion runs `<command> --version` and returns the first non-empty
// line it prints, trimmed: some tools add a banner or an update notice after
// the version itself.
func commandVersion(ctx context.Context, command string, extraEnv []string) (string, error) {
	dir, err := currentDir()
	if err != nil {
		return "", err
	}
	proc, err := procexec.Run(ctx, procexec.Spec{
		Command:        command,
		Args:           []string{"--version"},
		Dir:            dir,
		Timeout:        versionTimeout,
		MaxOutputBytes: 64 << 10,
		ExtraEnv:       extraEnv,
	})
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", command, err)
	}
	if !proc.Succeeded() {
		return "", fmt.Errorf("%s --version: %s", command, firstLine(proc.Stderr))
	}
	for _, line := range strings.Split(proc.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line, nil
		}
	}
	return "", fmt.Errorf("%s --version printed nothing", command)
}
