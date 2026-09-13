package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A new shell has no environment. Requiring a developer to export a connection
// string before every session, or to hand-edit their shell profile with one, is a
// poor answer to "how do I use this tomorrow" — so aidev reads a config file from
// one fixed location.
//
// The location is fixed rather than the working directory on purpose: aidev is run
// from inside whatever repository a task is about, and reading a .env from there
// would pick up that project's secrets and make behaviour depend on where you stood.

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigFileSuppliesValues(t *testing.T) {
	path := writeConfigFile(t, `
# aidev configuration
DATABASE_URL=postgres://u:p@127.0.0.1:5434/aidev?sslmode=disable

WORKSPACE_ROOT=/tmp/from-file
LOG_LEVEL=debug
`)

	cfg, err := Load(env(map[string]string{"AIDEV_CONFIG": path}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DatabaseURL == "" {
		t.Error("DATABASE_URL was not read from the file")
	}
	if cfg.WorkspaceRoot != "/tmp/from-file" {
		t.Errorf("WorkspaceRoot = %q, want it from the file", cfg.WorkspaceRoot)
	}
	if cfg.LogLevel.String() != "DEBUG" {
		t.Errorf("LogLevel = %s, want DEBUG from the file", cfg.LogLevel)
	}
	if cfg.ConfigFile != path {
		t.Errorf("ConfigFile = %q, want the file that was used to be reported", cfg.ConfigFile)
	}
}

// A real environment variable must win, so that one command can be run differently
// without editing a file.
func TestEnvironmentOverridesTheConfigFile(t *testing.T) {
	path := writeConfigFile(t, "DATABASE_URL=postgres://from-file/aidev\nLOG_LEVEL=error\n")

	cfg, err := Load(env(map[string]string{
		"AIDEV_CONFIG": path,
		"LOG_LEVEL":    "debug",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel.String() != "DEBUG" {
		t.Errorf("LogLevel = %s, want the environment to win over the file", cfg.LogLevel)
	}
	if !strings.Contains(cfg.DatabaseURL, "from-file") {
		t.Errorf("DatabaseURL = %q, want the file value where the environment says nothing", cfg.DatabaseURL)
	}
}

// No file is the normal case for someone who exports variables, and must not be an
// error.
func TestMissingConfigFileIsNotAnError(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"AIDEV_CONFIG": filepath.Join(t.TempDir(), "does-not-exist.env"),
		"DATABASE_URL": testDSN,
	}))
	if err != nil {
		t.Fatalf("a missing config file was treated as an error: %v", err)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile = %q, want empty when no file was read", cfg.ConfigFile)
	}
}

// A file that exists but cannot be understood must fail loudly. Silently ignoring it
// would leave someone staring at a setting that is plainly there and plainly ignored.
func TestMalformedConfigFileIsAnError(t *testing.T) {
	path := writeConfigFile(t, "DATABASE_URL=postgres://x/y\nthis line has no equals sign\n")

	_, err := Load(env(map[string]string{"AIDEV_CONFIG": path}))
	if err == nil {
		t.Fatal("a malformed config file was accepted")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("error = %v, want it to name the line number", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the file", err)
	}
}

// The file is parsed, not executed. There is no shell here, so a value is taken
// literally — the same decision as verification commands, and for the same reason.
func TestConfigFileValuesAreLiteral(t *testing.T) {
	path := writeConfigFile(t, strings.Join([]string{
		`DATABASE_URL=postgres://u:p@127.0.0.1:5434/aidev?sslmode=disable`,
		`OPENCODE_MODEL="quoted/model"`,
		`OPENCODE_AGENT='single/quoted'`,
		`WORKSPACE_ROOT=/tmp/no-$EXPANSION`,
		`OPENCODE_COMMAND=  spaced  `,
	}, "\n"))

	cfg, err := Load(env(map[string]string{"AIDEV_CONFIG": path}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Surrounding quotes are a convention people expect in these files and are
	// stripped; nothing inside them is interpreted.
	if cfg.OpenCodeModel != "quoted/model" {
		t.Errorf("OpenCodeModel = %q, want the quotes stripped", cfg.OpenCodeModel)
	}
	if cfg.OpenCodeAgent != "single/quoted" {
		t.Errorf("OpenCodeAgent = %q, want the quotes stripped", cfg.OpenCodeAgent)
	}
	if !strings.Contains(cfg.WorkspaceRoot, "no-$EXPANSION") {
		t.Errorf("WorkspaceRoot = %q, want $EXPANSION left literal: there is no shell here", cfg.WorkspaceRoot)
	}
	if cfg.OpenCodeCommand != "spaced" {
		t.Errorf("OpenCodeCommand = %q, want surrounding whitespace trimmed", cfg.OpenCodeCommand)
	}
}

// The default location must be under the user's config directory, and must honour
// XDG_CONFIG_HOME, so that it lands where a Linux user expects to find it.
func TestDefaultConfigFileLocation(t *testing.T) {
	dir := t.TempDir()
	aidevDir := filepath.Join(dir, "aidev")
	if err := os.MkdirAll(aidevDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(aidevDir, "config.env")
	if err := os.WriteFile(path, []byte("DATABASE_URL=postgres://xdg/aidev\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(env(map[string]string{"XDG_CONFIG_HOME": dir}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(cfg.DatabaseURL, "xdg") {
		t.Errorf("DatabaseURL = %q, want it read from $XDG_CONFIG_HOME/aidev/config.env", cfg.DatabaseURL)
	}
	if cfg.ConfigFile != path {
		t.Errorf("ConfigFile = %q, want %q", cfg.ConfigFile, path)
	}
}

// DefaultConfigPath is what the documentation and `aidev config` tell a user to
// create, so it must be derivable without loading anything.
func TestDefaultConfigPathIsReportable(t *testing.T) {
	path, err := DefaultConfigPath(env(map[string]string{"XDG_CONFIG_HOME": "/tmp/xdg"}))
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if path != "/tmp/xdg/aidev/config.env" {
		t.Errorf("DefaultConfigPath = %q", path)
	}
}
