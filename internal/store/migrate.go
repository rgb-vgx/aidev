package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// migrationLockID namespaces aidev's advisory lock. Any value works as long as
// it is stable; it only has to be distinct from other users of the database.
const migrationLockID int64 = 0x61696465_76 // "aidev"

// Migration is one SQL file to be applied exactly once.
type Migration struct {
	Version  string // filename without .sql, e.g. "0001_init"
	SQL      string
	Checksum string // sha256 of SQL, hex encoded
}

// LoadMigrations reads and sorts migrations from fsys.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("no migrations found")
	}
	sort.Strings(entries)

	migrations := make([]Migration, 0, len(entries))
	for _, name := range entries {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  strings.TrimSuffix(path.Base(name), ".sql"),
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	return migrations, nil
}

// MigrateResult reports what a migration run did.
type MigrateResult struct {
	Applied   []string // versions applied by this run
	AlreadyUp []string // versions that were already present
}

// Migrate brings the database up to date.
//
// Each migration runs in its own transaction together with the bookkeeping row
// that records it, so a failure leaves the database at a known version rather
// than half-migrated. A session-level advisory lock serialises concurrent
// runners: two aidev processes starting at once is ordinary, and both trying to
// create the same table is not a useful failure.
//
// An already-applied migration whose file has since been edited is reported as
// an error instead of being ignored, because silently diverging schemas are
// much harder to diagnose later than a refusal to start.
func (s *Store) Migrate(ctx context.Context, migrations []Migration) (MigrateResult, error) {
	var result MigrateResult

	if s.pool == nil {
		return result, errors.New("Migrate requires a pool-backed Store")
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return result, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return result, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT        PRIMARY KEY,
			checksum   TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return result, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return result, fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return result, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("read schema_migrations: %w", err)
	}

	for _, m := range migrations {
		if have, ok := applied[m.Version]; ok {
			if have != m.Checksum {
				return result, fmt.Errorf(
					"migration %s was already applied but its file has changed "+
						"(recorded %s, found %s); add a new migration instead of editing an applied one",
					m.Version, short(have), short(m.Checksum))
			}
			result.AlreadyUp = append(result.AlreadyUp, m.Version)
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return result, fmt.Errorf("begin migration %s: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			_ = tx.Rollback(ctx)
			return result, fmt.Errorf("apply migration %s: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
			m.Version, m.Checksum); err != nil {
			_ = tx.Rollback(ctx)
			return result, fmt.Errorf("record migration %s: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return result, fmt.Errorf("commit migration %s: %w", m.Version, err)
		}
		result.Applied = append(result.Applied, m.Version)
	}

	return result, nil
}

func short(checksum string) string {
	if len(checksum) <= 12 {
		return checksum
	}
	return checksum[:12]
}
