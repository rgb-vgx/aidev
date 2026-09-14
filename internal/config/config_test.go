package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"aidev/internal/tracing"
)

// Configuration lives in one JSON file named by AIDEV_CONFIG and nowhere else, as
// decided with the user on 2026-09-15: no environment variable overrides it, and the
// old config.env is gone. Everything the environment-based loader guaranteed —
// defaults, validation, "an empty model lets OpenCode choose", every problem reported
// in one pass — is kept, now stated against the file.

const testDSN = "postgres://u:p@127.0.0.1:5434/aidev?sslmode=disable"

var minimal = `{"database": {"url": "` + testDSN + `"}}`

// process is a process environment holding exactly pairs.
func process(pairs map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

func writeConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conf.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, body string) (Config, error) {
	t.Helper()
	return Load(process(map[string]string{"AIDEV_CONFIG": writeConf(t, body)}))
}

func mustLoad(t *testing.T, body string) Config {
	t.Helper()
	cfg, err := load(t, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadRequiresAIDEVConfig(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset": nil,
		"empty": {"AIDEV_CONFIG": "  "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(process(env))
			if err == nil {
				t.Fatal("Load without AIDEV_CONFIG succeeded")
			}
			for _, want := range []string{"AIDEV_CONFIG", "conf.json"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %s", err, want)
				}
			}
		})
	}
}

// A relative path resolves against whatever directory the process started in. For
// the MCP server that is the repository Claude Code has open, not aidev's, so a
// relative AIDEV_CONFIG is refused rather than read from the wrong place.
func TestAIDEVConfigMustBeAbsolute(t *testing.T) {
	_, err := Load(process(map[string]string{"AIDEV_CONFIG": "conf/conf.json"}))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v, want a relative AIDEV_CONFIG refused as not absolute", err)
	}
}

func TestMissingConfigFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	_, err := Load(process(map[string]string{"AIDEV_CONFIG": path}))
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want it to name %s", err, path)
	}
}

func TestMalformedJSONNamesTheFileAndLine(t *testing.T) {
	path := writeConf(t, "{\n  \"database\": {\n    \"url\": \"x\",\n  }\n}\n")
	_, err := Load(process(map[string]string{"AIDEV_CONFIG": path}))
	if err == nil {
		t.Fatal("malformed JSON was accepted")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "line 4") {
		t.Errorf("error = %v, want the file and line 4", err)
	}
}

// A misspelt key would otherwise be ignored silently and the default used instead,
// which is exactly the kind of mistake nobody finds by reading the file again.
func TestUnknownSettingIsRejected(t *testing.T) {
	_, err := load(t, `{"database": {"url": "`+testDSN+`"}, "agent": {"opencode": {"modle": "m"}}}`)
	if err == nil || !strings.Contains(err.Error(), "agent.opencode.modle") {
		t.Fatalf("error = %v, want the misspelt setting named by its full path", err)
	}
}

