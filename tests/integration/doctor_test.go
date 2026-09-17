package integration

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"aidev/internal/store"
	"aidev/migrations"
)

// aidev doctor asks the database which migrations are still to apply, without
// applying them: a health check must not change what it inspects.

func TestPendingMigrationsOnAnUpToDateDatabaseIsEmpty(t *testing.T) {
	db, ctx := openStore(t)
	loaded, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingMigrations(ctx, loaded)
	if err != nil {
		t.Fatalf("PendingMigrations: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v on a migrated database, want none", pending)
	}
}

func TestPendingMigrationsListsUnappliedVersionsInOrderAndAppliesNothing(t *testing.T) {
	db, ctx := openStore(t)
	loaded, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	future := []store.Migration{
		{Version: "9998_doctor_probe_a", Checksum: strings.Repeat("a", 64), SQL: "CREATE TABLE doctor_probe_a (id int)"},
		{Version: "9999_doctor_probe_b", Checksum: strings.Repeat("b", 64), SQL: "CREATE TABLE doctor_probe_b (id int)"},
	}
	pending, err := db.PendingMigrations(ctx, append(append([]store.Migration(nil), loaded...), future...))
	if err != nil {
		t.Fatalf("PendingMigrations: %v", err)
	}
	if want := []string{"9998_doctor_probe_a", "9999_doctor_probe_b"}; !reflect.DeepEqual(pending, want) {
		t.Errorf("pending = %v, want %v", pending, want)
	}

	var n int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version LIKE '999%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("PendingMigrations recorded %d migration(s); it must not apply anything", n)
	}
	var table *string
	if err := db.Pool().QueryRow(ctx, `SELECT to_regclass('doctor_probe_a')::text`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != nil {
		t.Error("PendingMigrations ran a migration's SQL; it must not apply anything")
	}
}

// An applied migration whose file has changed is the same error Migrate reports,
// so doctor surfaces it before the next run trips over it.
func TestPendingMigrationsReportsAnEditedAppliedMigration(t *testing.T) {
	db, ctx := openStore(t)
	loaded, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	edited := append([]store.Migration(nil), loaded...)
	edited[0].Checksum = strings.Repeat("0", 64)

	_, err = db.PendingMigrations(ctx, edited)
	if err == nil || !strings.Contains(err.Error(), "file has changed") {
		t.Fatalf("PendingMigrations with an edited applied migration: err = %v, want one saying its file has changed", err)
	}
}

// A database nobody has migrated has no schema_migrations table yet: everything is
// pending, and asking must not create the table.
func TestPendingMigrationsOnAnEmptyDatabaseListsEverything(t *testing.T) {
	db, ctx := openStore(t)
	schema := fmt.Sprintf("doctor_empty_%d", time.Now().UnixNano())
	if _, err := db.Pool().Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool().Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })

	// pgx passes unknown URL parameters to the server as run-time settings, so
	// this connection sees only the empty schema.
	url := os.Getenv(envDatabaseURL)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	empty, err := store.Open(ctx, url+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(empty.Close)

	loaded, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := empty.PendingMigrations(ctx, loaded)
	if err != nil {
		t.Fatalf("PendingMigrations on an empty database: %v", err)
	}
	if len(pending) != len(loaded) || pending[0] != loaded[0].Version {
		t.Errorf("pending = %v, want all %d migrations in order", pending, len(loaded))
	}

	var table *string
	if err := db.Pool().QueryRow(ctx, `SELECT to_regclass($1)::text`, schema+".schema_migrations").Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != nil {
		t.Error("PendingMigrations created schema_migrations; it must not change the database")
	}
}
