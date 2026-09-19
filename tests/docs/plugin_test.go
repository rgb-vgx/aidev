package docs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"aidev/internal/doctor"
	aidevmcp "aidev/internal/mcp"
)

// The Claude Code plugin in plugin/aidev is prose that tells Claude how to drive
// aidev. Nothing compiles it, so these checks hold it to the program: the server it
// registers is `aidev mcp`, and every tool and argument its skills name exists.

const pluginDir = "../../plugin/aidev"

var skillFiles = []string{
	pluginDir + "/skills/delegate/SKILL.md",
	pluginDir + "/skills/review/SKILL.md",
	pluginDir + "/skills/doctor/SKILL.md",
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
}

func TestPluginManifestNamesAidev(t *testing.T) {
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	readJSON(t, pluginDir+"/.claude-plugin/plugin.json", &manifest)
	if manifest.Name != "aidev" || manifest.Version == "" {
		t.Errorf("plugin.json name = %q, version = %q; want name aidev and a version", manifest.Name, manifest.Version)
	}

	var market struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	readJSON(t, "../../.claude-plugin/marketplace.json", &market)
	if len(market.Plugins) != 1 || market.Plugins[0].Name != "aidev" {
		t.Fatalf("marketplace.json must list exactly the aidev plugin, got %+v", market.Plugins)
	}
	src := filepath.Join("../..", market.Plugins[0].Source, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the marketplace source %q has no plugin manifest: %v", market.Plugins[0].Source, err)
	}
}

// The server must be the one this repository builds, configured the only way aidev
// accepts configuration.
func TestPluginRegistersTheAidevMCPServer(t *testing.T) {
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	readJSON(t, pluginDir+"/.mcp.json", &cfg)
	server, ok := cfg.MCPServers["aidev"]
	if !ok || len(cfg.MCPServers) != 1 {
		t.Fatalf(".mcp.json must declare exactly one server named aidev, got %v", cfg.MCPServers)
	}
	if server.Command != "aidev" || !reflect.DeepEqual(server.Args, []string{"mcp"}) {
		t.Errorf("server runs %q %q, want aidev [mcp]", server.Command, server.Args)
	}
	if server.Env["AIDEV_CONFIG"] == "" {
		t.Errorf("the server is not given AIDEV_CONFIG, so it would start unconfigured")
	}
	for name := range server.Env {
		if name != "AIDEV_CONFIG" {
			t.Errorf("the server is given %s; aidev reads no environment variable but AIDEV_CONFIG", name)
		}
	}
}

func knownTools() map[string]bool {
	return map[string]bool{
		aidevmcp.ToolCreateTask: true, aidevmcp.ToolGetTask: true, aidevmcp.ToolListTasks: true,
		aidevmcp.ToolRunTask: true, aidevmcp.ToolCancelTask: true, aidevmcp.ToolGetResult: true,
		aidevmcp.ToolGetEvents: true, aidevmcp.ToolApprove: true,
	}
}

func TestSkillsNameOnlyToolsThatExist(t *testing.T) {
	toolName := regexp.MustCompile(`\baidev_[a-z_]+\b`)
	known := knownTools()
	for _, path := range skillFiles {
		text := readDoc(t, path)
		if !strings.HasPrefix(text, "---\nname: ") || !strings.Contains(text, "\ndescription: ") {
			t.Errorf("%s does not start with skill frontmatter (name, description)", path)
		}
		for _, name := range toolName.FindAllString(text, -1) {
			if !known[name] {
				t.Errorf("%s names the tool %s, which the MCP server does not have", path, name)
			}
		}
	}
}

// The delegate skill tells Claude which arguments to pass to aidev_create_task; each
// must be a field the tool accepts, or the call fails or the value is ignored.
func TestDelegateSkillUsesRealCreateTaskArguments(t *testing.T) {
	fields := map[string]bool{}
	typ := reflect.TypeOf(aidevmcp.CreateTaskInput{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		fields[name] = true
	}

	text := readDoc(t, pluginDir+"/skills/delegate/SKILL.md")
	start := strings.Index(text, "Call `aidev_create_task` with:")
	if start < 0 {
		t.Fatal("the delegate skill no longer describes the aidev_create_task call")
	}
	section := text[start:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	argument := regexp.MustCompile("(?m)^- `([a-z_]+)`:")
	found := argument.FindAllStringSubmatch(section, -1)
	if len(found) == 0 {
		t.Fatal("found no arguments in the aidev_create_task list")
	}
	for _, m := range found {
		if !fields[m[1]] {
			t.Errorf("the delegate skill passes %q, which aidev_create_task does not accept", m[1])
		}
	}
	for _, required := range []string{"repo_path", "title", "verification"} {
		if !strings.Contains(section, "`"+required+"`") {
			t.Errorf("the delegate skill does not mention the required argument %s", required)
		}
	}
}

// The doctor skill explains each check by name; the names must be the ones
// `aidev doctor` prints.
func TestDoctorSkillNamesEveryCheck(t *testing.T) {
	text := readDoc(t, pluginDir+"/skills/doctor/SKILL.md")
	checks := []string{doctor.CheckConfig, doctor.CheckGit, doctor.CheckAgent,
		doctor.CheckDatabase, doctor.CheckMigrations, doctor.CheckWorkspace}
	for _, name := range checks {
		if !strings.Contains(text, "`"+name+"`") {
			t.Errorf("the doctor skill does not name the %s check", name)
		}
	}
	for _, status := range []doctor.Status{doctor.StatusOK, doctor.StatusWarn, doctor.StatusFail, doctor.StatusSkipped} {
		if !strings.Contains(text, "`"+string(status)+"`") {
			t.Errorf("the doctor skill does not explain the %s status", status)
		}
	}
}

// A new user's first problem is having no configuration and no database; the
// doctor skill must know the command that makes both, and its flags for a
// company registry or an existing database.
func TestDoctorSkillOffersAidevSetup(t *testing.T) {
	text := readDoc(t, pluginDir+"/skills/doctor/SKILL.md")
	for _, want := range []string{"`aidev setup`", "--postgres-image", "--database-url", "settings.json"} {
		if !strings.Contains(text, want) {
			t.Errorf("the doctor skill does not mention %s", want)
		}
	}
}
