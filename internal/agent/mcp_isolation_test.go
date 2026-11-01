package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"aidev/internal/task"
)

// OpenCode starts every MCP server its configuration names, wherever that
// configuration comes from: the person's global file, and every opencode.json
// it finds walking up from the working directory — for a worktree under
// ~/.local/share/aidev that walk reaches ~/opencode.json. Measured on OpenCode
// 2.2.5 (2026-11-01): a task's agent saw aidev's own MCP server (8 tools,
// approve included, with AIDEV_CONFIG set by that configuration), gitnexus and
// web-research, which created data/research.db in the worktree and was
// committed with TASK-000143's work.
//
// What switched them off, measured the same day: an OPENCODE_CONFIG_CONTENT
// entry that replaces each server whole — {"type":"local","command":[...],
// "disabled":true} under mcp.servers.<name>. A partial entry ({"enabled":false}
// or {"disabled":true} alone, at mcp.<name> or mcp.servers.<name>) is ignored
// without a word. The names come from `opencode debug config`, run in the
// worktree so the walk-up finds what the run will find.

// debugConfigFixture is `opencode debug config` as OpenCode 2.2.5 prints it,
// cut to the parts that matter: documents carry their settings under info,
// directories carry none.
const debugConfigFixture = `[
  {"type": "document", "path": "/home/u/.config/opencode/opencode.jsonc",
   "info": {"mcp": {"servers": {
     "gitnexus": {"type": "local", "command": ["gitnexus", "mcp"]},
     "aidev": {"type": "local", "command": ["aidev", "mcp"], "environment": {"AIDEV_CONFIG": "/x/conf.json"}}
   }}, "providers": {}}},
  {"type": "directory", "path": "/home/u/.config/opencode"},
  {"type": "document", "path": "/home/u/opencode.json",
   "info": {"mcp": {"servers": {
     "web-research": {"type": "local", "command": ["node", "index.js"], "disabled": false}
   }}}},
  {"type": "document", "path": "/repo/opencode.json", "info": {"$schema": "x"}}
]`

// isolationFake is an opencode stand-in that answers `debug config` with
// listing (recording the directory it was asked in) and records the
// environment of the run itself.
type isolationFake struct {
	command   string
	envFile   string
	listedIn  string
	ranMarker string
}

