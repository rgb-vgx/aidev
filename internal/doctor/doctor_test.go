package doctor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

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
		StuckTasks: func(context.Context, string) ([]string, error) {
			return nil, nil
		},
		CheckWorkspace: func(string) error { return nil },
		FreeSpace:      func(string) (uint64, error) { return 100 << 30, nil },
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
	want := []string{CheckConfig, CheckGit, CheckAgent, CheckDatabase, CheckMigrations, CheckStuckTasks, CheckWorkspace, CheckDisk}
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

	// A new user has no conf.json yet: `aidev setup` writes one.
	assertFail(t, got[CheckConfig], "aidev setup", "AIDEV_CONFIG", "conf.example.json")
	if !strings.Contains(got[CheckConfig].Summary, "AIDEV_CONFIG is not set") {
		t.Errorf("config summary should carry the loader's message, got %q", got[CheckConfig].Summary)
	}
	// git does not depend on the configuration and is still checked.
	if got[CheckGit].Status != StatusOK {
		t.Errorf("git: status %s, want ok even without a configuration", got[CheckGit].Status)
	}
	for _, name := range []string{CheckAgent, CheckDatabase, CheckMigrations, CheckStuckTasks, CheckWorkspace, CheckDisk} {
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
	// `aidev setup` starts the container without a checkout of the repository,
	// which `make db-up` needs.
	assertFail(t, got[CheckDatabase], "aidev setup", "database.url")
	if !strings.Contains(got[CheckDatabase].Summary, "connection refused") {
		t.Errorf("database summary should say why it failed, got %q", got[CheckDatabase].Summary)
	}
	if got[CheckMigrations].Status != StatusSkipped {
		t.Errorf("migrations: status %s, want skipped when the database is unreachable", got[CheckMigrations].Status)
	}
	if migrationsAsked {
		t.Error("migrations were queried although the database did not answer")
	}
	if got[CheckStuckTasks].Status != StatusSkipped {
		t.Errorf("stuck tasks: status %s, want skipped when the database is unreachable", got[CheckStuckTasks].Status)
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
	assertFail(t, got[CheckDatabase], "Docker", "aidev setup", "database.url")
}

func TestPendingMigrationsFailWithTheCommandThatAppliesThem(t *testing.T) {
	deps := healthy()
	deps.PendingMigrations = func(context.Context, string) ([]string, error) {
		return []string{"0004_task_hardness", "0005_protected_paths"}, nil
	}
	// The lease columns the stuck-task check queries may not exist yet, so the
	// check must not ask the database about them while migrations are pending.
	deps.StuckTasks = func(context.Context, string) ([]string, error) {
		t.Fatal("stuck tasks were checked although migrations are pending")
		return nil, nil
	}
	got := byName(t, Run(context.Background(), deps))
	assertFail(t, got[CheckMigrations], "aidev migrate")
	if !strings.Contains(got[CheckMigrations].Summary, "2") {
		t.Errorf("migrations summary should say how many are pending, got %q", got[CheckMigrations].Summary)
	}
	if got[CheckStuckTasks].Status != StatusSkipped {
		t.Errorf("stuck tasks: status %s, want skipped while migrations are pending", got[CheckStuckTasks].Status)
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

// A task whose process died leaves it RUNNING forever; doctor must name it and
// point at the command that clears it. It is a warning, not a failure: aidev
// itself still works, and the operator decides when to recover.
func TestStuckTasksWarnWithTheCommandThatRecoversThem(t *testing.T) {
	deps := healthy()
	deps.StuckTasks = func(context.Context, string) ([]string, error) {
		return []string{"task_0001 (RUNNING)", "task_0002 (VERIFYING)"}, nil
	}
	got := byName(t, Run(context.Background(), deps))
	r := got[CheckStuckTasks]
	if r.Status != StatusWarn {
		t.Errorf("status = %s, want warn (summary %q)", r.Status, r.Summary)
	}
	if !strings.Contains(r.Summary, "task_0001 (RUNNING)") || !strings.Contains(r.Summary, "task_0002 (VERIFYING)") {
		t.Errorf("summary should name the stuck tasks, got %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "2 tasks are") {
		t.Errorf("summary = %q, want \"2 tasks are\"", r.Summary)
	}
	if !strings.Contains(r.Fix, "aidev task recover") {
		t.Errorf("fix %q does not mention `aidev task recover`", r.Fix)
	}
	if Failed(Run(context.Background(), deps)) {
		t.Error("stuck tasks are a warning: a doctor that warns must still exit 0")
	}
}

// One stuck task reads as "1 task is", not "1 tasks are".
func TestOneStuckTaskIsSingular(t *testing.T) {
	deps := healthy()
	deps.StuckTasks = func(context.Context, string) ([]string, error) {
		return []string{"task_0001 (RUNNING)"}, nil
	}
	got := byName(t, Run(context.Background(), deps))
	if s := got[CheckStuckTasks].Summary; !strings.Contains(s, "1 task is stuck") {
		t.Errorf("summary = %q, want it to say \"1 task is stuck\"", s)
	}
}

// The check reports the database's refusal as a failure of the check — the
// operator needs to know it could not look, not to believe nothing is there.
func TestStuckTasksErrorFailsAndSaysWhy(t *testing.T) {
	deps := healthy()
	deps.StuckTasks = func(context.Context, string) ([]string, error) {
		return nil, errors.New("failed to connect to `" + secretURL + "`: connection refused")
	}
	got := byName(t, Run(context.Background(), deps))
	assertFail(t, got[CheckStuckTasks], "aidev task recover")
	if !strings.Contains(got[CheckStuckTasks].Summary, "connection refused") {
		t.Errorf("summary should say why it failed, got %q", got[CheckStuckTasks].Summary)
	}
	for _, r := range []Result{got[CheckConfig], got[CheckDatabase], got[CheckStuckTasks]} {
		if strings.Contains(r.Summary+r.Fix, "hunter2") {
			t.Errorf("%s leaks the database password: %+v", r.Name, r)
		}
	}
}

// The disk check warns while there is still room and fails once a task would
// likely die half-way, and says how to free space either way. A system where
// space cannot be measured skips it rather than guessing.
func TestDiskCheck(t *testing.T) {
	cases := []struct {
		name   string
		free   uint64
		err    error
		status Status
	}{
		{"plenty", 40 << 30, nil, StatusOK},
		{"little", 3 << 30, nil, StatusWarn},
		{"almost none", 200 << 20, nil, StatusFail},
		{"unmeasurable", 0, errors.ErrUnsupported, StatusSkipped},
		{"unreadable", 0, errors.New("statfs: permission denied"), StatusWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := healthy()
			deps.FreeSpace = func(string) (uint64, error) { return tc.free, tc.err }
			got := byName(t, Run(context.Background(), deps))[CheckDisk]
			if got.Status != tc.status {
				t.Fatalf("status = %s, want %s (summary %q)", got.Status, tc.status, got.Summary)
			}
			if (tc.status == StatusWarn || tc.status == StatusFail) && got.Fix == "" {
				t.Error("a warning or failure must say what to do")
			}
			if tc.status == StatusWarn && tc.err == nil && !strings.Contains(got.Fix, "aidev worktree remove") {
				t.Errorf("fix = %q, want it to name the way to reclaim a worktree", got.Fix)
			}
		})
	}

	deps := healthy()
	deps.FreeSpace = nil
	if got := byName(t, Run(context.Background(), deps))[CheckDisk]; got.Status != StatusSkipped {
		t.Errorf("without a way to measure, status = %s, want skipped", got.Status)
	}
}

// When the database does not answer and Docker is installed, the advice
// depends on what Docker says: these used to share one generic sentence, and
// "the daemon is not running" after a reboot looked like a broken container.
func TestDatabaseAdviceFollowsDockerState(t *testing.T) {
	cases := []struct {
		name  string
		state DockerState
		want  string
	}{
		{"daemon down", DockerState{Daemon: DaemonDown}, "daemon is not running"},
		{"permission denied", DockerState{Daemon: DaemonDenied}, "docker group"},
		{"container stopped", DockerState{Daemon: DaemonUp, Container: "exited", Name: "aidev-postgres"}, "docker start aidev-postgres"},
		{"container running", DockerState{Daemon: DaemonUp, Container: "running", Name: "aidev-postgres"}, "database.url"},
		{"no container", DockerState{Daemon: DaemonUp, Name: "aidev-postgres"}, "aidev setup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := healthy()
			deps.PingDatabase = func(context.Context, string) error { return errors.New("connection refused") }
			deps.Docker = func(context.Context) DockerState { return tc.state }
			got := byName(t, Run(context.Background(), deps))[CheckDatabase]
			if got.Status != StatusFail || !strings.Contains(got.Fix, tc.want) {
				t.Errorf("database = %s, fix %q; want fail with a fix mentioning %q", got.Status, got.Fix, tc.want)
			}
		})
	}
}

// After a reboot on a machine where docker.service is disabled and only
// docker.socket is enabled, Docker is not running and PostgreSQL with it, and
// aidev — which reaches PostgreSQL over TCP — never wakes it. The doctor's own
// `docker info` does wake it, through the socket, and the container comes back
// under its restart policy a moment later. Reporting "the container is running
// but the database does not answer at database.url" then sends the person to
// check a password that is fine, and the next run is green with the cause never
// named. The doctor says what happened instead.
func wokenByProbe() DockerState {
	return DockerState{
		Daemon:         DaemonUp,
		Container:      "running",
		Name:           "aidev-postgres",
		ServiceActive:  "inactive",
		ServiceEnabled: "disabled",
		SocketEnabled:  "enabled",
	}
}

func TestDoctorSaysWhenItStartedDockerItself(t *testing.T) {
	deps := healthy()
	pings := 0
	deps.PingDatabase = func(context.Context, string) error {
		pings++
		if pings < 3 {
			return errors.New("connection refused")
		}
		return nil
	}
	deps.Docker = func(context.Context) DockerState { return wokenByProbe() }
	deps.Sleep = func(time.Duration) {}

	results := byName(t, Run(context.Background(), deps))
	got := results[CheckDatabase]
	if got.Status != StatusWarn {
		t.Fatalf("database = %s (%s), want warn: it answers now, after the doctor started Docker", got.Status, got.Summary)
	}
	for _, want := range []string{"Docker was not running", "answers now"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary %q lacks %q", got.Summary, want)
		}
	}
	for _, want := range []string{"docker.service is disabled", "docker.socket", "sudo systemctl enable docker.service"} {
		if !strings.Contains(got.Fix, want) {
			t.Errorf("fix %q lacks %q", got.Fix, want)
		}
	}
	// The database answers, so the checks that need it run.
	if m := results[CheckMigrations]; m.Status != StatusOK {
		t.Errorf("migrations = %s, want ok: the database answered in the end", m.Status)
	}
}

// Woken, but PostgreSQL is still starting when the wait runs out: a failure,
// with the cause named and no word about database.url.
func TestDoctorStartedDockerButPostgresIsNotReadyYet(t *testing.T) {
	deps := healthy()
	deps.PingDatabase = func(context.Context, string) error { return errors.New("connection refused") }
	deps.Docker = func(context.Context) DockerState { return wokenByProbe() }
	deps.Sleep = func(time.Duration) {}

	got := byName(t, Run(context.Background(), deps))[CheckDatabase]
	if got.Status != StatusFail {
		t.Fatalf("database = %s, want fail", got.Status)
	}
	for _, want := range []string{"Docker was not running", "aidev doctor", "sudo systemctl enable docker.service"} {
		if !strings.Contains(got.Fix, want) {
			t.Errorf("fix %q lacks %q", got.Fix, want)
		}
	}
	if strings.Contains(got.Fix, "database.url") {
		t.Errorf("fix %q points at database.url, which is not the problem", got.Fix)
	}
}

// The boot advice follows what systemd reports, and is absent when Docker
// already starts at boot or when there is no systemd to ask.
func TestDockerBootAdvice(t *testing.T) {
	cases := []struct {
		name    string
		state   DockerState
		want    string
		notWant string
	}{
		{"daemon down, service disabled, socket on",
			DockerState{Daemon: DaemonDown, ServiceActive: "inactive", ServiceEnabled: "disabled", SocketEnabled: "enabled"},
			"sudo systemctl enable docker.service", ""},
		{"daemon down, both disabled",
			DockerState{Daemon: DaemonDown, ServiceActive: "inactive", ServiceEnabled: "disabled", SocketEnabled: "disabled"},
			"sudo systemctl enable docker.service", "docker.socket is enabled"},
		{"daemon down, service enabled",
			DockerState{Daemon: DaemonDown, ServiceActive: "failed", ServiceEnabled: "enabled", SocketEnabled: "enabled"},
			"daemon is not running", "systemctl enable"},
		{"daemon down, no systemd",
			DockerState{Daemon: DaemonDown},
			"daemon is not running", "systemctl enable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := healthy()
			deps.PingDatabase = func(context.Context, string) error { return errors.New("connection refused") }
			deps.Docker = func(context.Context) DockerState { return tc.state }
			deps.Sleep = func(time.Duration) {}
			got := byName(t, Run(context.Background(), deps))[CheckDatabase]
			if !strings.Contains(got.Fix, tc.want) {
				t.Errorf("fix %q lacks %q", got.Fix, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got.Fix, tc.notWant) {
				t.Errorf("fix %q should not say %q", got.Fix, tc.notWant)
			}
		})
	}
}

// A container that was already running before the doctor touched Docker is
// not a wake-up: the original advice about database.url stands, and the doctor
// does not wait.
func TestRunningContainerThatWasNotWokenKeepsTheDatabaseURLAdvice(t *testing.T) {
	deps := healthy()
	deps.PingDatabase = func(context.Context, string) error { return errors.New("password authentication failed") }
	state := wokenByProbe()
	state.ServiceActive = "active"
	deps.Docker = func(context.Context) DockerState { return state }
	slept := false
	deps.Sleep = func(time.Duration) { slept = true }

	got := byName(t, Run(context.Background(), deps))[CheckDatabase]
	if got.Status != StatusFail || !strings.Contains(got.Fix, "database.url") {
		t.Errorf("database = %s, fix %q; want fail pointing at database.url", got.Status, got.Fix)
	}
	if slept {
		t.Error("the doctor waited for a database whose Docker was already running")
	}
}
