package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `aidev config` is how someone checks what aidev will actually use. With one
// configuration file, the answer must name that file, show values without secrets,
// and not be swayed by variables that no longer mean anything.
func TestConfigCommandReportsTheFileAndHidesSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.json")
	body, err := json.Marshal(map[string]any{
		"database":       map[string]any{"url": "postgres://aidev:topsecret@127.0.0.1:5434/aidev?sslmode=disable"},
		"workspace_root": filepath.Join(dir, "workspaces"),
		"tracing":        map[string]any{"endpoint": "http://127.0.0.1:4318", "headers": map[string]string{"Authorization": "Bearer topsecret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIDEV_CONFIG", path)
	t.Setenv("DATABASE_URL", "postgres://env:envsecret@elsewhere:5432/env")

	stdout, _, err := runCLI(t, "config")
	if err != nil {
		t.Fatalf("aidev config: %v", err)
	}
	if !strings.Contains(stdout, path) {
		t.Errorf("output does not name the configuration file %s:\n%s", path, stdout)
	}
	if !strings.Contains(stdout, "aidev:***@127.0.0.1:5434") {
		t.Errorf("output does not show the redacted database url:\n%s", stdout)
	}
	for _, leak := range []string{"topsecret", "envsecret", "elsewhere"} {
		if strings.Contains(stdout, leak) {
			t.Errorf("output contains %q:\n%s", leak, stdout)
		}
	}

	stdout, _, err = runCLI(t, "config", "--json")
	if err != nil {
		t.Fatalf("aidev config --json: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, stdout)
	}
	if out["config_file"] != path {
		t.Errorf("config_file = %v, want %s", out["config_file"], path)
	}
	if strings.Contains(stdout, "topsecret") {
		t.Errorf("--json output contains a secret:\n%s", stdout)
	}
}

func TestConfigCommandWithoutAIDEVConfigSaysWhatToSet(t *testing.T) {
	t.Setenv("AIDEV_CONFIG", "")
	_, _, err := runCLI(t, "config")
	if err == nil || !strings.Contains(err.Error(), "AIDEV_CONFIG") {
		t.Fatalf("err = %v, want it to say AIDEV_CONFIG must be set", err)
	}
}
