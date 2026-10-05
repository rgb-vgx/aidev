package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// runLogDirname is where a detached run's stderr log lives, under
// workspace_root. It mirrors internal/mcp's runLogDirname without importing an
// MCP package from the CLI.
const runLogDirname = "run-logs"

type runLogFile struct {
	path     string
	name     string
	stamp    string
	size     int64
	modified time.Time
}

// taskRunLog shows the stderr log a detached run wrote.
func taskRunLog(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task run-log", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	showPath := fs.Bool("path", false, "print only the absolute path of the newest log")
	showAll := fs.Bool("all", false, "print every log, oldest first")
	asJSON := fs.Bool("json", false, "print the logs as JSON")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev task run-log <task> [--path] [--all] [--json]

Shows the stderr log of a run the MCP server started, which is where the
reason lives when a run dies before recording an ending.

By default it prints the newest log for the task: a header line naming the
file and its size, then the file's contents.

flags:
`)
		fs.PrintDefaults()
	}
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task run-log: %v", err)
	}
	identifier, err := oneIdentifier("run-log", positionals)
	if err != nil {
		return err
	}
	if *showPath && *showAll {
		return usagef("aidev task run-log: --path and --all cannot be combined")
	}
	if *showPath && *asJSON {
		return usagef("aidev task run-log: --path and --json cannot be combined")
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	t, err := app.store.ResolveTask(ctx, identifier)
	if err != nil {
		return err
	}
	ref := t.Identifier()

	logs, err := listRunLogs(filepath.Join(app.cfg.WorkspaceRoot, runLogDirname), ref)
	if err != nil {
		return err
	}
	if len(logs) == 0 {
		return fmt.Errorf("%s has no run log: only runs started by the MCP server write one, because aidev task run writes to the terminal", ref)
	}

	if *asJSON {
		selected := logs
		if !*showAll {
			selected = logs[len(logs)-1:]
		}
		out := make([]map[string]any, 0, len(selected))
		for _, l := range selected {
			out = append(out, map[string]any{
				"path":        l.path,
				"size":        l.size,
				"modified_at": l.modified.UTC().Format(time.RFC3339Nano),
			})
		}
		return writeJSON(env.Stdout, out)
	}

	if *showPath {
		fmt.Fprintln(env.Stdout, logs[len(logs)-1].path)
		return nil
	}

	selected := logs
	if !*showAll {
		selected = logs[len(logs)-1:]
	}
	for _, l := range selected {
		fmt.Fprintf(env.Stdout, "%s (%d bytes)\n", l.path, l.size)
		if err := printRunLog(env, l.path); err != nil {
			return err
		}
	}
	return nil
}

// listRunLogs returns the run logs for ref, oldest first. A missing directory
// is no logs rather than an error: a task that never ran under the MCP server
// simply has none.
func listRunLogs(dir, ref string) ([]runLogFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := ref + "-"
	var logs []runLogFile
	for _, e := range entries {
		name := e.Name()
		stamp, ok := runLogStamp(name, prefix)
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		logs = append(logs, runLogFile{
			path:     filepath.Join(dir, name),
			name:     name,
			stamp:    stamp,
			size:     info.Size(),
			modified: info.ModTime(),
		})
	}
	sort.Slice(logs, func(i, j int) bool {
		return compareRunLogStamps(logs[i].stamp, logs[j].stamp) < 0
	})
	return logs, nil
}

// runLogStamp reports whether name is exactly "<ref>-<digits>.log" and returns
// the digits.
func runLogStamp(name, prefix string) (string, bool) {
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(name, prefix)
	if !strings.HasSuffix(rest, ".log") {
		return "", false
	}
	stamp := strings.TrimSuffix(rest, ".log")
	if len(stamp) == 0 {
		return "", false
	}
	for i := 0; i < len(stamp); i++ {
		if stamp[i] < '0' || stamp[i] > '9' {
			return "", false
		}
	}
	return stamp, true
}

// compareRunLogStamps orders the numeric filename parts ascending.
func compareRunLogStamps(a, b string) int {
	trimmedA := strings.TrimLeft(a, "0")
	trimmedB := strings.TrimLeft(b, "0")
	if len(trimmedA) != len(trimmedB) {
		if len(trimmedA) < len(trimmedB) {
			return -1
		}
		return 1
	}
	if trimmedA != trimmedB {
		return strings.Compare(trimmedA, trimmedB)
	}
	// Same numeric value: fewer leading zeros (shorter raw form) is older.
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func printRunLog(env *Env, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmt.Fprint(env.Stdout, string(data))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(env.Stdout)
	}
	return nil
}
