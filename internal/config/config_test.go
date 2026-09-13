package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// env builds a Lookup from a map, so tests never touch the real environment.
func env(pairs map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

const testDSN = "postgres://u:p@127.0.0.1:5434/aidev?sslmode=disable"

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":   testDSN,
		"WORKSPACE_ROOT": "/tmp/aidev-workspaces",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultTaskTimeout != DefaultTaskTimeout {
		t.Errorf("task timeout = %s, want %s", cfg.DefaultTaskTimeout, DefaultTaskTimeout)
	}
	if cfg.DefaultVerificationTimeout != DefaultVerificationTimeout {
		t.Errorf("verification timeout = %s, want %s", cfg.DefaultVerificationTimeout, DefaultVerificationTimeout)
	}
	if cfg.OpenCodeCommand != DefaultOpenCodeCommand {
		t.Errorf("opencode command = %q, want %q", cfg.OpenCodeCommand, DefaultOpenCodeCommand)
	}
	if cfg.OpenCodeAgent != DefaultOpenCodeAgent {
		t.Errorf("opencode agent = %q, want %q", cfg.OpenCodeAgent, DefaultOpenCodeAgent)
	}
	if cfg.OpenCodeModel != DefaultOpenCodeModel {
		t.Errorf("model = %q, want the measured-fast default %q", cfg.OpenCodeModel, DefaultOpenCodeModel)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("log level = %s, want INFO", cfg.LogLevel)
	}
	if cfg.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Errorf("max output bytes = %d, want %d", cfg.MaxOutputBytes, DefaultMaxOutputBytes)
	}
}

// The default task timeout must stay generous: Phase 0 measured a first OpenCode
// run against an unseen project taking over four minutes.
func TestDefaultTaskTimeoutToleratesOpenCodeColdStart(t *testing.T) {
	if DefaultTaskTimeout < 5*time.Minute {
		t.Errorf("DefaultTaskTimeout = %s, which is shorter than the cold start observed in Phase 0 (>4m)", DefaultTaskTimeout)
	}
}

func TestDatabaseURLIsRequired(t *testing.T) {
	_, err := Load(env(map[string]string{"WORKSPACE_ROOT": "/tmp/x"}))
	if err == nil {
		t.Fatal("a missing DATABASE_URL was accepted")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Errorf("error = %v, want it to name DATABASE_URL", err)
	}
	if !strings.Contains(err.Error(), "example:") {
		t.Errorf("error = %v, want it to show an example", err)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":                 testDSN,
		"WORKSPACE_ROOT":               "/tmp/aidev-workspaces",
		"DEFAULT_TASK_TIMEOUT":         "90s",
		"DEFAULT_VERIFICATION_TIMEOUT": "2m",
		"OPENCODE_COMMAND":             "/opt/opencode/bin/opencode",
		"OPENCODE_MODEL":               "opencode/nemotron-3.5-lightning-free",
		"OPENCODE_AGENT":               "plan",
		"MAX_OUTPUT_BYTES":             "2048",
		"LOG_LEVEL":                    "debug",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultTaskTimeout != 90*time.Second {
		t.Errorf("task timeout = %s, want 90s", cfg.DefaultTaskTimeout)
	}
	if cfg.DefaultVerificationTimeout != 2*time.Minute {
		t.Errorf("verification timeout = %s, want 2m", cfg.DefaultVerificationTimeout)
	}
	if cfg.OpenCodeCommand != "/opt/opencode/bin/opencode" {
		t.Errorf("command = %q", cfg.OpenCodeCommand)
	}
	if cfg.OpenCodeAgent != "plan" {
		t.Errorf("agent = %q, want plan", cfg.OpenCodeAgent)
	}
	if cfg.MaxOutputBytes != 2048 {
		t.Errorf("max output bytes = %d, want 2048", cfg.MaxOutputBytes)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("log level = %s, want DEBUG", cfg.LogLevel)
	}
}

func TestWorkspaceRootIsMadeAbsolute(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":   testDSN,
		"WORKSPACE_ROOT": "relative/dir/../dir",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.HasPrefix(cfg.WorkspaceRoot, "/") {
		t.Errorf("workspace root = %q, want an absolute path", cfg.WorkspaceRoot)
	}
	if strings.Contains(cfg.WorkspaceRoot, "..") {
		t.Errorf("workspace root = %q, want it cleaned", cfg.WorkspaceRoot)
	}
}

func TestWorkspaceRootDefaultsWithoutEnv(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": testDSN}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkspaceRoot == "" {
		t.Fatal("no default workspace root was derived")
	}
	if !strings.HasPrefix(cfg.WorkspaceRoot, "/") {
		t.Errorf("default workspace root = %q, want absolute", cfg.WorkspaceRoot)
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	cases := []struct {
		name  string
		vars  map[string]string
		wants string
	}{
		{"bad duration", map[string]string{"DEFAULT_TASK_TIMEOUT": "30 minutes"}, "is not a duration"},
		{"zero duration", map[string]string{"DEFAULT_TASK_TIMEOUT": "0s"}, "must be positive"},
		{"negative duration", map[string]string{"DEFAULT_VERIFICATION_TIMEOUT": "-1m"}, "must be positive"},
		{"bad log level", map[string]string{"LOG_LEVEL": "verbose"}, "not one of debug"},
		{"bad output bytes", map[string]string{"MAX_OUTPUT_BYTES": "lots"}, "not an integer"},
		{"tiny output bytes", map[string]string{"MAX_OUTPUT_BYTES": "10"}, "at least 1024"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{"DATABASE_URL": testDSN, "WORKSPACE_ROOT": "/tmp/x"}
			for k, v := range tc.vars {
				vars[k] = v
			}
			_, err := Load(env(vars))
			if err == nil {
				t.Fatalf("expected rejection mentioning %q", tc.wants)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wants)
			}
		})
	}
}