func newIsolationFake(t *testing.T, listing string, listingExit int) isolationFake {
	t.Helper()
	dir := t.TempDir()
	f := isolationFake{
		command:   filepath.Join(dir, "opencode"),
		envFile:   filepath.Join(dir, "env.txt"),
		listedIn:  filepath.Join(dir, "listed-in.txt"),
		ranMarker: filepath.Join(dir, "ran.txt"),
	}
	listingFile := filepath.Join(dir, "listing.json")
	if err := os.WriteFile(listingFile, []byte(listing), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo 'opencode v2.2.5'; exit 0; fi\n" +
		"if [ \"$1\" = debug ] && [ \"$2\" = config ]; then\n" +
		"  pwd > " + shellQuote(f.listedIn) + "\n" +
		"  cat " + shellQuote(listingFile) + "\n" +
		"  exit " + itoa(listingExit) + "\n" +
		"fi\n" +
		"touch " + shellQuote(f.ranMarker) + "\n" +
		"env > " + shellQuote(f.envFile) + "\n" +
		emit(fixtureText) +
		"exit 0\n"
	if err := os.WriteFile(f.command, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (f isolationFake) configContent(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(f.envFile)
	if err != nil {
		t.Fatalf("the run never started: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(line, "OPENCODE_CONFIG_CONTENT="); ok {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(value), &parsed); err != nil {
				t.Fatalf("OPENCODE_CONFIG_CONTENT is not JSON: %v: %q", err, value)
			}
			return parsed
		}
	}
	return nil
}

// overriddenServers returns the mcp.servers entries of the config content.
func overriddenServers(t *testing.T, content map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	mcp, _ := content["mcp"].(map[string]any)
	servers, _ := mcp["servers"].(map[string]any)
	for name, raw := range servers {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("mcp.servers.%s is %T, want an object", name, raw)
		}
		out[name] = entry
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestOpenCodeSwitchesOffEveryMCPServerItsConfigurationNames(t *testing.T) {
	fake := newIsolationFake(t, debugConfigFixture, 0)
	backend := NewOpenCode(fake.command, "")
	result, err := backend.Run(context.Background(), openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	servers := overriddenServers(t, fake.configContent(t))
	if got, want := strings.Join(sortedKeys(servers), ","), "aidev,gitnexus,web-research"; got != want {
		t.Fatalf("servers switched off = %s, want %s", got, want)
	}
	for name, entry := range servers {
		// The whole entry, because OpenCode ignores a partial one.
		if entry["disabled"] != true {
			t.Errorf("mcp.servers.%s.disabled = %v, want true", name, entry["disabled"])
		}
		if entry["type"] != "local" {
			t.Errorf("mcp.servers.%s.type = %v, want \"local\" (a complete entry)", name, entry["type"])
		}
		if command, _ := entry["command"].([]any); len(command) == 0 {
			t.Errorf("mcp.servers.%s.command = %v, want a non-empty command (a complete entry)", name, entry["command"])
		}
		if _, leaked := entry["environment"]; leaked {
			t.Errorf("mcp.servers.%s carries an environment; the replacement must not repeat the original's settings", name)
		}
	}

	// The record says what the run was not allowed to use.
	if got := strings.Join(result.DisabledMCP, ","); got != "aidev,gitnexus,web-research" {
		t.Errorf("Result.DisabledMCP = %v, want [aidev gitnexus web-research], sorted", result.DisabledMCP)
	}
}

// The switch-off and the provider keys travel in the one variable OpenCode
// reads, so neither may replace the other.
func TestOpenCodeMergesMCPSwitchOffWithProviderKeys(t *testing.T) {
	fake := newIsolationFake(t, debugConfigFixture, 0)
	backend := NewOpenCode(fake.command, "")
	backend.ProviderKeyEnv = map[string]string{"opencode-go": "OPENCODE_GO_API_KEY"}
	if _, err := backend.Run(context.Background(), openCodeRequest(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	content := fake.configContent(t)
	provider, _ := content["provider"].(map[string]any)
	goEntry, _ := provider["opencode-go"].(map[string]any)
	options, _ := goEntry["options"].(map[string]any)
	if options["apiKey"] != "{env:OPENCODE_GO_API_KEY}" {
		t.Errorf("provider key lost from OPENCODE_CONFIG_CONTENT: %v", content)
	}
	if len(overriddenServers(t, content)) != 3 {
		t.Errorf("MCP switch-off lost from OPENCODE_CONFIG_CONTENT: %v", content)
	}
}

// The walk-up starts at the working directory, so the listing must be taken
// there: listed from aidev's own directory it would miss what the worktree's
// ancestors add, and name what they do not.
func TestOpenCodeListsMCPServersFromTheWorktree(t *testing.T) {
	fake := newIsolationFake(t, debugConfigFixture, 0)
	req := openCodeRequest(t)
	if _, err := NewOpenCode(fake.command, "").Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(fake.listedIn)
	if err != nil {
		t.Fatalf("`opencode debug config` was never run: %v", err)
	}
	want, _ := filepath.EvalSymlinks(req.WorkingDir)
	if got := strings.TrimSpace(string(raw)); got != want && got != req.WorkingDir {
		t.Errorf("listing ran in %s, want the worktree %s", got, req.WorkingDir)
	}
}

// A configuration aidev cannot read may name servers it cannot switch off, so
// the run does not start: a startup failure, never a run with unknown tools.
func TestOpenCodeDoesNotRunWhenTheMCPListingFails(t *testing.T) {
	for name, tc := range map[string]struct {
		listing string
		exit    int
	}{
		"listing exits non-zero": {listing: "", exit: 1},
		"listing is not JSON":    {listing: "Error: something", exit: 0},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newIsolationFake(t, tc.listing, tc.exit)
			result, err := NewOpenCode(fake.command, "").Run(context.Background(), openCodeRequest(t))
			if err == nil {
				t.Fatal("Run returned no error for a configuration it could not read")
			}
			if result.Status != task.WorkerFailed || result.FailureKind != task.FailureStartup {
				t.Errorf("status = %s/%s, want FAILED/STARTUP", result.Status, result.FailureKind)
			}
			if _, statErr := os.Stat(fake.ranMarker); !errors.Is(statErr, os.ErrNotExist) {
				t.Error("the agent run started although its MCP servers could not be listed")
			}
		})
	}
}

// With nothing to switch off, no mcp setting is added.
func TestOpenCodeWithoutMCPServersAddsNoSwitchOff(t *testing.T) {
	fake := newIsolationFake(t, `[{"type": "document", "path": "/x/opencode.json", "info": {"$schema": "x"}}]`, 0)
	result, err := NewOpenCode(fake.command, "").Run(context.Background(), openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, set := fake.configContent(t)["mcp"]; set {
		t.Errorf("OPENCODE_CONFIG_CONTENT carries an mcp setting with no server to switch off: %v", fake.configContent(t))
	}
	if len(result.DisabledMCP) != 0 {
		t.Errorf("Result.DisabledMCP = %v, want none", result.DisabledMCP)
	}
}

// OpenCode 1.x has a different configuration shape and no such listing; its
// runs are unchanged.
func TestOpenCode1DoesNotListMCPServers(t *testing.T) {
	fake := newIsolationFake(t, debugConfigFixture, 0)
	body, err := os.ReadFile(fake.command)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "opencode v2.2.5", "1.18.35", 1))
	if err := os.WriteFile(fake.command, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOpenCode(fake.command, "").Run(context.Background(), openCodeRequest(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, statErr := os.Stat(fake.listedIn); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("`opencode debug config` was run against OpenCode 1.x")
	}
}
