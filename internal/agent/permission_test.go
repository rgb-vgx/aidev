package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// OpenCode's default for a tool call that reaches outside the working
// directory is "ask", and a headless run cannot answer: it rejects the call
// and the session ends (research §7m). TASK-000091 lost its whole run to one
// read of /opt/kingsoft. Under "deny" the same call comes back to the agent
// as a failed tool call and the session goes on — measured on 1.18.35 — so
// aidev runs OpenCode with external_directory denied, except the
// directories the project lets the agent read.

// permissionFor runs the fake opencode and returns the OPENCODE_PERMISSION it
// was started with, decoded.
func permissionFor(t *testing.T, req Request) map[string]any {
	t.Helper()
	envFile := filepath.Join(t.TempDir(), "env.txt")
	command, _ := fakeOpenCode(t, "env > "+shellQuote(envFile)+"\nexit 0")
	if _, err := NewOpenCode(command, "").Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(line, "OPENCODE_PERMISSION="); ok {
			var got map[string]any
			if err := json.Unmarshal([]byte(value), &got); err != nil {
				t.Fatalf("OPENCODE_PERMISSION is not JSON: %v: %s", err, value)
			}
			return got
		}
	}
	t.Fatalf("opencode was started without OPENCODE_PERMISSION:\n%s", raw)
	return nil
}

func TestOpenCodeDeniesDirectoriesOutsideTheWorktree(t *testing.T) {
	got := permissionFor(t, openCodeRequest(t))
	rules, _ := got["external_directory"].(map[string]any)
	if rules["*"] != "deny" {
		t.Errorf("external_directory = %v, want \"*\": \"deny\" so a refusal is a tool error, not the end of the session", rules)
	}
	// OpenCode's own scratch directory keeps the allow it has by default.
	if rules["/tmp/opencode/*"] != "allow" {
		t.Errorf("external_directory = %v, want OpenCode's /tmp/opencode/* left allowed", rules)
	}
}

func TestOpenCodeMayReadTheDirectoriesTheProjectAllows(t *testing.T) {
	req := openCodeRequest(t)
	req.ReadDirs = []string{"/opt/kingsoft", "/usr/share/doc/"}
	got := permissionFor(t, req)
	rules, _ := got["external_directory"].(map[string]any)
	for _, want := range []string{"/opt/kingsoft", "/opt/kingsoft/**", "/usr/share/doc", "/usr/share/doc/**"} {
		if rules[want] != "allow" {
			t.Errorf("external_directory[%q] = %v, want allow: %v", want, rules[want], rules)
		}
	}
	if rules["*"] != "deny" {
		t.Errorf("external_directory = %v, want everything else still denied", rules)
	}
}
