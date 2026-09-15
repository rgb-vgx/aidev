// Package cli implements aidev's command line interface.
//
// Commands are kept out of main so that they can be exercised by tests without
// building and spawning a binary: Run takes its arguments and writers as
// parameters rather than reading globals.
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

// UsageError reports a malformed invocation. main turns it into exit status 2,
// distinguishing "you typed it wrong" from "the operation failed".
type UsageError struct {
	Message string
}

func (e *UsageError) Error() string { return e.Message }

func usagef(format string, args ...any) error {
	return &UsageError{Message: fmt.Sprintf(format, args...)}
}

// command is one top-level subcommand.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env *Env, args []string) error
}

// Env carries what every command needs: where to write, and the build version.
type Env struct {
	Version string
	Stdout  io.Writer
	Stderr  io.Writer
}

// Run dispatches a command line. It returns a *UsageError for malformed input.
func Run(ctx context.Context, version string, args []string, stdout, stderr io.Writer) error {
	env := &Env{Version: version, Stdout: stdout, Stderr: stderr}

	commands := map[string]command{
		"version": {
			name:    "version",
			summary: "print the aidev version",
			run:     runVersion,
		},
		"config": {
			name:    "config",
			summary: "print the resolved configuration, with secrets redacted",
			run:     runConfig,
		},
		"migrate": {
			name:    "migrate",
			summary: "apply pending database migrations",
			run:     runMigrate,
		},
		"task": {
			name:    "task",
			summary: "create, inspect and run tasks",
			run:     runTask,
		},
		"worktree": {
			name:    "worktree",
			summary: "inspect and reclaim the worktrees tasks left behind",
			run:     runWorktree,
		},
		"stats": {
			name:    "stats",
			summary: "show outcomes by model and task hardness",
			run:     runStats,
		},
		"mcp": {
			name:    "mcp",
			summary: "run the MCP server on stdio, for Claude Code",
			run:     runMCPServer,
		},
	}

	if len(args) == 0 {
		writeUsage(stderr, commands)
		return usagef("no command given")
	}

	name := args[0]
	switch name {
	case "-h", "--help", "help":
		writeUsage(stdout, commands)
		return nil
	case "-v", "--version":
		return runVersion(ctx, env, nil)
	}

	cmd, ok := commands[name]
	if !ok {
		writeUsage(stderr, commands)
		return usagef("unknown command %q", name)
	}
	return cmd.run(ctx, env, args[1:])
}

func writeUsage(w io.Writer, commands map[string]command) {
	names := make([]string, 0, len(commands))
	width := 0
	for n := range commands {
		names = append(names, n)
		if len(n) > width {
			width = len(n)
		}
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("aidev orchestrates implementation tasks: it isolates each one in a git\n")
	b.WriteString("worktree, runs an agent there, verifies the result itself, and records it.\n\n")
	b.WriteString("usage: aidev <command> [flags]\n\ncommands:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, n, commands[n].summary)
	}
	b.WriteString("\nConfiguration is the conf.json named by the AIDEV_CONFIG environment variable,\n")
	b.WriteString("and nothing else; run `aidev config` to see what aidev resolved from it. Every\n")
	b.WriteString("setting is listed in conf/conf.example.json.\n")
	fmt.Fprint(w, b.String())
}

func runVersion(_ context.Context, env *Env, _ []string) error {
	fmt.Fprintf(env.Stdout, "aidev %s\n", env.Version)
	return nil
}
