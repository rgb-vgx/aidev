// Package store is aidev's PostgreSQL persistence layer.
//
// PostgreSQL is the authoritative state: nothing important lives only in
// memory. State changes that must not be observed half-applied are wrapped in a
// transaction via Store.InTx, and status changes use compare-and-set so that two
// concurrent callers cannot both believe they won.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors callers are expected to distinguish.
var (
	// ErrNotFound means no row matched.
	ErrNotFound = errors.New("not found")

	// ErrConflict means a compare-and-set lost: the row changed underneath the
	// caller. It is a normal outcome under concurrency, not a bug.
	ErrConflict = errors.New("conflict: row changed concurrently")

	// ErrAlreadyExists means a uniqueness constraint rejected the write.
	ErrAlreadyExists = errors.New("already exists")
)

// querier is the subset of pgx shared by a pool and a transaction. Repository
// methods are written against it so that each one works identically inside and
// outside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store provides access to aidev's tables. A Store obtained inside InTx runs
// every method on that transaction.
type Store struct {
	db querier

	// pool is nil for a transaction-scoped Store, which is what prevents a
	// nested InTx from silently opening a second, independent transaction.
	pool *pgxpool.Pool
}

// Open connects to PostgreSQL and verifies the connection.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database.url: %w", err)
	}

	// Keep the pool modest: aidev is a local-first single-operator tool, and an
	// oversized pool only moves contention into the database.
	cfg.MaxConns = 8
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database is not reachable: %w", err)
	}

	return &Store{db: pool, pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Pool exposes the underlying pool for operations that need it, such as
// migrations. It is nil for a transaction-scoped Store.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// InTx runs fn inside a single transaction, committing when fn returns nil and
// rolling back otherwise. The Store handed to fn is transaction-scoped.
//
// Calling InTx on an already transaction-scoped Store is an error rather than a
// silent no-op, because the caller would otherwise believe it had rollback
// isolation that it does not have.
func (s *Store) InTx(ctx context.Context, fn func(*Store) error) error {
	if s.pool == nil {
		return errors.New("InTx called on a transaction-scoped Store: nested transactions are not supported")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// Roll back on panic as well as on error, so a bug cannot leave a
	// transaction open and holding locks.
	committed := false
	defer func() {
		if !committed {
			// The rollback uses its own context: if ctx is already cancelled,
			// a rollback bound to it would be refused and the connection would
			// be returned to the pool in a broken state.
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(&Store{db: tx}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}

// classify turns a pgx error into one of the package's sentinels where the
// distinction matters to callers.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrAlreadyExists, pgErr.ConstraintName)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrNotFound, pgErr.ConstraintName)
		case "23514": // check_violation
			// A check violation means aidev tried to persist something its own
			// domain validation should have rejected. Surface the constraint
			// name: it identifies which invariant the code failed to uphold.
			return fmt.Errorf("database rejected the value (constraint %s): %w", pgErr.ConstraintName, err)
		}
	}
	return err
}
