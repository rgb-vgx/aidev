package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aidev/internal/config"
)

// Configuration moved into one conf.json named by AIDEV_CONFIG (TASK-000033). The
// prose did not move with it: README still tells a reader to copy .env.example to
// ~/.config/aidev/config.env and to export DATABASE_URL, none of which aidev reads any
// more. Following it produces "AIDEV_CONFIG is not set" on the first command, which is
// the worst possible first five minutes, and it is exactly the reader a setup is
// written for who cannot tell the instructions from the tool.
//
// These checks are mechanical: they cannot judge whether an explanation is good, but
// they can insist it describes the program that exists.

// docFiles are the documents a person reads to set aidev up or to look a setting up.
var docFiles = []string{
	"../../README.md",
	"../../docs/mcp-tools.md",
	"../../docs/architecture.md",
	"../../docs/database.md",
	"../../docs/guide/getting-started.html",
	"../../docs/guide/debugging.html",
	"../../docs/guide/reference.html",
	"../../docs/guide/vi/getting-started.html",
	"../../docs/guide/vi/debugging.html",
	"../../docs/guide/vi/reference.html",
}

// removedFromTheEnvironment is what the old prose told people to set. aidev reads
// none of them now: AIDEV_CONFIG is the only variable it looks at.
var removedFromTheEnvironment = []string{
	"config.env", ".env.example",
	"DATABASE_URL", "WORKSPACE_ROOT", "DEFAULT_TASK_TIMEOUT", "DEFAULT_VERIFICATION_TIMEOUT",
	"OPENCODE_COMMAND", "OPENCODE_MODEL", "OPENCODE_AGENT", "AGENT_BACKEND",
	"MAX_OUTPUT_BYTES", "WORKTREE_CLEANUP", "LOG_LEVEL", "CODEX_PROFILE", "CODEX_MODEL",
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	return string(body)
}

func TestDocsDoNotTellPeopleToSetVariablesAidevIgnores(t *testing.T) {
	for _, path := range docFiles {
		text := readDoc(t, path)
		for _, removed := range removedFromTheEnvironment {
			// TEST_DATABASE_URL is a test knob, not aidev configuration, and it stays.
			cleaned := strings.ReplaceAll(text, "TEST_DATABASE_URL", "")
			if strings.Contains(cleaned, removed) {
				t.Errorf("%s still names %s, which aidev no longer reads", filepath.Base(path), removed)
			}
		}
	}
}

// The reference table is the reader's map of the file they have to edit, so it must
// name every setting and invent none.
func TestReadmeDocumentsEverySetting(t *testing.T) {
	text := readDoc(t, "../../README.md")

	for _, key := range config.SettingKeys() {
		if !strings.Contains(text, key) {
			t.Errorf("README does not document the setting %s", key)
		}
	}

	// Anything that looks like a dotted setting path in the README must be one.
	known := map[string]bool{}
	for _, key := range config.SettingKeys() {
		known[key] = true
		// A parent path such as "agent.opencode" is a heading, not an invention.
		for i, c := range key {
			if c == '.' {
				known[key[:i]] = true
			}
		}
	}
	dotted := regexp.MustCompile("`([a-z_]+(?:\\.[a-z_]+)+)`")
	for _, m := range dotted.FindAllStringSubmatch(text, -1) {
		if !known[m[1]] {
			t.Errorf("README names %q as a setting, which does not exist", m[1])
		}
	}
}

// The first commands a reader runs. AIDEV_CONFIG is how aidev finds anything, and the
// MCP registration has to carry it or the server starts without configuration.
func TestReadmeSetsUpTheConfigurationFileThatAidevActuallyReads(t *testing.T) {
	text := readDoc(t, "../../README.md")
	for _, want := range []string{"AIDEV_CONFIG", "conf.example.json", "conf.json"} {
		if !strings.Contains(text, want) {
			t.Errorf("README does not mention %s, which is how aidev is configured", want)
		}
	}
	if !strings.Contains(text, "claude mcp add --scope user") {
		t.Errorf("README does not show registering the MCP server for every project")
	}
	if !regexp.MustCompile(`claude mcp add[^\n]*-e AIDEV_CONFIG=`).MatchString(text) {
		t.Errorf("the MCP registration in README does not pass AIDEV_CONFIG, so the server would start unconfigured")
	}
}

// .env.example was the template for config.env. Keeping it after config.env went
// away leaves a file whose only use is to mislead: conf/conf.example.json is the
// template now.
func TestTheOldEnvironmentTemplateIsGone(t *testing.T) {
	if _, err := os.Stat("../../.env.example"); !os.IsNotExist(err) {
		t.Errorf(".env.example still exists (err = %v); conf/conf.example.json replaced it", err)
	}
	if _, err := os.Stat("../../conf/conf.example.json"); err != nil {
		t.Errorf("conf/conf.example.json, the template the docs point at, is missing: %v", err)
	}
}
