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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	DefaultOpenCodeCommand     = "opencode"
	DefaultOpenCodeAgent       = "build"

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

// Backend names the agent implementation that runs tasks.
type Backend string

const (
	// BackendOpenCode runs tasks through the OpenCode CLI. This is the default.
	BackendOpenCode Backend = "opencode"
	// BackendCodex runs tasks through the Codex CLI.
	BackendCodex Backend = "codex"
)

// DefaultCodexCommand is the executable name, resolved through PATH.
const DefaultCodexCommand = "codex"

// Valid reports whether b is a known backend.
func (b Backend) Valid() bool {
	return b == BackendOpenCode || b == BackendCodex
}

func (b Backend) String() string { return string(b) }

// AllBackends lists every backend, for error messages and documentation.
func AllBackends() []Backend {
	return []Backend{BackendOpenCode, BackendCodex}
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

	// ConfigFile is the path of the configuration file that was read, so
	// `aidev config` can report which file is in effect.
	ConfigFile string
}

// Lookup abstracts environment access so tests need no global state. Only
// AIDEV_CONFIG is read from it; everything else lives in the file it names.
type Lookup func(key string) (string, bool)

// OSLookup reads the real process environment.
func OSLookup(key string) (string, bool) { return os.LookupEnv(key) }

// configSchema is the vocabulary of conf.json: every key aidev understands.
// SettingKeys and the unknown-key check are both derived from it, so the
// schema is stated once and cannot drift between the two.
var configSchema = map[string]any{
	"database": map[string]any{
		"url": true,
	},
	"workspace_root": true,
	"tasks": map[string]any{
		"timeout":              true,
		"verification_timeout": true,
		"max_output_bytes":     true,
		"worktree_cleanup":     true,
	},
	"agent": map[string]any{
		"backend": true,
		"opencode": map[string]any{
			"command": true,
			"model":   true,
			"agent":   true,
		},
		"codex": map[string]any{
			"command": true,
			"profile": true,
			"model":   true,
			"sandbox": true,
		},
	},
	"log_level": true,
	"tracing": map[string]any{
		"endpoint":        true,
		"traces_endpoint": true,
		"headers":         true,
		"service_name":    true,
		"sample_ratio":    true,
	},
}

// SettingKeys returns the dotted paths of all 20 settings, sorted. It is
// derived from configSchema rather than typed out separately, so adding a
// setting to the schema teaches every consumer at once.
func SettingKeys() []string {
	var keys []string
	var walk func(prefix string, node map[string]any)
	walk = func(prefix string, node map[string]any) {
		for k, v := range node {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			if sub, ok := v.(map[string]any); ok {
				walk(key, sub)
				continue
			}
			keys = append(keys, key)
		}
	}
	walk("", configSchema)
	sort.Strings(keys)
	return keys
}

// Load resolves configuration from the file named by AIDEV_CONFIG. Nothing
// else in the environment is read: a variable left over in a shell profile
// must not quietly win over the file someone is reading and editing.
func Load(lookup Lookup) (Config, error) {
	if lookup == nil {
		lookup = OSLookup
	}
	raw, ok := lookup("AIDEV_CONFIG")
	if !ok || strings.TrimSpace(raw) == "" {
		return Config{}, errors.New("AIDEV_CONFIG is not set: set it to the absolute path of your conf.json (example: AIDEV_CONFIG=/home/you/aidev/conf/conf.json)")
	}
	path := strings.TrimSpace(raw)
	if !filepath.IsAbs(path) {
		return Config{}, fmt.Errorf("AIDEV_CONFIG %q must be absolute: set it to the absolute path of your conf.json", path)
	}
	return LoadFile(path)
}

