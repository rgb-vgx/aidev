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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aidev/internal/task"
	"aidev/internal/tracing"

	"github.com/jackc/pgx/v5/pgxpool"
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

	// ConfigFile is the path of the configuration file that was read, so
	// `aidev config` can report which file is in effect.
	ConfigFile string
}

// Lookup abstracts environment access so tests need no global state. Only
// AIDEV_CONFIG is read from it; everything else lives in the file it names.
type Lookup func(key string) (string, bool)

// OSLookup reads the real process environment.
func OSLookup(key string) (string, bool) { return os.LookupEnv(key) }

// configSchema is the vocabulary of conf.json: every key aidev understands and
// the kind of value it takes. SettingKeys, the unknown-key check and the type
// check are all derived from it, so the schema is stated once for them.
var configSchema = map[string]any{
	"database": map[string]any{
		"url": kindString,
	},
	"workspace_root": kindString,
	"tasks": map[string]any{
		"timeout":              kindString,
		"verification_timeout": kindString,
		"max_output_bytes":     kindInteger,
		"worktree_cleanup":     kindString,
	},
	"agent": map[string]any{
		"backend": kindString,
		"opencode": map[string]any{
			"command": kindString,
			"model":   kindString,
			"agent":   kindString,
		},
		"codex": map[string]any{
			"command": kindString,
			"profile": kindString,
			"model":   kindString,
			"sandbox": kindString,
		},
		// The routing table maps hardness levels to models. Its keys are
		// validated against the hardness vocabulary on load rather than
		// enumerated here, but its values are plain strings like any other
		// object-of-strings setting.
		"routing": kindStringMap,
	},
	"log_level": kindString,
	"tracing": map[string]any{
		"endpoint":        kindString,
		"traces_endpoint": kindString,
		"headers":         kindStringMap,
		"service_name":    kindString,
		"sample_ratio":    kindNumber,
	},
}

// valueKind is the kind of JSON value a setting takes.
type valueKind string

const (
	kindString    valueKind = "a string"
	kindInteger   valueKind = "a whole number"
	kindNumber    valueKind = "a number"
	kindStringMap valueKind = "an object of strings"
)

