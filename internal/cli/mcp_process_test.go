package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// On 2026-09-15 a Claude Code session started after a reboot launched `aidev mcp`
// one minute before Docker brought PostgreSQL up. aidev exited at startup, the
// client recorded CONNECTION_CLOSED with no reason, and it never tried again, so
// aidev was unusable for the whole session although the database was healthy a
// minute later. These tests run the real binary the way a client launches it.

func buildAidev(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the aidev binary")
	}
	bin := filepath.Join(t.TempDir(), "aidev")
	if out, err := exec.Command("go", "build", "-o", bin, "aidev/cmd/aidev").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// isolatedEnv is a machine with no aidev configuration file, plus vars, so that a
// developer's own config.env or exported variables cannot decide the result.
func isolatedEnv(t *testing.T, vars ...string) []string {
	t.Helper()
	home := t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch {
		case key == "HOME", key == "XDG_CONFIG_HOME", key == "AIDEV_CONFIG",
			key == "DATABASE_URL", key == "WORKSPACE_ROOT", strings.HasPrefix(key, "OTEL_"):
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	return append(env, vars...)
}

// Nothing listens on loopback port 1, so the connection is refused at once: the
// failure a client meets while Docker is still starting.
const unreachableDatabase = "postgres://aidev:aidev@127.0.0.1:1/aidev?sslmode=disable"

// writeConfFile writes the only configuration aidev reads, a conf.json that
// AIDEV_CONFIG names, with a workspace inside the test's own directory.
func writeConfFile(t *testing.T, settings map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	if _, ok := settings["workspace_root"]; !ok {
		settings["workspace_root"] = filepath.Join(dir, "workspaces")
	}
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "conf.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func toolText(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, " ")
}

func TestMCPServerStartsWhileTheDatabaseIsDown(t *testing.T) {
	bin := buildAidev(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "mcp")
	cmd.Env = isolatedEnv(t, "AIDEV_CONFIG="+writeConfFile(t, map[string]any{
		"database": map[string]any{"url": unreachableDatabase},
	}))
	cmd.Stderr = &stderr

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("a client could not connect while the database is down, which is the CONNECTION_CLOSED "+
			"a Claude Code session saw: %v\nstderr:\n%s", err, stderr.String())
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v\nstderr:\n%s", err, stderr.String())
	}
	listed := map[string]bool{}
	for _, tool := range tools.Tools {
		listed[tool.Name] = true
	}
	for _, want := range []string{"aidev_create_task", "aidev_run_task", "aidev_get_task_result", "aidev_list_tasks"} {
		if !listed[want] {
			t.Errorf("%s is not listed while the database is down: listing tools must not need it", want)
		}
	}

	// Twice: the first failure must not take the server down, and the second call
	// must try again rather than repeat a remembered failure.
	for call := 1; call <= 2; call++ {
		started := time.Now()
		res, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name: "aidev_list_tasks", Arguments: map[string]any{"limit": 1},
		})
		if err != nil {
			t.Fatalf("call %d: transport error, so the server did not stay up: %v\nstderr:\n%s", call, err, stderr.String())
		}
		if !res.IsError {
			t.Fatalf("call %d: aidev_list_tasks succeeded with no database", call)
		}
		text := toolText(res)
		if !strings.Contains(strings.ToLower(text), "database") || !strings.Contains(text, "make db-up") {
			t.Errorf("call %d: error = %q, want it to say the database is not reachable and how to start it", call, text)
		}
		if elapsed := time.Since(started); elapsed > 20*time.Second {
			t.Errorf("call %d took %s: a client waiting that long cannot tell a slow answer from a hung server", call, elapsed)
		}
	}
}

// A misconfiguration is not a database that is still starting: waiting will not fix
// it, so the server must still refuse to start and say why.
func TestMCPServerStillRefusesAnInvalidConfiguration(t *testing.T) {
	bin := buildAidev(t)
	cases := []struct {
		name  string
		env   func(t *testing.T) []string
		names string
	}{
		// Subtest names become part of t.TempDir() paths, and error messages quote the
		// configuration file's path, so a name must never contain what is being matched.
		{"variable unset", func(t *testing.T) []string { return isolatedEnv(t) }, "AIDEV_CONFIG"},
		{"file without a database", func(t *testing.T) []string {
			return isolatedEnv(t, "AIDEV_CONFIG="+writeConfFile(t, map[string]any{}))
		}, "database.url is required"},
		// The old variable is ignored, so it cannot rescue a file without a database.
		{"database only in the old variable", func(t *testing.T) []string {
			return isolatedEnv(t, "AIDEV_CONFIG="+writeConfFile(t, map[string]any{}), "DATABASE_URL="+unreachableDatabase)
		}, "database.url is required"},
		// A URL PostgreSQL cannot parse never becomes reachable, so it is a
		// configuration error like any other rather than something to retry on
		// every tool call (docs/research.md 7g).
		{"unparseable database url", func(t *testing.T) []string {
			return isolatedEnv(t, "AIDEV_CONFIG="+writeConfFile(t, map[string]any{
				"database": map[string]any{"url": "postgres://u:p@127.0.0.1:5434/aidev?pool_max_conns=abc"},
			}))
		}, "database.url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, bin, "mcp")
			cmd.Env = tc.env(t)
			cmd.Stdin = strings.NewReader("")
			cmd.Stderr = &stderr

			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() == 0 {
				t.Fatalf("err = %v, want a non-zero exit\nstderr:\n%s", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.names) {
				t.Errorf("stderr does not name %s:\n%s", tc.names, stderr.String())
			}
		})
	}
}

// The usage text is what someone copies when registering the server. --scope project
// writes a .mcp.json into one repository; aidev is meant to be available in every
// repository, which is --scope user (README, docs/mcp-tools.md).
func TestMCPUsageRegistersForEveryProject(t *testing.T) {
	_, stderr, _ := runCLI(t, "mcp", "-h")
	if !strings.Contains(stderr, "claude mcp add --scope user") {
		t.Errorf("usage does not register with --scope user:\n%s", stderr)
	}
	if strings.Contains(stderr, "--scope project") {
		t.Errorf("usage still says --scope project:\n%s", stderr)
	}
}
