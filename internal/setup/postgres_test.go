package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"aidev/internal/config"
)

// fakeDocker answers `docker inspect` from a script of states and records every
// other command. Errors mimic docker 29.8 as measured: a missing container makes
// inspect exit 1 with "error: no such object: <name>".
type fakeDocker struct {
	exists  bool
	status  string   // .State.Status
	healths []string // successive .State.Health.Status answers; the last repeats
	daemon  error    // when set, every command fails with it
	calls   [][]string
}

func (f *fakeDocker) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.daemon != nil {
		return "", f.daemon
	}
	switch args[0] {
	case "inspect":
		if !f.exists {
			return "", fmt.Errorf("exit status 1: error: no such object: %s", args[len(args)-1])
		}
		format := strings.Join(args, " ")
		if strings.Contains(format, "Health") {
			h := f.healths[0]
			if len(f.healths) > 1 {
				f.healths = f.healths[1:]
			}
			return h + "\n", nil
		}
		return f.status + "\n", nil
	case "run":
		f.exists, f.status = true, "running"
		return "0123456789abcdef\n", nil
	case "start":
		f.status = "running"
		return args[len(args)-1] + "\n", nil
	}
	return "", fmt.Errorf("unexpected docker %v", args)
}

func (f *fakeDocker) commands() []string {
	var out []string
	for _, c := range f.calls {
		if c[0] != "inspect" {
			out = append(out, c[0])
		}
	}
	return out
}

func (f *fakeDocker) runArgs(t *testing.T) []string {
	t.Helper()
	for _, c := range f.calls {
		if c[0] == "run" {
			return c
		}
	}
	t.Fatal("docker run was not called")
	return nil
}

func testOptions() PostgresOptions {
	o := DefaultPostgresOptions()
	o.Image = "registry.example.com/mirror/postgres:16-alpine"
	return o
}

const fast = time.Millisecond

func TestDefaultsMatchDockerCompose(t *testing.T) {
	body, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(body)
	def := func(variable string) string {
		m := regexp.MustCompile(`\$\{` + variable + `:-([^}]+)\}`).FindStringSubmatch(compose)
		if m == nil {
			t.Fatalf("docker-compose.yml has no default for %s", variable)
		}
		return m[1]
	}
	o := DefaultPostgresOptions()
	if o.Image != def("AIDEV_DB_IMAGE") {
		t.Errorf("Image = %q, compose default %q", o.Image, def("AIDEV_DB_IMAGE"))
	}
	if o.Container != def("AIDEV_DB_CONTAINER") || o.User != def("AIDEV_DB_USER") ||
		o.Password != def("AIDEV_DB_PASSWORD") || o.Database != def("AIDEV_DB_NAME") {
		t.Errorf("defaults %+v differ from docker-compose.yml", o)
	}
	if port, _ := strconv.Atoi(def("AIDEV_DB_PORT")); o.Port != port {
		t.Errorf("Port = %d, compose default %d", o.Port, port)
	}
	if !strings.Contains(compose, `"127.0.0.1:${AIDEV_DB_PORT:-5434}:5432"`) {
		t.Error("docker-compose.yml must publish PostgreSQL on 127.0.0.1 only, as setup does")
	}
	if o.Volume != "aidev-pgdata" || !strings.Contains(compose, "aidev-pgdata:/var/lib/postgresql/data") {
		t.Errorf("Volume = %q, want aidev-pgdata as in docker-compose.yml", o.Volume)
	}
}

func TestDatabaseURLIsOneAidevAccepts(t *testing.T) {
	o := DefaultPostgresOptions()
	o.Password = "p@ss/word"
	o.Port = 6543
	url := o.DatabaseURL()
	if !strings.HasPrefix(url, "postgres://") || !strings.Contains(url, "127.0.0.1:6543") || !strings.Contains(url, "sslmode=disable") {
		t.Errorf("DatabaseURL() = %q, want postgres://...@127.0.0.1:6543/...?sslmode=disable", url)
	}
	path := t.TempDir() + "/conf.json"
	if err := os.WriteFile(path, fmt.Appendf(nil, `{"database": {"url": %q}}`, url), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("aidev rejects %q: %v", url, err)
	}
	if cfg.DatabaseURL != url {
		t.Errorf("loaded %q, want %q", cfg.DatabaseURL, url)
	}
	// A password with reserved characters must be escaped, not break the URL.
	if strings.Contains(url, "p@ss/word@") {
		t.Errorf("the password is not escaped: %q", url)
	}
}