// SettingKeys returns the dotted paths of every setting, sorted. It is derived
// from configSchema rather than typed out separately, so adding a setting to the
// schema teaches every consumer at once.
//
// A setting whose value is an object of strings — tracing.headers, agent.routing
// — is one key, not one per entry: its entries are values a person writes, not
// further settings.
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
		Routing map[string]string `json:"routing"`
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
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, fmt.Errorf("config file %s is empty: it must hold a JSON object, for example "+
			`{"database": {"url": "postgres://aidev:aidev@127.0.0.1:5434/aidev?sslmode=disable"}}`, path)
	}

	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		if syn, ok := err.(*json.SyntaxError); ok {
			return Config{}, fmt.Errorf("parse config file %s line %d: %v", path, lineOf(data, syn.Offset), err)
		}
		return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
	}
	doc, isObject := parsed.(map[string]any)
	switch {
	case parsed == nil:
		doc = map[string]any{}
	case !isObject:
		// Said about the file, not in Go's terms: the reader is editing JSON.
		return Config{}, fmt.Errorf("config file %s must hold a JSON object ({ ... }), but it holds %s", path, describeJSON(parsed))
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

	// Every wrongly typed value is reported, then removed, so the rest of the
	// file is still validated in the same pass without a complaint about a
	// value the file does not contain (a string where a number belongs used to
	// read back as 0 and add "must be at least 1024, got 0").
	wrongType := map[string]bool{}
	for _, problem := range checkTypes("", doc, configSchema, wrongType) {
		fail("%s", problem)
	}

	normalized, err := json.Marshal(doc)
	if err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
	}
	var file fileConfig
	if err := json.Unmarshal(normalized, &file); err != nil {
		// checkTypes has already given every known value its expected kind.
		fail("parse config file %s: %v", path, err)
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
		if !wrongType["database"] && !wrongType["database.url"] {
			fail("database.url is required (example: postgres://aidev:aidev@127.0.0.1:5434/aidev?sslmode=disable)")
		}
	} else {
		cfg.DatabaseURL = strings.TrimSpace(*file.Database.URL)
		// PostgreSQL's own parser is the only authority on what it accepts, and a
		// URL it cannot parse will never connect: accepting it here turns a typo
		// into "the database is not reachable", advice that sends a person to
		// `make db-up` for a problem starting the database cannot fix. Parsing
		// neither resolves names nor connects.
		if _, err := pgxpool.ParseConfig(cfg.DatabaseURL); err != nil {
			fail("database.url is not a connection string PostgreSQL can parse: %v", err)
		}
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
		if file.Agent.Routing != nil {
			// Routing is the point of hardness: a person states how hard a
			// task is, and this table says which model that deserves. Keys
			// are hardness levels, so a key outside the vocabulary would
			// silently never match a task and is refused.
			keys := make([]string, 0, len(file.Agent.Routing))
			for k := range file.Agent.Routing {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			routing := make(map[string]string, len(keys))
			for _, raw := range keys {
				h, err := task.ParseHardness(raw)
				if err != nil || h == "" {
					fail("agent.routing key %q is not a hardness (want one of TRIVIAL, STANDARD, HARD)", raw)
					continue
				}
				model := strings.TrimSpace(file.Agent.Routing[raw])
				if model == "" {
					// Leaving the entry out already means "let the backend
					// choose", so an empty model is a mistake, not a choice.
					fail("agent.routing %q has an empty model: remove the entry to let the backend choose", string(h))
					continue
				}
				routing[string(h)] = model
			}
			cfg.Routing = routing
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
			// A conf.json is hand-edited, so "  x-api-key  " is what a person types,
			// and a header name cannot contain a space: the collector rejects the
			// request while aidev prints the name as if it were fine. The environment
			// loader this replaced trimmed both sides of every header.
			headers := make(map[string]string, len(file.Tracing.Headers))
			empty := false
			for name, value := range file.Tracing.Headers {
				trimmed := strings.TrimSpace(name)
				if trimmed == "" {
					empty = true
					continue
				}
				headers[trimmed] = strings.TrimSpace(value)
			}
			if empty {
				fail("tracing.headers has an empty name: header names must not be empty")
			}
			cfg.Tracing.Headers = headers
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

// checkTypes compares every known value in doc with the kind the schema expects.
// Each mismatch is reported by its dotted path, recorded in wrong, and removed from
// doc so that decoding and validating the rest cannot invent a value for it. A
// whole number written for an integer setting in any JSON form (1048576 or 1e6)
// is accepted and rewritten as an integer: JSON does not distinguish the two. A
// null is left alone and means the setting is absent.
func checkTypes(prefix string, doc map[string]any, schema map[string]any, wrong map[string]bool) []string {
	var problems []string
	for k, v := range doc {
		node, known := schema[k]
		if !known || v == nil {
			continue
		}
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		reject := func(want string) {
			problems = append(problems, fmt.Sprintf("setting %s has the wrong type: want %s, got %s", key, want, describeJSON(v)))
			wrong[key] = true
			delete(doc, k)
		}
		switch want := node.(type) {
		case map[string]any:
			obj, ok := v.(map[string]any)
			if !ok {
				reject("an object")
				continue
			}
			problems = append(problems, checkTypes(key, obj, want, wrong)...)
		case valueKind:
			if want == kindStringMap {
				// Naming the map is not enough to act on: the person has to be told
				// which of their headers is wrong, and what they actually wrote.
				m, ok := v.(map[string]any)
				if !ok {
					reject(string(want))
					continue
				}
				for _, name := range sortedKeys(m) {
					if _, isString := m[name].(string); isString {
						continue
					}
					problems = append(problems, fmt.Sprintf("setting %s.%s has the wrong type: want a string, got %s",
						key, name, describeJSON(m[name])))
					wrong[key+"."+name] = true
					delete(m, name)
				}
				doc[k] = m
				continue
			}
			fixed, ok := matchKind(want, v)
			if !ok {
				reject(string(want))
				continue
			}
			doc[k] = fixed
		}
	}
	sort.Strings(problems)
	return problems
}

// matchKind reports whether v is of kind, returning the value to keep.
func matchKind(kind valueKind, v any) (any, bool) {
	switch kind {
	case kindString:
		_, ok := v.(string)
		return v, ok
	case kindNumber:
		_, ok := v.(float64)
		return v, ok
	case kindInteger:
		f, ok := v.(float64)
		if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return nil, false
		}
		return int64(f), true
	case kindStringMap:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		for _, value := range m {
			if _, isString := value.(string); !isString {
				return nil, false
			}
		}
		return v, true
	}
	return nil, false
}

// describeJSON names a decoded JSON value's kind the way someone editing the file
// would, with the value itself when it is short enough to recognise.
func describeJSON(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("the string %q", x)
	case float64:
		return fmt.Sprintf("the number %v", x)
	case bool:
		return fmt.Sprintf("%v", x)
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return "null"
	}
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

// sortedKeys returns a map's keys in order, so that problems are reported in the
// same order however Go happens to walk the map on a given run.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