// fileConfig mirrors conf.json with pointers throughout, so an absent key
// (nil) keeps its default while an explicitly empty value stays expressible —
// notably agent.opencode.model, where absent means the default model and
// empty means let OpenCode choose.
type fileConfig struct {
	Database *struct {
		URL *string `json:"url"`
	} `json:"database"`
	WorkspaceRoot *string `json:"workspace_root"`
	Tasks         *struct {
		Timeout             *string `json:"timeout"`
		VerificationTimeout *string `json:"verification_timeout"`
		MaxOutputBytes      *int    `json:"max_output_bytes"`
		WorktreeCleanup     *string `json:"worktree_cleanup"`
	} `json:"tasks"`
	Agent *struct {
		Backend  *string `json:"backend"`
		OpenCode *struct {
			Command *string `json:"command"`
			Model   *string `json:"model"`
			Agent   *string `json:"agent"`
		} `json:"opencode"`
		Codex *struct {
			Command *string `json:"command"`
			Profile *string `json:"profile"`
			Model   *string `json:"model"`
			Sandbox *string `json:"sandbox"`
		} `json:"codex"`
	} `json:"agent"`
	LogLevel *string `json:"log_level"`
	Tracing  *struct {
		Endpoint       *string           `json:"endpoint"`
		TracesEndpoint *string           `json:"traces_endpoint"`
		Headers        map[string]string `json:"headers"`
		ServiceName    *string           `json:"service_name"`
		SampleRatio    *float64          `json:"sample_ratio"`
	} `json:"tracing"`
}

