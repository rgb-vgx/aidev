package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// OpenCode 2 keeps a provider's API key in its database (table credential), so a task
// run against its own OPENCODE_DB — which aidev does so that tasks can run side by side
// (research §7k) — cannot reach a provider the person connected with `opencode auth`:
// it fails at once with provider.no-route (research §7n). A key named in OpenCode's
// configuration does not depend on the database, so aidev passes one, pointing at an
// environment variable: the secret never passes through aidev, argv or its records.

// configContentFor runs the fake opencode and returns OPENCODE_CONFIG_CONTENT as it was
// started with, or "" when it was not set.
func configContentFor(t *testing.T, keys map[string]string) string {
	t.Helper()
	envFile := filepath.Join(t.TempDir(), "env.txt")
	command, _ := fakeOpenCode(t, "env > "+shellQuote(envFile)+"\nexit 0")
	backend := NewOpenCode(command, "")
	backend.ProviderKeyEnv = keys
	if _, err := backend.Run(context.Background(), openCodeRequest(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(line, "OPENCODE_CONFIG_CONTENT="); ok {
			return value
		}
	}
	return ""
}

func TestOpenCodeGetsProviderKeysAsEnvironmentReferences(t *testing.T) {
	got := configContentFor(t, map[string]string{"opencode-go": "OPENCODE_GO_API_KEY", "zai": "ZAI_KEY"})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT is not JSON: %v: %q", err, got)
	}
	want := map[string]any{"provider": map[string]any{
		"opencode-go": map[string]any{"options": map[string]any{"apiKey": "{env:OPENCODE_GO_API_KEY}"}},
		"zai":         map[string]any{"options": map[string]any{"apiKey": "{env:ZAI_KEY}"}},
	}}
	gotJSON, _ := json.Marshal(parsed)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %s, want %s (a reference OpenCode resolves, never the key)", gotJSON, wantJSON)
	}
}

// Without provider keys configured, no provider setting is added: that would be a
// change nobody asked for. (On OpenCode 2 the variable still carries the
// permission rules, opencode2_permission_test.go.)
func TestOpenCodeWithoutProviderKeysAddsNoProviderSetting(t *testing.T) {
	got := configContentFor(t, nil)
	if got == "" {
		return
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT is not JSON: %v: %q", err, got)
	}
	if _, set := parsed["provider"]; set {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %s, want no provider setting when no key is configured", got)
	}
}
