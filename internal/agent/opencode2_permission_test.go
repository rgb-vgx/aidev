package agent

import (
	"context"
	"os"
	"strings"
	"testing"
)

// aidev confines OpenCode to its worktree with OPENCODE_PERMISSION
// (docs/research.md §7m). OpenCode 2 no longer reads that variable — the
// 2.2.5 binary does not contain the name — so since 73f1d78 every task ran
// with no external_directory rule at all: measured 2026-11-01, an agent's
// write tool created a file outside its working directory without a word
// (docs/research.md §7p). The same rules given in OPENCODE_CONFIG_CONTENT,
// as {"permission":{"external_directory":{...}}}, are honoured: the outside
// write is refused, the session goes on, and a read dir stays readable.

func externalDirectoryRules(t *testing.T, content map[string]any) map[string]any {
	t.Helper()
	permission, _ := content["permission"].(map[string]any)
	rules, _ := permission["external_directory"].(map[string]any)
	if rules == nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT has no permission.external_directory: %v", content)
	}
	return rules
}

func TestOpenCode2GetsItsDirectoryRulesThroughItsConfiguration(t *testing.T) {
	fake := newIsolationFake(t, `[]`, 0)
	req := openCodeRequest(t)
	req.ReadDirs = []string{"/opt/kingsoft/wps-office/"}
	if _, err := NewOpenCode(fake.command, "").Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rules := externalDirectoryRules(t, fake.configContent(t))
	want := map[string]string{
		"*":                           "deny",
		"/tmp/opencode/*":             "allow",
		"/opt/kingsoft/wps-office":    "allow",
		"/opt/kingsoft/wps-office/**": "allow",
	}
	for pattern, effect := range want {
		if rules[pattern] != effect {
			t.Errorf("external_directory[%q] = %v, want %q (rules %v)", pattern, rules[pattern], effect, rules)
		}
	}
	if len(rules) != len(want) {
		t.Errorf("external_directory = %v, want exactly %v", rules, want)
	}
}

// The rules share the one variable with the provider keys and the MCP
// switch-off: none of the three may replace another.
func TestOpenCode2MergesRulesKeysAndMCPSwitchOff(t *testing.T) {
	fake := newIsolationFake(t, debugConfigFixture, 0)
	backend := NewOpenCode(fake.command, "")
	backend.ProviderKeyEnv = map[string]string{"opencode-go": "OPENCODE_GO_API_KEY"}
	if _, err := backend.Run(context.Background(), openCodeRequest(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	content := fake.configContent(t)
	externalDirectoryRules(t, content)
	if _, ok := content["provider"]; !ok {
		t.Errorf("provider keys lost: %v", content)
	}
	if len(overriddenServers(t, content)) != 3 {
		t.Errorf("MCP switch-off lost: %v", content)
	}
}

// OpenCode 1.x reads OPENCODE_PERMISSION (measured, research §7m) and keeps
// getting it; its configuration gains nothing it did not have.
func TestOpenCode1KeepsItsPermissionVariable(t *testing.T) {
	fake := newIsolationFake(t, `[]`, 0)
	body, err := os.ReadFile(fake.command)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake.command, []byte(strings.Replace(string(body), "opencode v2.2.5", "1.18.35", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOpenCode(fake.command, "").Run(context.Background(), openCodeRequest(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(fake.envFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "OPENCODE_PERMISSION=") {
		t.Error("an OpenCode 1.x run lost OPENCODE_PERMISSION")
	}
	if content := fake.configContent(t); content != nil {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %v for 1.x with no keys, want it unset", content)
	}
}