// LoadFile reads the JSON configuration file at path. It reports every
// problem in one error rather than one per run.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config file %s: %w", path, err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		if syn, ok := err.(*json.SyntaxError); ok {
			return Config{}, fmt.Errorf("parse config file %s line %d: %v", path, lineOf(data, syn.Offset), err)
		}
		return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}

	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// A misspelt key would otherwise be ignored silently and the default used
	// instead, which is exactly the kind of mistake nobody finds by reading
	// the file again.
	for _, unknown := range unknownKeys("", doc, configSchema) {
		fail("unknown setting %q", unknown)
	}

	var file fileConfig
	if err := json.Unmarshal(data, &file); err != nil {
		if terr, ok := err.(*json.UnmarshalTypeError); ok {
			fail("setting %s has the wrong type: cannot use %s as %s", terr.Field, terr.Value, terr.Type)
		} else {
			fail("parse config file %s: %v", path, err)
		}
	}

	cfg := Config{
		ConfigFile:                 path,
		DefaultTaskTimeout:         DefaultTaskTimeout,
		DefaultVerificationTimeout: DefaultVerificationTimeout,
		OpenCodeCommand:            DefaultOpenCodeCommand,
		OpenCodeModel:              DefaultOpenCodeModel,
		OpenCodeAgent:              DefaultOpenCodeAgent,
		AgentBackend:               BackendOpenCode,
		CodexCommand:               DefaultCodexCommand,
		// The default sandbox must let the agent write in its worktree, or
		// every task fails at the first file it creates.
		CodexSandbox:    "workspace-write",
		MaxOutputBytes:  DefaultMaxOutputBytes,
		WorktreeCleanup: DefaultCleanupPolicy,
		LogLevel:        slog.LevelInfo,
	}

	if file.Database == nil || file.Database.URL == nil || strings.TrimSpace(*file.Database.URL) == "" {
		fail("database.url is required (example: postgres://aidev:aidev@127.0.0.1:5434/aidev?sslmode=disable)")
	} else {
		cfg.DatabaseURL = strings.TrimSpace(*file.Database.URL)
	}

	if file.WorkspaceRoot == nil || strings.TrimSpace(*file.WorkspaceRoot) == "" {
		def, err := defaultWorkspaceRoot()
		if err != nil {
			fail("workspace_root is required: %v", err)
		} else {
			cfg.WorkspaceRoot = def
		}
	} else {
		root := strings.TrimSpace(*file.WorkspaceRoot)
		// The MCP server runs with the working directory of whatever
		// repository is open, so a relative workspace_root can only mean
		// relative to the file that states it.
		if !filepath.IsAbs(root) {
			root = filepath.Join(filepath.Dir(path), root)
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			fail("workspace_root %q is not a usable path: %v", strings.TrimSpace(*file.WorkspaceRoot), err)
		} else {
			cfg.WorkspaceRoot = filepath.Clean(abs)
		}
	}

	if file.Tasks != nil {
		if file.Tasks.Timeout != nil && strings.TrimSpace(*file.Tasks.Timeout) != "" {
			d, err := parseDuration("tasks.timeout", strings.TrimSpace(*file.Tasks.Timeout))
			if err != nil {
				fail("%v", err)
			} else {
				cfg.DefaultTaskTimeout = d
			}
		}
		if file.Tasks.VerificationTimeout != nil && strings.TrimSpace(*file.Tasks.VerificationTimeout) != "" {
			d, err := parseDuration("tasks.verification_timeout", strings.TrimSpace(*file.Tasks.VerificationTimeout))
			if err != nil {
				fail("%v", err)
			} else {
				cfg.DefaultVerificationTimeout = d
			}
		}
		if file.Tasks.MaxOutputBytes != nil {
			if *file.Tasks.MaxOutputBytes < 1024 {
				fail("tasks.max_output_bytes must be at least 1024, got %d", *file.Tasks.MaxOutputBytes)
			} else {
				cfg.MaxOutputBytes = *file.Tasks.MaxOutputBytes
			}
		}
		if file.Tasks.WorktreeCleanup != nil && strings.TrimSpace(*file.Tasks.WorktreeCleanup) != "" {
			policy := CleanupPolicy(strings.ToLower(strings.TrimSpace(*file.Tasks.WorktreeCleanup)))
			if !policy.Valid() {
				names := make([]string, 0, len(AllCleanupPolicies()))
				for _, p := range AllCleanupPolicies() {
					names = append(names, string(p))
				}
				fail("tasks.worktree_cleanup %q is not one of %s", strings.TrimSpace(*file.Tasks.WorktreeCleanup), strings.Join(names, ", "))
			} else {
				cfg.WorktreeCleanup = policy
			}
		}
	}

	if file.Agent != nil {
		if file.Agent.Backend != nil && strings.TrimSpace(*file.Agent.Backend) != "" {
			backend := Backend(strings.ToLower(strings.TrimSpace(*file.Agent.Backend)))
			if !backend.Valid() {
				names := make([]string, 0, len(AllBackends()))
				for _, b := range AllBackends() {
					names = append(names, string(b))
				}
				fail("agent.backend %q is not one of %s", strings.TrimSpace(*file.Agent.Backend), strings.Join(names, ", "))
			} else {
				cfg.AgentBackend = backend
			}
		}
		if file.Agent.OpenCode != nil {
			if file.Agent.OpenCode.Command != nil && strings.TrimSpace(*file.Agent.OpenCode.Command) != "" {
				cfg.OpenCodeCommand = strings.TrimSpace(*file.Agent.OpenCode.Command)
			}
			// An absent model takes the default; an explicitly empty one is
			// how a user asks OpenCode to choose. The file must keep both
			// intents expressible.
			if file.Agent.OpenCode.Model != nil {
				cfg.OpenCodeModel = strings.TrimSpace(*file.Agent.OpenCode.Model)
			}
			if file.Agent.OpenCode.Agent != nil && strings.TrimSpace(*file.Agent.OpenCode.Agent) != "" {
				cfg.OpenCodeAgent = strings.TrimSpace(*file.Agent.OpenCode.Agent)
			}
		}
		if file.Agent.Codex != nil {
			if file.Agent.Codex.Command != nil && strings.TrimSpace(*file.Agent.Codex.Command) != "" {
				cfg.CodexCommand = strings.TrimSpace(*file.Agent.Codex.Command)
			}
			if file.Agent.Codex.Profile != nil {
				cfg.CodexProfile = strings.TrimSpace(*file.Agent.Codex.Profile)
			}
			if file.Agent.Codex.Model != nil {
				cfg.CodexModel = strings.TrimSpace(*file.Agent.Codex.Model)
			}
			if file.Agent.Codex.Sandbox != nil && strings.TrimSpace(*file.Agent.Codex.Sandbox) != "" {
				cfg.CodexSandbox = strings.TrimSpace(*file.Agent.Codex.Sandbox)
			}
		}
	}

	if file.LogLevel != nil && strings.TrimSpace(*file.LogLevel) != "" {
		lvl, err := ParseLevel(strings.TrimSpace(*file.LogLevel))
		if err != nil {
			fail("%v", err)
		} else {
			cfg.LogLevel = lvl
		}
	}

	if file.Tracing != nil {
		if file.Tracing.Endpoint != nil {
			cfg.Tracing.Endpoint = strings.TrimSpace(*file.Tracing.Endpoint)
		}
		if file.Tracing.TracesEndpoint != nil {
			cfg.Tracing.TracesEndpoint = strings.TrimSpace(*file.Tracing.TracesEndpoint)
		}
		if file.Tracing.Headers != nil {
			cfg.Tracing.Headers = file.Tracing.Headers
			for name := range file.Tracing.Headers {
				if strings.TrimSpace(name) == "" {
					fail("tracing.headers has an empty name: header names must not be empty")
					break
				}
			}
		}
		if file.Tracing.ServiceName != nil {
			cfg.Tracing.ServiceName = strings.TrimSpace(*file.Tracing.ServiceName)
		}
		if file.Tracing.SampleRatio != nil {
			if *file.Tracing.SampleRatio < 0 || *file.Tracing.SampleRatio > 1 {
				fail("tracing.sample_ratio %v is outside 0..1", *file.Tracing.SampleRatio)
			} else {
				cfg.Tracing.SampleRatio = file.Tracing.SampleRatio
			}
		}
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// unknownKeys walks doc against the schema, naming every key that is not in
// it by its full dotted path.
func unknownKeys(prefix string, doc map[string]any, schema map[string]any) []string {
	var unknown []string
	for k, v := range doc {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		// tracing.headers maps arbitrary header names to values, so its
		// contents are values rather than further settings.
		if key == "tracing.headers" {
			continue
		}
		node, ok := schema[k]
		if !ok {
			unknown = append(unknown, key)
			continue
		}
		sub, isMap := node.(map[string]any)
		if !isMap {
			continue
		}
		obj, isObj := v.(map[string]any)
		if !isObj {
			// Not an object: the typed decode reports the wrong type, so
			// there is no unknown key to add here.
			continue
		}
		unknown = append(unknown, unknownKeys(key, obj, sub)...)
	}
	sort.Strings(unknown)
	return unknown
}

// lineOf computes the 1-based line of a byte offset, so a malformed file can
// be located without opening an editor at the wrong place.
func lineOf(data []byte, offset int64) int64 {
	var line int64 = 1
	for i := int64(0); i < offset && i < int64(len(data)); i++ {
		if data[i] == '\n' {
			line++
		}
	}
	return line
}

// ParseLevel maps a log_level string onto a slog level.
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
		return 0, fmt.Errorf("log_level %q is not one of debug, info, warn, error", s)
	}
}

// Redacted returns the configuration with secrets removed, for logging. aidev
// never logs a connection string or a tracing credential verbatim.
func (c Config) Redacted() Config {
	c.DatabaseURL = RedactURL(c.DatabaseURL)
	if c.Tracing.Headers != nil {
		redacted := make(map[string]string, len(c.Tracing.Headers))
		for k := range c.Tracing.Headers {
			redacted[k] = "***"
		}
		c.Tracing.Headers = redacted
	}
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

func parseDuration(key, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a duration (examples: 90s, 30m, 2h)", key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, raw)
	}
	return d, nil
}

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