func TestWrongTypeNamesTheSetting(t *testing.T) {
	_, err := load(t, `{"database": {"url": "`+testDSN+`"}, "tasks": {"max_output_bytes": "lots"}}`)
	if err == nil || !strings.Contains(err.Error(), "tasks.max_output_bytes") {
		t.Fatalf("error = %v, want the setting with the wrong type named", err)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeConf(t, minimal)
	cfg, err := Load(process(map[string]string{"AIDEV_CONFIG": path}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"config file", cfg.ConfigFile, path},
		{"database url", cfg.DatabaseURL, testDSN},
		{"task timeout", cfg.DefaultTaskTimeout, 30 * time.Minute},
		{"verification timeout", cfg.DefaultVerificationTimeout, 10 * time.Minute},
		{"backend", cfg.AgentBackend, BackendOpenCode},
		{"opencode command", cfg.OpenCodeCommand, DefaultOpenCodeCommand},
		{"opencode model", cfg.OpenCodeModel, DefaultOpenCodeModel},
		{"opencode agent", cfg.OpenCodeAgent, DefaultOpenCodeAgent},
		{"codex command", cfg.CodexCommand, DefaultCodexCommand},
		{"codex profile", cfg.CodexProfile, ""},
		{"codex model", cfg.CodexModel, ""},
		{"codex sandbox", cfg.CodexSandbox, "workspace-write"},
		{"max output bytes", cfg.MaxOutputBytes, DefaultMaxOutputBytes},
		{"worktree cleanup", cfg.WorktreeCleanup, CleanupOnSuccess},
		{"log level", cfg.LogLevel, slog.LevelInfo},
		{"tracing endpoint", cfg.Tracing.Endpoint, ""},
		{"tracing traces endpoint", cfg.Tracing.TracesEndpoint, ""},
		{"tracing service name", cfg.Tracing.ServiceName, ""},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cfg.Tracing.SampleRatio != nil || len(cfg.Tracing.Headers) != 0 {
		t.Errorf("tracing = %+v, want no sample ratio and no headers when unset", cfg.Tracing)
	}
	if !filepath.IsAbs(cfg.WorkspaceRoot) || !strings.HasSuffix(cfg.WorkspaceRoot, filepath.Join("aidev", "worktrees")) {
		t.Errorf("workspace root = %q, want the per-user default ending in aidev/worktrees", cfg.WorkspaceRoot)
	}
}

func TestDefaultTaskTimeoutToleratesOpenCodeColdStart(t *testing.T) {
	if DefaultTaskTimeout < 5*time.Minute {
		t.Errorf("default task timeout %s is shorter than an observed cold start", DefaultTaskTimeout)
	}
}

func TestDatabaseURLIsRequired(t *testing.T) {
	_, err := load(t, `{}`)
	if err == nil {
		t.Fatal("a missing database.url was accepted")
	}
	if !strings.Contains(err.Error(), "database.url is required") || !strings.Contains(err.Error(), "example:") {
		t.Errorf("error = %v, want database.url named with an example", err)
	}
}

func TestEverySettingIsRead(t *testing.T) {
	cfg := mustLoad(t, `{
  "database": {"url": "postgres://a:b@db:5432/other"},
  "workspace_root": "/srv/aidev/worktrees",
  "tasks": {"timeout": "45m", "verification_timeout": "2m", "max_output_bytes": 4096, "worktree_cleanup": "never"},
  "agent": {
    "backend": "codex",
    "opencode": {"command": "/opt/opencode", "model": "anthropic/claude-opus-5", "agent": "plan"},
    "codex": {"command": "/opt/codex", "profile": "web9router", "model": "gpt-5", "sandbox": "read-only"}
  },
  "log_level": "debug",
  "tracing": {
    "endpoint": "http://127.0.0.1:4318",
    "traces_endpoint": "http://127.0.0.1:4318/v1/traces",
    "headers": {"Authorization": "Bearer secret"},
    "service_name": "aidev-test",
    "sample_ratio": 0.25
  }
}`)
	checks := []struct {
		name      string
		got, want any
	}{
		{"database url", cfg.DatabaseURL, "postgres://a:b@db:5432/other"},
		{"workspace root", cfg.WorkspaceRoot, filepath.Clean("/srv/aidev/worktrees")},
		{"task timeout", cfg.DefaultTaskTimeout, 45 * time.Minute},
		{"verification timeout", cfg.DefaultVerificationTimeout, 2 * time.Minute},
		{"max output bytes", cfg.MaxOutputBytes, 4096},
		{"worktree cleanup", cfg.WorktreeCleanup, CleanupNever},
		{"backend", cfg.AgentBackend, BackendCodex},
		{"opencode command", cfg.OpenCodeCommand, "/opt/opencode"},
		{"opencode model", cfg.OpenCodeModel, "anthropic/claude-opus-5"},
		{"opencode agent", cfg.OpenCodeAgent, "plan"},
		{"codex command", cfg.CodexCommand, "/opt/codex"},
		{"codex profile", cfg.CodexProfile, "web9router"},
		{"codex model", cfg.CodexModel, "gpt-5"},
		{"codex sandbox", cfg.CodexSandbox, "read-only"},
		{"log level", cfg.LogLevel, slog.LevelDebug},
		{"tracing endpoint", cfg.Tracing.Endpoint, "http://127.0.0.1:4318"},
		{"tracing traces endpoint", cfg.Tracing.TracesEndpoint, "http://127.0.0.1:4318/v1/traces"},
		{"tracing service name", cfg.Tracing.ServiceName, "aidev-test"},
		{"tracing authorization header", cfg.Tracing.Headers["Authorization"], "Bearer secret"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cfg.Tracing.SampleRatio == nil || *cfg.Tracing.SampleRatio != 0.25 {
		t.Errorf("tracing sample ratio = %v, want 0.25", cfg.Tracing.SampleRatio)
	}
}

// The user chose one source of configuration. A variable left over in a shell
// profile must not quietly win over the file someone is reading and editing.
func TestEnvironmentVariablesAreIgnored(t *testing.T) {
	path := writeConf(t, `{"database": {"url": "`+testDSN+`"}, "workspace_root": "/srv/from-file"}`)
	cfg, err := Load(process(map[string]string{
		"AIDEV_CONFIG":                path,
		"DATABASE_URL":                "postgres://env:env@elsewhere/env",
		"WORKSPACE_ROOT":              "/srv/from-env",
		"AGENT_BACKEND":               "codex",
		"OPENCODE_MODEL":              "from/env",
		"LOG_LEVEL":                   "debug",
		"DEFAULT_TASK_TIMEOUT":        "1s",
		"MAX_OUTPUT_BYTES":            "2048",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://from-env:4318",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DatabaseURL != testDSN || cfg.WorkspaceRoot != filepath.Clean("/srv/from-file") {
		t.Errorf("database/workspace = %q/%q, want the file's values", cfg.DatabaseURL, cfg.WorkspaceRoot)
	}
	if cfg.AgentBackend != BackendOpenCode || cfg.OpenCodeModel != DefaultOpenCodeModel || cfg.LogLevel != slog.LevelInfo ||
		cfg.DefaultTaskTimeout != DefaultTaskTimeout || cfg.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Errorf("an environment variable changed the configuration: %+v", cfg)
	}
	if cfg.Tracing.Endpoint != "" {
		t.Errorf("tracing endpoint = %q, want OTEL_EXPORTER_OTLP_ENDPOINT ignored", cfg.Tracing.Endpoint)
	}
}

// The MCP server runs with the working directory of whatever repository is open, so
// a relative workspace_root can only mean relative to the file that states it.
func TestRelativeWorkspaceRootIsRelativeToTheConfigFile(t *testing.T) {
	path := writeConf(t, `{"database": {"url": "`+testDSN+`"}, "workspace_root": "worktrees"}`)
	cfg, err := Load(process(map[string]string{"AIDEV_CONFIG": path}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(filepath.Dir(path), "worktrees"); cfg.WorkspaceRoot != want {
		t.Errorf("workspace root = %q, want %q", cfg.WorkspaceRoot, want)
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	cases := []struct {
		name, fragment string
		wants          []string
	}{
		{"bad duration", `"tasks": {"timeout": "30 minutes"}`, []string{"tasks.timeout", "is not a duration"}},
		{"zero duration", `"tasks": {"timeout": "0s"}`, []string{"tasks.timeout", "must be positive"}},
		{"negative duration", `"tasks": {"verification_timeout": "-1m"}`, []string{"tasks.verification_timeout", "must be positive"}},
		{"tiny output bytes", `"tasks": {"max_output_bytes": 10}`, []string{"tasks.max_output_bytes", "at least 1024"}},
		{"unknown cleanup policy", `"tasks": {"worktree_cleanup": "delete"}`, []string{"tasks.worktree_cleanup", "on-success"}},
		{"bad log level", `"log_level": "verbose"`, []string{"log_level", "not one of debug"}},
		{"unknown backend", `"agent": {"backend": "gemini"}`, []string{"agent.backend", "opencode", "codex"}},
		{"sample ratio above 1", `"tracing": {"sample_ratio": 2}`, []string{"tracing.sample_ratio"}},
		{"empty header name", `"tracing": {"headers": {"": "x"}}`, []string{"tracing.headers"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, `{"database": {"url": "`+testDSN+`"}, `+tc.fragment+`}`)
			if err == nil {
				t.Fatalf("accepted; want rejection mentioning %q", tc.wants)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestAllProblemsReportedTogether(t *testing.T) {
	_, err := load(t, `{"tasks": {"timeout": "nope"}, "log_level": "loud"}`)
	if err == nil {
		t.Fatal("expected rejection")
	}
	for _, want := range []string{"database.url", "tasks.timeout", "log_level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s in one pass, got: %v", want, err)
		}
	}
}

// An absent model takes the default; an explicitly empty one is how a user asks
// OpenCode to choose. The file must keep both intents expressible.
func TestEmptyModelMeansLetOpenCodeChoose(t *testing.T) {
	cfg := mustLoad(t, `{"database": {"url": "`+testDSN+`"}, "agent": {"opencode": {"model": ""}}}`)
	if cfg.OpenCodeModel != "" {
		t.Errorf("model = %q, want empty when agent.opencode.model is explicitly empty", cfg.OpenCodeModel)
	}
}

func TestBackendIsCaseInsensitiveAndTrimmed(t *testing.T) {
	cfg := mustLoad(t, `{"database": {"url": "`+testDSN+`"}, "agent": {"backend": " Codex "}}`)
	if cfg.AgentBackend != BackendCodex {
		t.Errorf("backend = %q, want codex", cfg.AgentBackend)
	}
	if cfg.CodexSandbox != "workspace-write" {
		t.Errorf("codex sandbox = %q, want workspace-write by default: read-only would break every task", cfg.CodexSandbox)
	}
}

func TestRedaction(t *testing.T) {
	cases := []struct{ in, want string }{
		{"postgres://aidev:secret@127.0.0.1:5434/aidev", "postgres://aidev:***@127.0.0.1:5434/aidev"},
		{"postgres://aidev@127.0.0.1:5434/aidev", "postgres://aidev:***@127.0.0.1:5434/aidev"},
		{"postgres://127.0.0.1:5434/aidev", "postgres://127.0.0.1:5434/aidev"},
		{"", ""},
		{"host=127.0.0.1 user=aidev", "host=127.0.0.1 user=aidev"},
	}
	for _, tc := range cases {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	headers := map[string]string{"Authorization": "Bearer topsecret"}
	cfg := Config{
		DatabaseURL: "postgres://aidev:topsecret@db/aidev",
		Tracing:     tracing.Settings{Headers: headers},
	}
	red := cfg.Redacted()
	if strings.Contains(red.DatabaseURL, "topsecret") {
		t.Errorf("Redacted() leaked the database password: %q", red.DatabaseURL)
	}
	if strings.Contains(red.Tracing.Headers["Authorization"], "topsecret") {
		t.Errorf("Redacted() leaked a tracing header value: %q", red.Tracing.Headers["Authorization"])
	}
	if headers["Authorization"] != "Bearer topsecret" {
		t.Error("Redacted() modified the original configuration's headers")
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo,
		"Warn": slog.LevelWarn, "warning": slog.LevelWarn, " error ": slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Error("trace should be rejected")
	}
}

func TestEnumerationsAreValidAndDistinct(t *testing.T) {
	seenPolicy := map[CleanupPolicy]bool{}
	for _, p := range AllCleanupPolicies() {
		if !p.Valid() || seenPolicy[p] {
			t.Errorf("cleanup policy %q is invalid or listed twice", p)
		}
		seenPolicy[p] = true
	}
	seenBackend := map[Backend]bool{}
	for _, b := range AllBackends() {
		if !b.Valid() || seenBackend[b] {
			t.Errorf("backend %q is invalid or listed twice", b)
		}
		seenBackend[b] = true
	}
	if CleanupPolicy("delete").Valid() || Backend("nope").Valid() {
		t.Error("an unknown value reported itself valid")
	}
}

// SettingKeys is the vocabulary of the file, for the example, `aidev config` and
// the reference documentation to be checked against.
var wantKeys = []string{
	"agent.backend",
	"agent.codex.command", "agent.codex.model", "agent.codex.profile", "agent.codex.sandbox",
	"agent.opencode.agent", "agent.opencode.command", "agent.opencode.model",
	"database.url",
	"log_level",
	"tasks.max_output_bytes", "tasks.timeout", "tasks.verification_timeout", "tasks.worktree_cleanup",
	"tracing.endpoint", "tracing.headers", "tracing.sample_ratio", "tracing.service_name", "tracing.traces_endpoint",
	"workspace_root",
}

func TestSettingKeysNameEverySetting(t *testing.T) {
	got := SettingKeys()
	if !sort.StringsAreSorted(got) {
		t.Errorf("SettingKeys() is not sorted: %q", got)
	}
	if strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("SettingKeys() = %q\nwant %q", got, wantKeys)
	}
}

// flatten collects dotted leaf paths, treating any path that is itself a setting as
// a leaf so that a map-valued setting such as tracing.headers is one key.
func flatten(prefix string, value any, settings map[string]bool, out map[string]bool) {
	if settings[prefix] {
		out[prefix] = true
		return
	}
	obj, ok := value.(map[string]any)
	if !ok {
		out[prefix] = true
		return
	}
	for k, v := range obj {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		flatten(key, v, settings, out)
	}
}

// The example is what people copy, so it must show every setting and must itself
// load: an example that does not work teaches the wrong thing first.
func TestExampleConfigListsEverySettingAndLoads(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "conf", "conf.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("conf/conf.example.json is missing: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("conf/conf.example.json is not JSON: %v", err)
	}
	settings := map[string]bool{}
	for _, k := range SettingKeys() {
		settings[k] = true
	}
	present := map[string]bool{}
	flatten("", doc, settings, present)
	for k := range settings {
		if !present[k] {
			t.Errorf("conf/conf.example.json does not show %s", k)
		}
	}
	for k := range present {
		if !settings[k] {
			t.Errorf("conf/conf.example.json has %s, which is not a setting", k)
		}
	}
	if _, err := LoadFile(path); err != nil {
		t.Errorf("conf/conf.example.json does not load: %v", err)
	}
}

// conf.json holds the database password and possibly tracing credentials.
func TestLocalConfigFileIsIgnoredByGit(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "/conf/conf.json" {
			return
		}
	}
	t.Error(".gitignore does not ignore /conf/conf.json")
}
