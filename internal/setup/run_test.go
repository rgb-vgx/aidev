package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aidev/internal/config"
)

// setup.Run is what `aidev setup` does: start the database (unless the user has
// their own), write conf.json (unless there is one), and migrate. Running it again
// must be safe, because "run setup again" is the natural answer to most problems
// a new user has, for example after a reboot.

type fakeSteps struct {
	calls      []string
	postgres   []PostgresOptions
	migrated   []string
	ensureErr  error
	migrateErr error
	configPath string
	// configExistedAtEnsure records whether conf.json existed when the
	// container was started: a failed start must not leave a new config behind.
	configExistedAtEnsure bool
}

func (f *fakeSteps) steps() Steps {
	return Steps{
		EnsurePostgres: func(_ context.Context, o PostgresOptions) (PostgresAction, error) {
			f.calls = append(f.calls, "ensure")
			f.postgres = append(f.postgres, o)
			_, err := os.Stat(f.configPath)
			f.configExistedAtEnsure = err == nil
			if f.ensureErr != nil {
				return "", f.ensureErr
			}
			return PostgresCreated, nil
		},
		Migrate: func(_ context.Context, databaseURL string) ([]string, error) {
			f.calls = append(f.calls, "migrate")
			f.migrated = append(f.migrated, databaseURL)
			if f.migrateErr != nil {
				return nil, f.migrateErr
			}
			return []string{"0001_init"}, nil
		},
	}
}

func newRun(t *testing.T) (Options, *fakeSteps) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "aidev", "conf.json")
	o := DefaultPostgresOptions()
	o.Image = "registry.example.com/mirror/postgres:16-alpine"
	return Options{ConfigPath: path, WorkspaceRoot: filepath.Join(dir, "worktrees"), Postgres: o},
		&fakeSteps{configPath: path}
}

func writeExistingConfig(t *testing.T, path, databaseURL string) []byte {
	t.Helper()
	if _, err := WriteConfig(ConfigOptions{Path: path, DatabaseURL: databaseURL}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestFirstRunStartsPostgresThenWritesConfigThenMigrates(t *testing.T) {
	opts, f := newRun(t)

	report, err := Run(context.Background(), opts, f.steps())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"ensure", "migrate"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("steps = %v, want %v", f.calls, want)
	}
	if !reflect.DeepEqual(f.postgres, []PostgresOptions{opts.Postgres}) {
		t.Errorf("EnsurePostgres got %+v, want the chosen options %+v", f.postgres, opts.Postgres)
	}
	if f.configExistedAtEnsure {
		t.Error("conf.json was written before the database started; a failed start would leave it behind")
	}

	cfg, err := config.LoadFile(opts.ConfigPath)
	if err != nil {
		t.Fatalf("aidev rejects the conf.json setup wrote: %v", err)
	}
	wantURL := opts.Postgres.DatabaseURL()
	if cfg.DatabaseURL != wantURL || cfg.WorkspaceRoot != opts.WorkspaceRoot {
		t.Errorf("conf.json has database.url %q, workspace_root %q; want %q, %q",
			cfg.DatabaseURL, cfg.WorkspaceRoot, wantURL, opts.WorkspaceRoot)
	}
	if !reflect.DeepEqual(f.migrated, []string{wantURL}) {
		t.Errorf("migrated %v, want the container's database %q", f.migrated, wantURL)
	}

	want := Report{ConfigPath: opts.ConfigPath, ConfigCreated: true, DatabaseURL: wantURL,
		Postgres: PostgresCreated, Applied: []string{"0001_init"}}
	if !reflect.DeepEqual(report, want) {
		t.Errorf("report = %+v, want %+v", report, want)
	}
}

func TestAFailedStartLeavesNoConfigAndDoesNotMigrate(t *testing.T) {
	opts, f := newRun(t)
	f.ensureErr = errors.New("PostgreSQL in container aidev-postgres is unhealthy: run `docker logs aidev-postgres`")

	_, err := Run(context.Background(), opts, f.steps())
	if err == nil || !strings.Contains(err.Error(), "docker logs aidev-postgres") {
		t.Fatalf("err = %v, want the EnsurePostgres error passed on", err)
	}
	if _, statErr := os.Stat(opts.ConfigPath); !os.IsNotExist(statErr) {
		t.Errorf("conf.json exists after a failed start (stat: %v)", statErr)
	}
	if len(f.migrated) != 0 {
		t.Error("Run migrated after the database failed to start")
	}
}

// Many companies already run PostgreSQL: then setup must not start a container.
func TestAnExistingDatabaseIsUsedWithoutDocker(t *testing.T) {
	opts, f := newRun(t)
	opts.DatabaseURL = "postgres://team:s3cret@db.internal:5432/aidev?sslmode=require"

	report, err := Run(context.Background(), opts, f.steps())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.postgres) != 0 {
		t.Error("Run started a container although the user gave their own database")
	}
	cfg, err := config.LoadFile(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != opts.DatabaseURL {
		t.Errorf("conf.json database.url = %q, want the given one", cfg.DatabaseURL)
	}
	if !reflect.DeepEqual(f.migrated, []string{opts.DatabaseURL}) {
		t.Errorf("migrated %v, want the given database", f.migrated)
	}
	if report.Postgres != "" || !report.ConfigCreated || report.DatabaseURL != opts.DatabaseURL {
		t.Errorf("report = %+v, want no container action, a created config and the given URL", report)
	}
}

// Running setup again after a reboot: the config stays as it is, the container is
// started again, and migrating is a no-op the store handles.
func TestRunningAgainKeepsTheConfigAndRestartsItsContainer(t *testing.T) {
	opts, f := newRun(t)
	before := writeExistingConfig(t, opts.ConfigPath, opts.Postgres.DatabaseURL())

	report, err := Run(context.Background(), opts, f.steps())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	after, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("Run changed an existing conf.json:\nbefore %s\nafter  %s", before, after)
	}
	if report.ConfigCreated {
		t.Error("ConfigCreated = true for a file that already existed")
	}
	if want := []string{"ensure", "migrate"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("steps = %v, want %v: the config names setup's container, so start it", f.calls, want)
	}
}

