// Package integration holds tests that talk to a real PostgreSQL instance.
//
// They are skipped unless TEST_DATABASE_URL is set, so that `go test ./...` on a
// machine without a database still passes rather than failing for an
// environmental reason. docker-compose.yml provides the database; see the README.
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"aidev/internal/store"
	"aidev/migrations"
)

const envDatabaseURL = "TEST_DATABASE_URL"

// envRequireDB turns the skip into a failure. A skipped package still reports
// ok, so a gate that meant to run these tests — `make test-integration`, or an
// aidev task's verification — would otherwise pass without running any of them.
const envRequireDB = "AIDEV_REQUIRE_DB"

// openStore returns a migrated, empty database, or skips the test when none is
// configured. Each test gets truncated tables rather than a fresh database, so
// the suite stays fast while still being order-independent.
func openStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()

	url := os.Getenv(envDatabaseURL)
	if url == "" {
		if os.Getenv(envRequireDB) == "1" {
			t.Fatalf("%s=1 but %s is not set: the integration tests were asked for and cannot run", envRequireDB, envDatabaseURL)
		}
		t.Skipf("%s is not set; start the database with `docker compose up -d` and export it to run integration tests", envDatabaseURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(db.Close)

	loaded, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := db.Migrate(ctx, loaded); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	truncate(t, ctx, db)
	return db, ctx
}

// truncate empties every table. tasks cascades to attempts, runs and events, but
// naming them all keeps the helper honest if a cascade is ever removed.
func truncate(t *testing.T, ctx context.Context, db *store.Store) {
	t.Helper()
	_, err := db.Pool().Exec(ctx, `
		TRUNCATE events, verification_runs, worker_runs, worktrees,
		         approvals, task_attempts, tasks, projects
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
}
