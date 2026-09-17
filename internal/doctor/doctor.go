// Package doctor checks whether aidev can work on this machine and says, in
// plain words, what to do about each problem it finds.
package doctor

import (
	"context"

	"aidev/internal/config"
)

// Status is the outcome of one check.
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusFail    Status = "fail"
	StatusSkipped Status = "skipped"
)

// Check names, in the order Run reports them.
const (
	CheckConfig     = "config"
	CheckGit        = "git"
	CheckAgent      = "agent"
	CheckDatabase   = "database"
	CheckMigrations = "migrations"
	CheckWorkspace  = "workspace"
)

// Result is one check's outcome. Summary says what was found; Fix, set whenever
// Status is warn or fail, says what to do about it.
type Result struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Summary string `json:"summary"`
	Fix     string `json:"fix,omitempty"`
}

// Deps is everything Run needs from the outside world, so that each check can be
// exercised without a real machine.
type Deps struct {
	// LoadConfig resolves aidev's configuration (config.Load in production).
	LoadConfig func() (config.Config, error)
	// LookPath finds an executable (exec.LookPath in production).
	LookPath func(name string) (string, error)
	// PingDatabase connects to the database and returns nil if it answers.
	PingDatabase func(ctx context.Context, databaseURL string) error
	// PendingMigrations returns the versions of migrations not yet applied.
	PendingMigrations func(ctx context.Context, databaseURL string) ([]string, error)
	// CheckWorkspace returns nil if dir exists or can be created, and is writable.
	CheckWorkspace func(dir string) error
}

// Run performs every check in order and returns one Result per check.
func Run(ctx context.Context, deps Deps) []Result { return nil }

// Failed reports whether any result has StatusFail.
func Failed(results []Result) bool { return false }
