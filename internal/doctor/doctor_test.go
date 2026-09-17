package doctor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"aidev/internal/config"
)

const secretURL = "postgres://aidev:hunter2@127.0.0.1:5434/aidev?sslmode=disable"

// healthy returns Deps under which every check passes; each test breaks one thing.
func healthy() Deps {
	return Deps{
		LoadConfig: func() (config.Config, error) {
			return config.Config{
				DatabaseURL:     secretURL,
				WorkspaceRoot:   "/home/you/.local/share/aidev/worktrees",
				AgentBackend:    config.BackendOpenCode,
				OpenCodeCommand: "opencode",
				CodexCommand:    "codex",
			}, nil
		},
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		PingDatabase: func(context.Context, string) error {
			return nil
		},
		PendingMigrations: func(context.Context, string) ([]string, error) { return nil, nil },
		CheckWorkspace:    func(string) error { return nil },
	}
}

func byName(t *testing.T, results []Result) map[string]Result {
	t.Helper()
	out := map[string]Result{}
	for _, r := range results {
		out[r.Name] = r
	}
	return out
}

func TestEverythingHealthyReportsEveryCheckInOrder(t *testing.T) {
	results := Run(context.Background(), healthy())

	var names []string
	for _, r := range results {
		names = append(names, r.Name)
		if r.Status != StatusOK {
			t.Errorf("%s: status %s, want ok (summary %q)", r.Name, r.Status, r.Summary)
		}
		if strings.TrimSpace(r.Summary) == "" {
			t.Errorf("%s: an ok result still says what it found", r.Name)
		}
		if r.Fix != "" {
			t.Errorf("%s: an ok result has nothing to fix, got %q", r.Name, r.Fix)
		}
	}
	want := []string{CheckConfig, CheckGit, CheckAgent, CheckDatabase, CheckMigrations, CheckWorkspace}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("checks = %v, want %v", names, want)
	}
	if Failed(results) {
		t.Error("Failed reports a failure when every check passed")
	}
}

// Every problem comes with something the reader can do.
func assertFail(t *testing.T, r Result, fixMustMention ...string) {
	t.Helper()
	if r.Status != StatusFail {
		t.Fatalf("%s: status %s, want fail (summary %q)", r.Name, r.Status, r.Summary)
	}
	if strings.TrimSpace(r.Summary) == "" || strings.TrimSpace(r.Fix) == "" {
		t.Errorf("%s: a failure needs both a summary and a fix, got %+v", r.Name, r)
	}
	for _, want := range fixMustMention {
		if !strings.Contains(r.Fix, want) {
			t.Errorf("%s: fix %q does not mention %q", r.Name, r.Fix, want)
		}
	}
}

func TestUnreadableConfigFailsAndSkipsWhatNeedsIt(t *testing.T) {
	deps := healthy()
	deps.LoadConfig = func() (config.Config, error) {
		return config.Config{}, errors.New("AIDEV_CONFIG is not set: set it to the absolute path of your conf.json")
	}
	called := false
	deps.PingDatabase = func(context.Context, string) error { called = true; return nil }

	results := Run(context.Background(), deps)
	got := byName(t, results)

	assertFail(t, got[CheckConfig], "AIDEV_CONFIG", "conf.example.json")
	if !strings.Contains(got[CheckConfig].Summary, "AIDEV_CONFIG is not set") {
		t.Errorf("config summary should carry the loader's message, got %q", got[CheckConfig].Summary)
	}
	// git does not depend on the configuration and is still checked.
	if got[CheckGit].Status != StatusOK {
		t.Errorf("git: status %s, want ok even without a configuration", got[CheckGit].Status)
	}
	for _, name := range []string{CheckAgent, CheckDatabase, CheckMigrations, CheckWorkspace} {
		if got[name].Status != StatusSkipped {
			t.Errorf("%s: status %s, want skipped when the configuration cannot be read", name, got[name].Status)
		}
		if strings.TrimSpace(got[name].Summary) == "" {
			t.Errorf("%s: a skipped check says why", name)
		}
	}
	if called {
		t.Error("the database was contacted without a configuration")
	}
	if !Failed(results) {
		t.Error("Failed does not report the configuration failure")
	}
}

func TestMissingGitFails(t *testing.T) {
	deps := healthy()
	deps.LookPath = func(name string) (string, error) {
		if name == "git" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	}
	got := byName(t, Run(context.Background(), deps))
	assertFail(t, got[CheckGit], "git")
}

