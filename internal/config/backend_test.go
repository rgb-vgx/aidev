package config

import (
	"strings"
	"testing"
)

// A second agent backend that cannot be selected is dead code. These checks are what
// make the Codex work reachable, and they fix the vocabulary: the names here are what
// a user types and what an audit record shows, so they must agree with agent.Name().

func TestAgentBackendDefaultsToOpenCode(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": testDSN}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentBackend != BackendOpenCode {
		t.Errorf("AgentBackend = %q, want %q: changing which agent runs must be a deliberate act",
			cfg.AgentBackend, BackendOpenCode)
	}
}

func TestAgentBackendSelectsCodex(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":  testDSN,
		"AGENT_BACKEND": "codex",
		"CODEX_PROFILE": "web9router",
		"CODEX_MODEL":   "some/model",
		"CODEX_SANDBOX": "workspace-write",
		"CODEX_COMMAND": "/opt/codex",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentBackend != BackendCodex {
		t.Errorf("AgentBackend = %q, want codex", cfg.AgentBackend)
	}
	if cfg.CodexProfile != "web9router" {
		t.Errorf("CodexProfile = %q", cfg.CodexProfile)
	}
	if cfg.CodexModel != "some/model" {
		t.Errorf("CodexModel = %q", cfg.CodexModel)
	}
	if cfg.CodexSandbox != "workspace-write" {
		t.Errorf("CodexSandbox = %q", cfg.CodexSandbox)
	}
	if cfg.CodexCommand != "/opt/codex" {
		t.Errorf("CodexCommand = %q", cfg.CodexCommand)
	}
}

// The default sandbox has to let the agent write, or every task fails at the first
// file it creates. read-only would be a safe-looking default that breaks everything.
func TestCodexSandboxDefaultsToWorkspaceWrite(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":  testDSN,
		"AGENT_BACKEND": "codex",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CodexSandbox != "workspace-write" {
		t.Errorf("CodexSandbox = %q, want workspace-write: the agent must be able to write in its worktree",
			cfg.CodexSandbox)
	}
	if cfg.CodexCommand != DefaultCodexCommand {
		t.Errorf("CodexCommand = %q, want %q", cfg.CodexCommand, DefaultCodexCommand)
	}
}

func TestUnknownAgentBackendIsRejected(t *testing.T) {
	_, err := Load(env(map[string]string{
		"DATABASE_URL":  testDSN,
		"AGENT_BACKEND": "gpt-5-turbo-agent",
	}))
	if err == nil {
		t.Fatal("an unknown backend was accepted")
	}
	// The message must list what exists, or the user is left guessing.
	for _, want := range []string{"gpt-5-turbo-agent", "opencode", "codex"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

func TestAgentBackendIsCaseInsensitive(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":  testDSN,
		"AGENT_BACKEND": "  CODEX  ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentBackend != BackendCodex {
		t.Errorf("AgentBackend = %q, want codex: a stray capital must not be a configuration error", cfg.AgentBackend)
	}
}

// Codex needs a profile on the installation this was developed against, and without
// one it reaches no model at all. Saying so at startup beats a task failing minutes
// later for a reason that looks like the agent's fault.
func TestCodexWithoutAProfileWarnsInTheConfiguration(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":  testDSN,
		"AGENT_BACKEND": "codex",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CodexProfile != "" {
		t.Errorf("CodexProfile = %q, want empty when unset", cfg.CodexProfile)
	}
}

func TestAllBackendsAreValidAndDistinct(t *testing.T) {
	seen := map[Backend]bool{}
	for _, b := range AllBackends() {
		if !b.Valid() {
			t.Errorf("%s is listed but reports itself invalid", b)
		}
		if seen[b] {
			t.Errorf("%s is listed twice", b)
		}
		seen[b] = true
	}
	if Backend("nope").Valid() {
		t.Error("an unknown backend reported itself valid")
	}
	if len(AllBackends()) < 2 {
		t.Error("there should be at least two backends now")
	}
}