// An existing config that names another database is the user's: setup migrates
// that database and leaves Docker alone.
func TestAnExistingConfigForAnotherDatabaseIsRespected(t *testing.T) {
	opts, f := newRun(t)
	other := "postgres://team:s3cret@db.internal:5432/aidev?sslmode=require"
	writeExistingConfig(t, opts.ConfigPath, other)

	report, err := Run(context.Background(), opts, f.steps())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.postgres) != 0 {
		t.Error("Run started a container although conf.json names another database")
	}
	if !reflect.DeepEqual(f.migrated, []string{other}) || report.DatabaseURL != other || report.Postgres != "" {
		t.Errorf("migrated %v, report %+v; want the configured database and no container", f.migrated, report)
	}
}

// Asking for one database while conf.json names another would silently use the
// wrong one; say so instead, without printing either password.
func TestAConflictingDatabaseURLIsAnError(t *testing.T) {
	opts, f := newRun(t)
	writeExistingConfig(t, opts.ConfigPath, "postgres://team:s3cret@db.internal:5432/aidev?sslmode=require")
	opts.DatabaseURL = "postgres://other:hunter2@db2.internal:5432/aidev?sslmode=require"

	_, err := Run(context.Background(), opts, f.steps())
	if err == nil {
		t.Fatal("Run accepted a database URL that conf.json contradicts")
	}
	if !strings.Contains(err.Error(), opts.ConfigPath) {
		t.Errorf("error %q does not name the config file", err)
	}
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error shows a password: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("Run ran %v after finding the conflict", f.calls)
	}
}

func TestAnInvalidExistingConfigStopsEverything(t *testing.T) {
	opts, f := newRun(t)
	if err := os.MkdirAll(filepath.Dir(opts.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.ConfigPath, []byte(`{"database": {"url": 5}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), opts, f.steps())
	if err == nil || !strings.Contains(err.Error(), opts.ConfigPath) {
		t.Fatalf("err = %v, want an error naming %s", err, opts.ConfigPath)
	}
	if len(f.calls) != 0 {
		t.Errorf("Run ran %v with an invalid conf.json", f.calls)
	}
}

func TestARelativeConfigPathIsRejectedBeforeAnythingRuns(t *testing.T) {
	opts, f := newRun(t)
	opts.ConfigPath = "conf/conf.json"

	if _, err := Run(context.Background(), opts, f.steps()); err == nil {
		t.Fatal("Run accepted a relative config path")
	}
	if len(f.calls) != 0 {
		t.Errorf("Run ran %v before rejecting the path", f.calls)
	}
}

// A failed migration leaves a valid conf.json: running setup again finishes the job.
func TestAFailedMigrationIsReportedAndKeepsTheConfig(t *testing.T) {
	opts, f := newRun(t)
	f.migrateErr = errors.New("connection refused")

	_, err := Run(context.Background(), opts, f.steps())
	if err == nil || !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), "migrat") {
		t.Fatalf("err = %v, want the migration error, saying it was the migration", err)
	}
	if _, statErr := config.LoadFile(opts.ConfigPath); statErr != nil {
		t.Errorf("conf.json is missing or invalid after a failed migration: %v", statErr)
	}
}
