// Package config loads and validates aidev's runtime configuration.
//
// Every setting lives in one JSON file named by the AIDEV_CONFIG environment
// variable. No other environment variable is read for configuration, and there
// is no config.env: AIDEV_CONFIG must hold the absolute path of a conf.json
// file. Everything is validated at startup and every problem is reported at
// once, so an invalid configuration fails fast instead of surfacing halfway
// through a task execution.
package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"aidev/internal/tracing"
)

// Defaults. The task timeout is deliberately generous: Phase 0 observed that
// OpenCode's first run against a repository it has not seen can exceed four
// minutes before producing any output (docs/research.md §2.9). A short default
// would make every fresh developer's first task fail for a reason that looks
// like a bug in aidev.
const (
	DefaultTaskTimeout         = 30 * time.Minute
	DefaultVerificationTimeout = 10 * time.Minute

	// DefaultVerificationTotalTimeout bounds the whole verification pass. The
	// per-step timeout alone lets N steps multiply into N × 10 minutes, so a
	// task whose checks hang one after another would hold a worker for hours.
	DefaultVerificationTotalTimeout = 30 * time.Minute

	DefaultOpenCodeCommand = "opencode"
	DefaultOpenCodeAgent   = "build"

	// DefaultOpenCodeModel is chosen on measured behaviour, not on branding.
	// On identical trivial prompts it returned in 3.2-3.9s across repeated runs,
	// while the alternative free model varied between 3.9s and 100.5s for the
	// same work (docs/research.md 7c). Predictable latency matters more here than
	// a marginally better model, because an unpredictable one turns a one-minute
	// task into an eleven-minute one.
	//
	// agent.opencode.model set to an empty string lets OpenCode choose instead.
	DefaultOpenCodeModel  = "opencode/muse-spark-1.3-contributor-free"
	DefaultMaxOutputBytes = 1 << 20 // 1 MiB per captured stream
	DefaultCleanupPolicy  = CleanupOnSuccess
)

// CleanupPolicy decides what happens to a task's worktree once it finishes.
//
// There is no policy that discards a failed attempt's work. Git itself refuses to
// remove a worktree holding uncommitted changes, and aidev never passes --force
// automatically (docs/research.md §7b), so a failed attempt is always inspectable.
type CleanupPolicy string

const (
	// CleanupOnSuccess commits the work to the task's branch and then removes
	// the worktree directory. The result stays reviewable with ordinary git
	// commands while the workspace does not grow without bound. This is the
	// default.
	CleanupOnSuccess CleanupPolicy = "on-success"

	// CleanupNever keeps every worktree on disk. Useful when debugging aidev
	// itself, at the cost of an ever-growing workspace.
	CleanupNever CleanupPolicy = "never"
)

// Valid reports whether p is a known policy.
func (p CleanupPolicy) Valid() bool {
	return p == CleanupOnSuccess || p == CleanupNever
}

func (p CleanupPolicy) String() string { return string(p) }

// AllCleanupPolicies lists every policy, for error messages and documentation.
func AllCleanupPolicies() []CleanupPolicy {
	return []CleanupPolicy{CleanupOnSuccess, CleanupNever}
}