func TestMissingContainerIsCreatedWithTheChosenImageAndComposeSettings(t *testing.T) {
	d := &fakeDocker{healths: []string{"starting", "starting", "healthy"}}
	o := testOptions()

	action, err := EnsurePostgres(context.Background(), d, o, fast, time.Second)
	if err != nil {
		t.Fatalf("EnsurePostgres: %v", err)
	}
	if action != PostgresCreated {
		t.Errorf("action = %q, want %q", action, PostgresCreated)
	}
	if got := strings.Join(d.commands(), ","); got != "run" {
		t.Errorf("docker commands = %s, want only run", got)
	}

	args := d.runArgs(t)
	joined := strings.Join(args, " ")
	if args[len(args)-1] != o.Image {
		t.Errorf("the image must be the last argument of docker run, got %v", args)
	}
	for _, want := range []string{
		"-d",
		"--name " + o.Container,
		"--restart unless-stopped",
		"-e POSTGRES_USER=" + o.User,
		"-e POSTGRES_PASSWORD=" + o.Password,
		"-e POSTGRES_DB=" + o.Database,
		"-e POSTGRES_INITDB_ARGS=--encoding=UTF8 --locale=C",
		// Loopback only: the default password must not be reachable from the network.
		fmt.Sprintf("-p 127.0.0.1:%d:5432", o.Port),
		"-v aidev-pgdata:/var/lib/postgresql/data",
		"--health-cmd pg_isready -U " + o.User + " -d " + o.Database,
		"--health-interval 2s",
		"--health-timeout 3s",
		"--health-retries 30",
		"--health-start-period 5s",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run is missing %q:\n%v", want, args)
		}
	}
	// Each flag value is its own argument: nothing goes through a shell.
	for i, a := range args {
		if a == "--health-cmd" && (i+1 >= len(args) || args[i+1] != "pg_isready -U "+o.User+" -d "+o.Database) {
			t.Errorf("--health-cmd must be followed by the whole command as one argument: %v", args)
		}
	}
}

func TestStoppedContainerIsStartedNotRecreated(t *testing.T) {
	for _, status := range []string{"exited", "created"} {
		t.Run(status, func(t *testing.T) {
			d := &fakeDocker{exists: true, status: status, healths: []string{"healthy"}}
			action, err := EnsurePostgres(context.Background(), d, testOptions(), fast, time.Second)
			if err != nil {
				t.Fatalf("EnsurePostgres: %v", err)
			}
			if action != PostgresStarted {
				t.Errorf("action = %q, want %q", action, PostgresStarted)
			}
			if got := strings.Join(d.commands(), ","); got != "start" {
				t.Errorf("docker commands = %s, want only start", got)
			}
		})
	}
}

func TestRunningHealthyContainerIsLeftAlone(t *testing.T) {
	d := &fakeDocker{exists: true, status: "running", healths: []string{"healthy"}}
	action, err := EnsurePostgres(context.Background(), d, testOptions(), fast, time.Second)
	if err != nil {
		t.Fatalf("EnsurePostgres: %v", err)
	}
	if action != PostgresAlreadyRunning {
		t.Errorf("action = %q, want %q", action, PostgresAlreadyRunning)
	}
	if cmds := d.commands(); len(cmds) != 0 {
		t.Errorf("a healthy container was touched: %v", cmds)
	}
}

func TestUnhealthyOrSlowContainerFailsWithWhereToLook(t *testing.T) {
	o := testOptions()

	d := &fakeDocker{exists: true, status: "running", healths: []string{"unhealthy"}}
	_, err := EnsurePostgres(context.Background(), d, o, fast, time.Second)
	if err == nil || !strings.Contains(err.Error(), "docker logs "+o.Container) {
		t.Errorf("unhealthy: err = %v, want one pointing at `docker logs %s`", err, o.Container)
	}

	d = &fakeDocker{exists: true, status: "running", healths: []string{"starting"}}
	start := time.Now()
	_, err = EnsurePostgres(context.Background(), d, o, fast, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "docker logs "+o.Container) {
		t.Errorf("timeout: err = %v, want one pointing at `docker logs %s`", err, o.Container)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("EnsurePostgres waited %s with a 50ms timeout", elapsed)
	}
}

func TestOtherContainerStatesAreNotGuessedAt(t *testing.T) {
	for _, status := range []string{"paused", "restarting", "dead", "removing"} {
		t.Run(status, func(t *testing.T) {
			d := &fakeDocker{exists: true, status: status, healths: []string{"healthy"}}
			_, err := EnsurePostgres(context.Background(), d, testOptions(), fast, time.Second)
			if err == nil || !strings.Contains(err.Error(), status) {
				t.Errorf("err = %v, want an error naming the %s state", err, status)
			}
			if cmds := d.commands(); len(cmds) != 0 {
				t.Errorf("a %s container was acted on: %v", status, cmds)
			}
		})
	}
}

func TestDockerNotRunningSaysToStartDocker(t *testing.T) {
	d := &fakeDocker{daemon: errors.New("exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")}
	_, err := EnsurePostgres(context.Background(), d, testOptions(), fast, time.Second)
	if err == nil || !strings.Contains(err.Error(), "Docker daemon") {
		t.Fatalf("err = %v, want the daemon error passed on", err)
	}
	if cmds := d.commands(); len(cmds) != 0 {
		t.Errorf("docker %v was attempted although inspect could not reach the daemon", cmds)
	}
}

func TestExecDockerReportsStderr(t *testing.T) {
	d := ExecDocker()
	if d == nil {
		t.Fatal("ExecDocker returned nil")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := d.Run(context.Background(), "version"); err == nil {
		t.Error("Run succeeded with no docker on PATH")
	}
}
