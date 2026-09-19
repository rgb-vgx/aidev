package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `aidev doctor` is the first thing to run when aidev does not work. It must print
// every check with what to do, exit non-zero when something is broken (so a script
// or Claude can tell), and never show the database password. These runs need no
// database: port 1 on loopback refuses every connection.

func writeDoctorConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.json")
	body, err := json.Marshal(map[string]any{
		"database":       map[string]any{"url": "postgres://aidev:hunter2@127.0.0.1:1/aidev?sslmode=disable&connect_timeout=2"},
		"workspace_root": filepath.Join(dir, "worktrees"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorReportsEveryCheckAndFailsOnAnUnreachableDatabase(t *testing.T) {
	path := writeDoctorConfig(t)
	t.Setenv("AIDEV_CONFIG", path)

	stdout, stderr, err := runCLI(t, "doctor")
	if err == nil {
		t.Fatalf("aidev doctor succeeded with an unreachable database\n%s", stdout)
	}
	var usage *UsageError
	if errors.As(err, &usage) {
		t.Fatalf("a failed check is not a usage error (exit 2): %v", err)
	}
	for _, name := range []string{"config", "git", "agent", "database", "migrations", "workspace"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("output does not list the %s check:\n%s", name, stdout)
		}
	}
	if !strings.Contains(stdout, "aidev setup") {
		t.Errorf("output does not say how to start the database:\n%s", stdout)
	}
	if strings.Contains(stdout+stderr+err.Error(), "hunter2") {
		t.Errorf("the database password is shown:\n%s\n%s\n%v", stdout, stderr, err)
	}
}

func TestDoctorJSONIsTheListOfResults(t *testing.T) {
	path := writeDoctorConfig(t)
	t.Setenv("AIDEV_CONFIG", path)

	stdout, _, err := runCLI(t, "doctor", "--json")
	if err == nil {
		t.Fatal("aidev doctor --json succeeded with an unreachable database")
	}
	var results []struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Summary string `json:"summary"`
		Fix     string `json:"fix"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &results); jsonErr != nil {
		t.Fatalf("--json output is not a JSON list of results: %v\n%s", jsonErr, stdout)
	}
	status := map[string]string{}
	for _, r := range results {
		status[r.Name] = r.Status
	}
	if status["config"] != "ok" || status["database"] != "fail" || status["migrations"] != "skipped" {
		t.Errorf("statuses = %v, want config ok, database fail, migrations skipped", status)
	}
	if status["workspace"] != "ok" {
		t.Errorf("workspace: status %q, want ok for a creatable directory under a temp dir", status["workspace"])
	}
}

func TestDoctorWithoutConfigurationStillReports(t *testing.T) {
	t.Setenv("AIDEV_CONFIG", "")

	stdout, _, err := runCLI(t, "doctor")
	if err == nil {
		t.Fatal("aidev doctor succeeded without a configuration")
	}
	for _, want := range []string{"config", "AIDEV_CONFIG", "conf.example.json", "git"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, stdout)
		}
	}
}

func TestDoctorIsListedInHelp(t *testing.T) {
	stdout, _, err := runCLI(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "doctor") {
		t.Errorf("help does not list doctor:\n%s", stdout)
	}
}

func TestDoctorTakesNoArguments(t *testing.T) {
	_, _, err := runCLI(t, "doctor", "extra")
	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("aidev doctor extra: err = %v, want a usage error", err)
	}
}
