// Package config loads and validates aidev's runtime configuration.
//
// Configuration comes from the environment. Nothing else in aidev reads
// environment variables directly: everything is funnelled through Load so that
// an invalid configuration fails at startup with one clear message instead of
// surfacing halfway through a task execution.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults. The task timeout is deliberately generous: Phase 0 observed that
// OpenCode's first run against a repository it has not seen can exceed four
// minutes before producing any output (docs/research.md §2.9). A short default
// would make every fresh developer's first task fail for a reason that looks
// like a bug in aidev.
const (
	DefaultTaskTimeout         = 30 * time.Minute
	DefaultVerificationTimeout = 10 * time.Minute
	DefaultOpenCodeCommand     = "opencode"
	DefaultOpenCodeAgent       = "build"
	DefaultMaxOutputBytes      = 1 << 20 // 1 MiB per captured stream
	DefaultCleanupPolicy       = CleanupOnSuccess
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

	// OpenCodeCommand is the executable used by the OpenCode backend.
	OpenCodeCommand string

	// OpenCodeModel is passed through as -m. Empty means "let OpenCode
	// choose", which Phase 0 confirmed works (docs/research.md §2.10).
	OpenCodeModel string

	// OpenCodeAgent is the default OpenCode agent for tasks that do not name
	// one. Phase 0 found that OpenCode silently falls back to its default on
	// an unknown agent name and still exits 0, so aidev validates this itself.
	OpenCodeAgent string

	// MaxOutputBytes bounds each captured stdout/stderr stream. Output beyond
	// it is discarded and flagged as truncated, so a runaway agent cannot
	// exhaust memory or the database.
	MaxOutputBytes int

	// WorktreeCleanup decides what happens to a worktree once its task
	// finishes. No policy discards failed work.
	WorktreeCleanup CleanupPolicy

	// LogLevel is the minimum level emitted by the structured logger.
	LogLevel slog.Level
}

// Lookup abstracts environment access so tests need no global state.
type Lookup func(key string) (string, bool)

// OSLookup reads the real process environment.
func OSLookup(key string) (string, bool) { return os.LookupEnv(key) }

// Load resolves configuration from lookup, applying defaults and validating
// the result. All problems are reported together rather than one per run.
func Load(lookup Lookup) (Config, error) {
	if lookup == nil {
		lookup = OSLookup
	}

	cfg := Config{
		DefaultTaskTimeout:         DefaultTaskTimeout,
		DefaultVerificationTimeout: DefaultVerificationTimeout,
		OpenCodeCommand:            DefaultOpenCodeCommand,
		OpenCodeAgent:              DefaultOpenCodeAgent,
		MaxOutputBytes:             DefaultMaxOutputBytes,
		WorktreeCleanup:            DefaultCleanupPolicy,
		LogLevel:                   slog.LevelInfo,
	}

	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	cfg.DatabaseURL = strings.TrimSpace(get(lookup, "DATABASE_URL"))
	if cfg.DatabaseURL == "" {
		fail("DATABASE_URL is required (example: postgres://aidev:aidev@127.0.0.1:5434/aidev?sslmode=disable)")
	}

	root := strings.TrimSpace(get(lookup, "WORKSPACE_ROOT"))
	if root == "" {
		def, err := defaultWorkspaceRoot()
		if err != nil {
			fail("WORKSPACE_ROOT is required: %v", err)
		}
		root = def
	}
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			fail("WORKSPACE_ROOT %q is not a usable path: %v", root, err)
		} else {
			cfg.WorkspaceRoot = filepath.Clean(abs)
		}
	}

	if d, ok, err := duration(lookup, "DEFAULT_TASK_TIMEOUT"); err != nil {
		fail("%v", err)
	} else if ok {
		cfg.DefaultTaskTimeout = d
	}

	if d, ok, err := duration(lookup, "DEFAULT_VERIFICATION_TIMEOUT"); err != nil {
		fail("%v", err)
	} else if ok {
		cfg.DefaultVerificationTimeout = d
	}

	if v := strings.TrimSpace(get(lookup, "OPENCODE_COMMAND")); v != "" {
		cfg.OpenCodeCommand = v
	}
	cfg.OpenCodeModel = strings.TrimSpace(get(lookup, "OPENCODE_MODEL"))
	if v := strings.TrimSpace(get(lookup, "OPENCODE_AGENT")); v != "" {
		cfg.OpenCodeAgent = v
	}

	if v := strings.TrimSpace(get(lookup, "MAX_OUTPUT_BYTES")); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			fail("MAX_OUTPUT_BYTES %q is not an integer", v)
		case n < 1024:
			fail("MAX_OUTPUT_BYTES must be at least 1024, got %d", n)
		default:
			cfg.MaxOutputBytes = n
		}
	}

	if v := strings.TrimSpace(get(lookup, "WORKTREE_CLEANUP")); v != "" {
		policy := CleanupPolicy(strings.ToLower(v))
		if !policy.Valid() {
			names := make([]string, 0, len(AllCleanupPolicies()))
			for _, p := range AllCleanupPolicies() {
				names = append(names, string(p))
			}
			fail("WORKTREE_CLEANUP %q is not one of %s", v, strings.Join(names, ", "))
		} else {
			cfg.WorktreeCleanup = policy
		}
	}

	if v := strings.TrimSpace(get(lookup, "LOG_LEVEL")); v != "" {
		lvl, err := ParseLevel(v)
		if err != nil {
			fail("%v", err)
		} else {
			cfg.LogLevel = lvl
		}
	}

	if cfg.DefaultTaskTimeout <= 0 {
		fail("DEFAULT_TASK_TIMEOUT must be positive")
	}
	if cfg.DefaultVerificationTimeout <= 0 {
		fail("DEFAULT_VERIFICATION_TIMEOUT must be positive")
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// ParseLevel maps a LOG_LEVEL string onto a slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL %q is not one of debug, info, warn, error", s)
	}
}

// Redacted returns the configuration with the database password removed, for
// logging. aidev never logs a connection string verbatim.
func (c Config) Redacted() Config {
	c.DatabaseURL = RedactURL(c.DatabaseURL)
	return c
}

// RedactURL removes userinfo credentials from a URL-shaped string. It works on
// a plain string rather than net/url so that an unparseable value is redacted
// conservatively instead of being passed through.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	scheme := ""
	rest := raw
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme, rest = raw[:i+3], raw[i+3:]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return raw
	}
	userinfo := rest[:at]
	user := userinfo
	if c := strings.Index(userinfo, ":"); c >= 0 {
		user = userinfo[:c]
	}
	if user == "" {
		return scheme + "***@" + rest[at+1:]
	}
	return scheme + user + ":***@" + rest[at+1:]
}

func get(lookup Lookup, key string) string {
	v, _ := lookup(key)
	return v
}

func duration(lookup Lookup, key string) (time.Duration, bool, error) {
	raw := strings.TrimSpace(get(lookup, key))
	if raw == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("%s %q is not a duration (examples: 90s, 30m, 2h)", key, raw)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("%s must be positive, got %s", key, raw)
	}
	return d, true, nil
}

// defaultWorkspaceRoot derives a per-user location instead of hard-coding a
// machine-specific path, honouring XDG_DATA_HOME when set.
func defaultWorkspaceRoot() (string, error) {
	if x := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); x != "" {
		return filepath.Join(x, "aidev", "worktrees"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no WORKSPACE_ROOT set and the user's home directory could not be determined")
	}
	return filepath.Join(home, ".local", "share", "aidev", "worktrees"), nil
}