func TestAllProblemsReportedTogether(t *testing.T) {
	_, err := Load(env(map[string]string{
		"DEFAULT_TASK_TIMEOUT": "nope",
		"LOG_LEVEL":            "loud",
	}))
	if err == nil {
		t.Fatal("expected rejection")
	}
	msg := err.Error()
	for _, want := range []string{"DATABASE_URL", "DEFAULT_TASK_TIMEOUT", "LOG_LEVEL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %s in one pass, got: %v", want, err)
		}
	}
}

// A connection string carries a password, so it must never be logged verbatim.
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

	cfg := Config{DatabaseURL: "postgres://aidev:topsecret@db/aidev"}
	if got := cfg.Redacted().DatabaseURL; strings.Contains(got, "topsecret") {
		t.Errorf("Redacted() leaked the password: %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo,
		"Warn": slog.LevelWarn, "warning": slog.LevelWarn, " error ": slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %s, want %s", in, got, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Error("trace should be rejected")
	}
}

func TestNilLookupFallsBackToEnvironment(t *testing.T) {
	// Load(nil) must not panic; it reads the real environment, which in the test
	// process has no DATABASE_URL, so a configuration error is the expected
	// outcome rather than a crash.
	if _, err := Load(nil); err == nil {
		t.Skip("environment happens to provide a valid configuration")
	}
}

func TestCleanupPolicyDefaultsToOnSuccess(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": testDSN}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorktreeCleanup != CleanupOnSuccess {
		t.Errorf("cleanup = %s, want on-success", cfg.WorktreeCleanup)
	}
}

func TestCleanupPolicyOverride(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":     testDSN,
		"WORKTREE_CLEANUP": "NEVER",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorktreeCleanup != CleanupNever {
		t.Errorf("cleanup = %s, want never (the value should be case-insensitive)", cfg.WorktreeCleanup)
	}
}

func TestUnknownCleanupPolicyIsRejected(t *testing.T) {
	_, err := Load(env(map[string]string{
		"DATABASE_URL":     testDSN,
		"WORKTREE_CLEANUP": "always",
	}))
	if err == nil {
		t.Fatal("an unknown cleanup policy was accepted")
	}
	// "always" would mean discarding failed work, which aidev does not offer;
	// the error must list what it does offer.
	if !strings.Contains(err.Error(), "on-success") || !strings.Contains(err.Error(), "never") {
		t.Errorf("error = %v, want the valid policies listed", err)
	}
}

func TestCleanupPolicyValidity(t *testing.T) {
	for _, p := range AllCleanupPolicies() {
		if !p.Valid() {
			t.Errorf("%s is listed but reports itself invalid", p)
		}
	}
	if CleanupPolicy("force").Valid() {
		t.Error("an unknown policy reported itself valid")
	}
}

// Unset and explicitly-empty are different intents: the first takes aidev's
// default, the second asks OpenCode to choose. Collapsing them would remove the
// only way to express the second.
func TestEmptyModelMeansLetOpenCodeChoose(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":   testDSN,
		"OPENCODE_MODEL": "",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenCodeModel != "" {
		t.Errorf("model = %q, want empty when OPENCODE_MODEL is explicitly empty", cfg.OpenCodeModel)
	}
}

func TestModelOverrideWins(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":   testDSN,
		"OPENCODE_MODEL": "anthropic/claude-opus-5",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenCodeModel != "anthropic/claude-opus-5" {
		t.Errorf("model = %q", cfg.OpenCodeModel)
	}
}
