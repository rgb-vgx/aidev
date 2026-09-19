package cli

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/config"
)

// `aidev setup` is the first command a new user runs: it prepares the database,
// writes conf.json and migrates, then says exactly what to add where. These runs
// use --database-url, so they need no Docker: port 1 on loopback refuses every
// connection, and TEST_DATABASE_URL (when set) is a database that works.

const unreachableDB = "postgres://aidev:hunter2@127.0.0.1:1/aidev?sslmode=disable&connect_timeout=2"

func TestSetupWritesTheConfigAndReportsAFailedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aidev", "conf.json")

	stdout, stderr, err := runCLI(t, "setup", "--config", path, "--database-url", unreachableDB)
	if err == nil {
		t.Fatalf("aidev setup succeeded with an unreachable database\n%s", stdout)
	}
	var usage *UsageError
	if errors.As(err, &usage) {
		t.Fatalf("a failed migration is not a usage error (exit 2): %v", err)
	}
	if !strings.Contains(err.Error(), "migrat") {
		t.Errorf("error %q does not say the migration failed", err)
	}
	if strings.Contains(stdout+stderr+err.Error(), "hunter2") {
		t.Errorf("the database password is shown:\n%s\n%s\n%v", stdout, stderr, err)
	}
	cfg, loadErr := config.LoadFile(path)
	if loadErr != nil {
		t.Fatalf("setup left no valid conf.json at %s: %v", path, loadErr)
	}
	if cfg.DatabaseURL != unreachableDB {
		t.Errorf("conf.json database.url is not the one given")
	}
}

// Without --config the file goes where a user's configuration belongs on this
// system (os.UserConfigDir: $XDG_CONFIG_HOME on Linux), not into the current
// directory.
func TestSetupDefaultsToTheUserConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))

	_, _, _ = runCLI(t, "setup", "--database-url", unreachableDB)
	want := filepath.Join(home, "xdg", "aidev", "conf.json")
	if _, err := config.LoadFile(want); err != nil {
		t.Errorf("no valid conf.json at the default path %s: %v", want, err)
	}
}

// A relative --config is taken from the current directory and written as an
// absolute path, since AIDEV_CONFIG must work from any directory.
func TestSetupAcceptsARelativeConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, _, err := runCLI(t, "setup", "--config", "conf/conf.json", "--database-url", unreachableDB)
	var usage *UsageError
	if errors.As(err, &usage) {
		t.Fatalf("a relative --config is a usage error: %v", err)
	}
	if _, err := config.LoadFile(filepath.Join(dir, "conf", "conf.json")); err != nil {
		t.Errorf("no valid conf.json under the current directory: %v", err)
	}
}

func TestSetupSucceedsAndSaysWhatToDoNext(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	t.Setenv("AIDEV_CONFIG", "")
	path := filepath.Join(t.TempDir(), "aidev", "conf.json")

	stdout, stderr, err := runCLI(t, "setup", "--config", path, "--database-url", databaseURL)
	if err != nil {
		t.Fatalf("aidev setup: %v\n%s\n%s", err, stdout, stderr)
	}
	for _, want := range []string{
		path,                             // where the config is
		"export AIDEV_CONFIG=",           // the shell profile line
		"settings.json",                  // where Claude Code reads its environment
		`"AIDEV_CONFIG": "` + path + `"`, // the settings.json entry, as JSON
		"aidev doctor",                   // how to check the rest
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not contain %q:\n%s", want, stdout)
		}
	}
	// The password itself may be a common word ("aidev" in development), so look
	// for it where a URL would show it.
	if password := passwordOf(t, databaseURL); password != "" && strings.Contains(stdout+stderr, ":"+password+"@") {
		t.Errorf("the database password is shown:\n%s\n%s", stdout, stderr)
	}

	// Running it again is safe and keeps the file.
	before, _ := os.ReadFile(path)
	if _, _, err := runCLI(t, "setup", "--config", path); err != nil {
		t.Fatalf("aidev setup a second time: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("running setup again changed conf.json")
	}
}

// When AIDEV_CONFIG already names this file there is nothing to add to the shell.
func TestSetupDoesNotAskForAnExportThatIsAlreadyThere(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	path := filepath.Join(t.TempDir(), "conf.json")
	t.Setenv("AIDEV_CONFIG", path)

	stdout, _, err := runCLI(t, "setup", "--config", path, "--database-url", databaseURL)
	if err != nil {
		t.Fatalf("aidev setup: %v", err)
	}
	if strings.Contains(stdout, "export AIDEV_CONFIG=") {
		t.Errorf("setup asks to export AIDEV_CONFIG although it is already %s:\n%s", path, stdout)
	}
}

func passwordOf(t *testing.T, databaseURL string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse: %v", err)
	}
	password, _ := u.User.Password()
	return password
}

// The container flags describe setup's own PostgreSQL; with --database-url there
// is no container, so combining them is a mistake to point out, not to ignore.
func TestSetupRejectsContainerFlagsWithYourOwnDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf.json")
	for _, flagArgs := range [][]string{
		{"--postgres-image", "registry.example.com/postgres:16-alpine"},
		{"--postgres-port", "6543"},
		{"--postgres-volume", "aidev_aidev-pgdata"},
	} {
		args := append([]string{"setup", "--config", path, "--database-url", unreachableDB}, flagArgs...)
		_, _, err := runCLI(t, args...)
		var usage *UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "--database-url") || !strings.Contains(err.Error(), flagArgs[0]) {
			t.Errorf("aidev %v: err = %v, want a usage error naming %s and --database-url", args, err, flagArgs[0])
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("setup wrote conf.json despite the usage error")
	}
}

func TestSetupFlagsAreDocumented(t *testing.T) {
	_, stderr, err := runCLI(t, "setup", "--help")
	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("aidev setup --help: err = %v, want a usage error like every command", err)
	}
	for _, name := range []string{"-config", "-database-url", "-postgres-image", "-postgres-port", "-postgres-volume", "-workspace-root"} {
		if !strings.Contains(stderr, name) {
			t.Errorf("setup --help does not list %s:\n%s", name, stderr)
		}
	}
	if !strings.Contains(stderr, "postgres:16-alpine") {
		t.Errorf("setup --help does not show the default image:\n%s", stderr)
	}
}

func TestSetupIsListedInHelpAndTakesNoArguments(t *testing.T) {
	stdout, _, err := runCLI(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "setup") {
		t.Errorf("help does not list setup:\n%s", stdout)
	}
	_, _, err = runCLI(t, "setup", "extra")
	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Errorf("aidev setup extra: err = %v, want a usage error", err)
	}
}