func TestMissingAgentNamesTheSettingForTheConfiguredBackend(t *testing.T) {
	deps := healthy()
	var looked []string
	deps.LookPath = func(name string) (string, error) {
		looked = append(looked, name)
		if name == "opencode" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	}
	got := byName(t, Run(context.Background(), deps))
	assertFail(t, got[CheckAgent], "OpenCode", "agent.opencode.command")

	// With the Codex backend, it is Codex that must be found, not OpenCode.
	deps = healthy()
	deps.LoadConfig = func() (config.Config, error) {
		return config.Config{
			DatabaseURL: secretURL, WorkspaceRoot: "/w",
			AgentBackend: config.BackendCodex, OpenCodeCommand: "opencode", CodexCommand: "/opt/codex/bin/codex",
		}, nil
	}
	looked = nil
	deps.LookPath = func(name string) (string, error) {
		looked = append(looked, name)
		if name == "/opt/codex/bin/codex" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	}
	got = byName(t, Run(context.Background(), deps))
	assertFail(t, got[CheckAgent], "agent.codex.command")
	for _, name := range looked {
		if name == "opencode" {
			t.Error("with the codex backend, the opencode command should not be required")
		}
	}
}

func TestUnreachableDatabaseSuggestsStartingItAndNeverShowsThePassword(t *testing.T) {
	deps := healthy()
	deps.PingDatabase = func(_ context.Context, url string) error {
		// A driver error can quote the whole connection string.
		return errors.New("failed to connect to `" + url + "`: dial tcp 127.0.0.1:5434: connect: connection refused")
	}
	migrationsAsked := false
	deps.PendingMigrations = func(context.Context, string) ([]string, error) {
		migrationsAsked = true
		return nil, nil
	}

	results := Run(context.Background(), deps)
	got := byName(t, results)
	assertFail(t, got[CheckDatabase], "make db-up", "database.url")
	if !strings.Contains(got[CheckDatabase].Summary, "connection refused") {
		t.Errorf("database summary should say why it failed, got %q", got[CheckDatabase].Summary)
	}
	if got[CheckMigrations].Status != StatusSkipped {
		t.Errorf("migrations: status %s, want skipped when the database is unreachable", got[CheckMigrations].Status)
	}
	if migrationsAsked {
		t.Error("migrations were queried although the database did not answer")
	}
	for _, r := range results {
		if strings.Contains(r.Summary+r.Fix, "hunter2") {
			t.Errorf("%s leaks the database password: %+v", r.Name, r)
		}
	}
}

// Without Docker, `make db-up` cannot work; say what to install instead.
func TestUnreachableDatabaseWithoutDockerSaysToInstallIt(t *testing.T) {
	deps := healthy()
	deps.PingDatabase = func(context.Context, string) error { return errors.New("connection refused") }
	deps.LookPath = func(name string) (string, error) {
		if name == "docker" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	}
	got := byName(t, Run(context.Background(), deps))
	assertFail(t, got[CheckDatabase], "Docker", "database.url")
}

func TestPendingMigrationsFailWithTheCommandThatAppliesThem(t *testing.T) {
	deps := healthy()
	deps.PendingMigrations = func(context.Context, string) ([]string, error) {
		return []string{"0004_task_hardness", "0005_protected_paths"}, nil
	}
	got := byName(t, Run(context.Background(), deps))
	assertFail(t, got[CheckMigrations], "aidev migrate")
	if !strings.Contains(got[CheckMigrations].Summary, "2") {
		t.Errorf("migrations summary should say how many are pending, got %q", got[CheckMigrations].Summary)
	}

	deps = healthy()
	deps.PendingMigrations = func(context.Context, string) ([]string, error) {
		return nil, errors.New("migration 0002 was already applied but its file has changed")
	}
	got = byName(t, Run(context.Background(), deps))
	if got[CheckMigrations].Status != StatusFail || !strings.Contains(got[CheckMigrations].Summary, "file has changed") {
		t.Errorf("an error reading migrations must fail and say why, got %+v", got[CheckMigrations])
	}
}

func TestUnusableWorkspaceNamesTheSetting(t *testing.T) {
	deps := healthy()
	deps.CheckWorkspace = func(dir string) error { return errors.New("mkdir " + dir + ": permission denied") }
	got := byName(t, Run(context.Background(), deps))
	assertFail(t, got[CheckWorkspace], "workspace_root")
	if !strings.Contains(got[CheckWorkspace].Summary, "permission denied") {
		t.Errorf("workspace summary should say why, got %q", got[CheckWorkspace].Summary)
	}
}

func TestFailedIgnoresWarningsAndSkips(t *testing.T) {
	if Failed([]Result{{Status: StatusOK}, {Status: StatusWarn}, {Status: StatusSkipped}}) {
		t.Error("warnings and skipped checks are not failures")
	}
	if !Failed([]Result{{Status: StatusOK}, {Status: StatusFail}}) {
		t.Error("a failed check must be reported")
	}
}

// Read by people who may not be developers: one pending migration is "1 migration
// is pending", not "1 migrations are pending".
func TestOnePendingMigrationIsSingular(t *testing.T) {
	deps := healthy()
	deps.PendingMigrations = func(context.Context, string) ([]string, error) {
		return []string{"0005_protected_paths"}, nil
	}
	got := byName(t, Run(context.Background(), deps))
	if s := got[CheckMigrations].Summary; !strings.Contains(s, "1 migration is pending") {
		t.Errorf("summary = %q, want it to say \"1 migration is pending\"", s)
	}
}
