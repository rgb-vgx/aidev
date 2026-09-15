package cli

import (
	"strings"
	"testing"
)

// aidev reads nothing from the environment but AIDEV_CONFIG (config.Load), so a
// help text naming OPENCODE_AGENT, DEFAULT_TASK_TIMEOUT, DATABASE_URL or
// .env.example tells a person to set something that has no effect, and hides the
// key in conf.json that does. The removed names are also exactly what an older
// session or a copied command line still uses, so the text has to be the thing
// that corrects them.
var removedFromTheEnvironment = []string{
	"OPENCODE_AGENT", "OPENCODE_MODEL", "OPENCODE_COMMAND", "AGENT_BACKEND",
	"DEFAULT_TASK_TIMEOUT", "VERIFICATION_TIMEOUT", "MAX_OUTPUT_BYTES", "WORKTREE_CLEANUP",
	"DATABASE_URL", "WORKSPACE_ROOT", "LOG_LEVEL", "CODEX_PROFILE",
	".env.example", "config.env",
}

func helpOf(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, _ := runCLI(t, args...)
	return stdout + stderr
}

func TestHelpNamesNoVariableAidevStoppedReading(t *testing.T) {
	commands := [][]string{
		{},
		{"config", "-h"},
		{"mcp", "-h"},
		{"migrate", "-h"},
		{"task", "-h"},
		{"task", "create", "-h"},
		{"task", "run", "-h"},
		{"task", "list", "-h"},
		{"task", "show", "-h"},
		{"worktree", "-h"},
	}
	for _, args := range commands {
		name := "aidev " + strings.Join(args, " ")
		text := helpOf(t, args...)
		for _, removed := range removedFromTheEnvironment {
			if strings.Contains(text, removed) {
				t.Errorf("%s names %s, which aidev no longer reads:\n%s", name, removed, text)
			}
		}
	}
}

// The two flags whose default comes from configuration must say which setting it
// is, in the dotted form conf.json uses, so a reader can find the line to edit.
func TestTaskCreateHelpNamesTheConfJSONSettings(t *testing.T) {
	text := helpOf(t, "task", "create", "-h")
	for flag, setting := range map[string]string{"agent": "agent.opencode.agent", "timeout": "tasks.timeout"} {
		if !strings.Contains(text, setting) {
			t.Errorf("--%s does not name its setting %s:\n%s", flag, setting, text)
		}
	}
}

// The overview is where someone learns where configuration lives at all.
func TestTheOverviewPointsAtTheConfigurationFile(t *testing.T) {
	text := helpOf(t)
	if !strings.Contains(text, "conf.json") || !strings.Contains(text, "AIDEV_CONFIG") {
		t.Errorf("the overview does not say configuration is the conf.json named by AIDEV_CONFIG:\n%s", text)
	}
}