// Config is the fully resolved, validated configuration.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string. Required.
	DatabaseURL string

	// WorkspaceRoot is the directory under which all task worktrees are
	// created. Every worktree path must resolve inside it; see internal/git.
	WorkspaceRoot string

	// DefaultTaskTimeout bounds a single agent run when the task does not
	// specify its own timeout.
	DefaultTaskTimeout time.Duration

	// DefaultVerificationTimeout bounds a single verification step.
	DefaultVerificationTimeout time.Duration

	// VerificationTotalTimeout bounds one whole verification pass: every step
	// together, not each one on its own.
	VerificationTotalTimeout time.Duration

	// OpenCodeCommand is the executable used by the OpenCode backend.
	OpenCodeCommand string

	// OpenCodeModel is passed through as -m. Empty means "let OpenCode choose",
	// which works with no credentials (docs/research.md §2.10), and is what an
	// explicitly empty agent.opencode.model selects.
	OpenCodeModel string

	// OpenCodeAgent is the default OpenCode agent for tasks that do not name
	// one. Phase 0 found that OpenCode silently falls back to its default on
	// an unknown agent name and still exits 0, so aidev validates this itself.
	OpenCodeAgent string

	// AgentBackend selects which agent implementation runs tasks.
	AgentBackend Backend

	// CodexCommand is the executable used by the Codex backend.
	CodexCommand string

	// CodexProfile is passed as --profile when set. On this installation a
	// profile is required for codex to reach a model at all.
	CodexProfile string

	// CodexModel is passed as -m when set. Empty lets Codex choose.
	CodexModel string

	// CodexSandbox is passed as --sandbox when set.
	CodexSandbox string

	// Routing maps a task hardness (TRIVIAL, STANDARD, HARD) to the model that
	// hardness deserves, so a person states how hard a task is once and
	// configuration decides which model that deserves. A hardness with no entry
	// leaves the choice to the backend, as does a task with no hardness.
	Routing map[string]string

	// MaxOutputBytes bounds each captured stdout/stderr stream. Output beyond
	// it is discarded and flagged as truncated, so a runaway agent cannot
	// exhaust memory or the database.
	MaxOutputBytes int

	// WorktreeCleanup decides what happens to a worktree once its task
	// finishes. No policy discards failed work.
	WorktreeCleanup CleanupPolicy

	// LogLevel is the minimum level emitted by the structured logger.
	LogLevel slog.Level

	// Tracing holds the tracing section of the configuration file.
	Tracing tracing.Settings

	// MCPAllowApproval decides whether the MCP approve tool may record a
	// decision at all. Default false: anything that can talk to the MCP
	// server can call it — in the common setup that is an LLM, and a planner
	// can create the task it would be approving. Approval is a human act, and
	// the human path is the CLI (`aidev task approve`). Setting this true
	// hands that power to the MCP client deliberately.
	MCPAllowApproval bool

	// MCPAutoRegisterProjects decides whether aidev_create_task may register
	// a repository aidev has never seen (research D3). Default false: the
	// path comes from the MCP client, usually a planner, and a mistyped or
	// guessed path would otherwise run an agent in the wrong repository.
	// Repositories are added by a person with `aidev project add`, or on
	// their first `aidev task create`; setting this true restores
	// registration on demand for MCP too.
	MCPAutoRegisterProjects bool

	// ConfigFile is the path of the configuration file that was read, so
	// `aidev config` can report which file is in effect.
	ConfigFile string
}

// Lookup abstracts environment access so tests need no global state. Only
// AIDEV_CONFIG is read from it; everything else lives in the file it names.
type Lookup func(key string) (string, bool)

// OSLookup reads the real process environment.
func OSLookup(key string) (string, bool) { return os.LookupEnv(key) }

// EnvUser is the login name of the account running the process, used as the
// default decider in `aidev task approve`. It is read here, beside every other
// piece of environment access, because internal/config is the only package
// that reads the environment (AGENTS.md); the caller keeps the fallback for
// when the variable is unset.
func EnvUser() string { return os.Getenv("USER") }

// EnvConfigPath is the raw AIDEV_CONFIG value, empty when unset. `aidev setup`
// compares it against the file it just wrote to decide whether the shell
// already points there or still needs the export line.
func EnvConfigPath() string { return os.Getenv("AIDEV_CONFIG") }

// defaultWorkspaceRoot derives a per-user location instead of hard-coding a
// machine-specific path. Only the home directory is consulted: no other
// environment variable participates in configuration.
func defaultWorkspaceRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no workspace_root set and the user's home directory could not be determined")
	}
	return filepath.Join(home, ".local", "share", "aidev", "worktrees"), nil
}
