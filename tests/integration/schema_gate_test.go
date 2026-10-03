package integration

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// A command must refuse a schema this binary has not seen, and say which
// command repairs it — otherwise a stale database fails halfway through a run
// as a missing column, inside the worker, after the agent has already done the
// work. The gate must not swallow the repair itself: `aidev migrate` has to
// stay reachable on the very database everything else refuses.
func TestPendingSchemaRefusesCommandsButNotMigrate(t *testing.T) {
	db, ctx := openStore(t)

	schema := fmt.Sprintf("gate_%d", time.Now().UnixNano())
	if _, err := db.Pool().Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool().Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })

	// pgx passes unknown URL parameters to the server as run-time settings, so
	// this connection sees only the empty schema — a database nobody migrated.
	url := os.Getenv(envDatabaseURL)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	staleURL := url + sep + "search_path=" + schema
	workspace := t.TempDir()

	_, _, err := runCLIWithDatabase(t, staleURL, workspace, "stats")
	if err == nil {
		t.Fatal("stats against a database with pending migrations succeeded; it must refuse")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("error = %v, must say the migrations are pending", err)
	}
	if !strings.Contains(err.Error(), "aidev migrate") {
		t.Errorf("error = %v, must name `aidev migrate` as the fix", err)
	}

	// The command that applies the migrations is not gated by them.
	if _, stderr, err := runCLIWithDatabase(t, staleURL, workspace, "migrate"); err != nil {
		t.Fatalf("migrate on the refused database: %v (stderr: %s)", err, stderr)
	}

	// With the schema current, the same command runs.
	stdout, _, err := runCLIWithDatabase(t, staleURL, workspace, "stats")
	if err != nil {
		t.Fatalf("stats after migrate: %v", err)
	}
	if !strings.Contains(stdout, "no finished attempts recorded yet") {
		t.Errorf("stats = %q, want the empty report", stdout)
	}
}
