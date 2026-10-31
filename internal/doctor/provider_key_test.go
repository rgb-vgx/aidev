package doctor

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"aidev/internal/config"
)

const secretKey = "sk-live-do-not-print-0123456789"

// withProviderKeys returns healthy Deps whose OpenCode configuration names
// keys and whose environment is env.
func withProviderKeys(keys map[string]string, env map[string]string) Deps {
	deps := healthy()
	load := deps.LoadConfig
	deps.LoadConfig = func() (config.Config, error) {
		cfg, err := load()
		cfg.OpenCodeModel = "opencode-go/deepseek-v4.1-flash"
		cfg.OpenCodeProviderKeyEnv = keys
		return cfg, err
	}
	deps.LookupEnv = func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
	return deps
}

func assertNoSecret(t *testing.T, results []Result) {
	t.Helper()
	for _, r := range results {
		if strings.Contains(r.Summary+r.Fix, secretKey) {
			t.Errorf("%s: the key's value must never be shown, got %+v", r.Name, r)
		}
	}
}

func TestProviderKeyPresentKeepsTheAgentCheckOK(t *testing.T) {
	deps := withProviderKeys(
		map[string]string{"opencode-go": "OPENCODE_GO_API_KEY"},
		map[string]string{"OPENCODE_GO_API_KEY": secretKey},
	)
	results := Run(context.Background(), deps)
	assertNoSecret(t, results)
	got := byName(t, results)[CheckAgent]
	if got.Status != StatusOK {
		t.Fatalf("agent: status %s, want ok (summary %q)", got.Status, got.Summary)
	}
}

// The agent would start and then fail with provider.no-route: say so before
// any task runs, naming the variable and the setting, never a value.
func TestMissingProviderKeyFailsTheAgentCheck(t *testing.T) {
	deps := withProviderKeys(
		map[string]string{"opencode-go": "OPENCODE_GO_API_KEY"},
		map[string]string{},
	)
	results := Run(context.Background(), deps)
	got := byName(t, results)[CheckAgent]
	assertFail(t, got, "OPENCODE_GO_API_KEY", "agent.opencode.provider_key_env")
	if !strings.Contains(got.Summary, "OPENCODE_GO_API_KEY") || !strings.Contains(got.Summary, "opencode-go") {
		t.Errorf("summary %q should name both the variable and the provider", got.Summary)
	}
	if !Failed(results) {
		t.Error("a missing provider key must make the doctor fail")
	}
}

func TestBlankProviderKeyCountsAsMissing(t *testing.T) {
	deps := withProviderKeys(
		map[string]string{"opencode-go": "OPENCODE_GO_API_KEY"},
		map[string]string{"OPENCODE_GO_API_KEY": "  \t"},
	)
	assertFail(t, byName(t, Run(context.Background(), deps))[CheckAgent], "OPENCODE_GO_API_KEY")
}

// Every missing variable is named, in a stable order, while a present one is not.
func TestEveryMissingProviderKeyIsNamed(t *testing.T) {
	deps := withProviderKeys(
		map[string]string{"zeta": "ZETA_KEY", "alpha": "ALPHA_KEY", "opencode-go": "OPENCODE_GO_API_KEY"},
		map[string]string{"OPENCODE_GO_API_KEY": secretKey},
	)
	results := Run(context.Background(), deps)
	assertNoSecret(t, results)
	got := byName(t, results)[CheckAgent]
	assertFail(t, got, "ALPHA_KEY", "ZETA_KEY")
	a, z := strings.Index(got.Summary, "ALPHA_KEY"), strings.Index(got.Summary, "ZETA_KEY")
	if a < 0 || z < 0 || a > z {
		t.Errorf("summary %q should name ALPHA_KEY then ZETA_KEY", got.Summary)
	}
	if strings.Contains(got.Summary, "OPENCODE_GO_API_KEY") {
		t.Errorf("summary %q names a variable that is set", got.Summary)
	}
}

// A missing executable is still the first thing to fix.
func TestMissingAgentCommandWinsOverMissingKey(t *testing.T) {
	deps := withProviderKeys(map[string]string{"opencode-go": "OPENCODE_GO_API_KEY"}, map[string]string{})
	deps.LookPath = func(name string) (string, error) {
		if name == "opencode" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	}
	assertFail(t, byName(t, Run(context.Background(), deps))[CheckAgent], "agent.opencode.command")
}

// provider_key_env is an OpenCode setting; the Codex backend does not read it.
func TestCodexBackendIgnoresProviderKeys(t *testing.T) {
	deps := withProviderKeys(map[string]string{"opencode-go": "OPENCODE_GO_API_KEY"}, map[string]string{})
	load := deps.LoadConfig
	deps.LoadConfig = func() (config.Config, error) {
		cfg, err := load()
		cfg.AgentBackend = config.BackendCodex
		return cfg, err
	}
	if got := byName(t, Run(context.Background(), deps))[CheckAgent]; got.Status != StatusOK {
		t.Errorf("agent: status %s, want ok with the codex backend (summary %q)", got.Status, got.Summary)
	}
}

// Without a LookupEnv, the check reads the real process environment.
func TestNilLookupEnvReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("AIDEV_DOCTOR_TEST_KEY", secretKey)
	deps := withProviderKeys(map[string]string{"opencode-go": "AIDEV_DOCTOR_TEST_KEY"}, nil)
	deps.LookupEnv = nil
	results := Run(context.Background(), deps)
	assertNoSecret(t, results)
	if got := byName(t, results)[CheckAgent]; got.Status != StatusOK {
		t.Errorf("agent: status %s, want ok (summary %q)", got.Status, got.Summary)
	}
}
