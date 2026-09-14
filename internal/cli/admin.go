package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"aidev/internal/config"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/migrations"
)

// valueOrNone shows a setting that is empty when unset, so that an absent
// value is visible rather than a blank line that looks like missing output.
func valueOrNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

// formatHeaders renders tracing headers with their redacted values, sorted so
// the output is stable. The values are already redacted by Config.Redacted.
func formatHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+headers[name])
	}
	return strings.Join(pairs, ", ")
}

// formatSampleRatio shows the configured ratio, or that none is set and the
// tracing default of 1 applies.
func formatSampleRatio(ratio *float64) string {
	if ratio == nil {
		return "(none)"
	}
	return fmt.Sprintf("%v", *ratio)
}

// runConfig prints the resolved configuration so that a developer can see what
// aidev actually read, rather than what they believe the environment contains.
func runConfig(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return usagef("aidev config: %v", err)
	}

	cfg, err := config.Load(config.OSLookup)
	if err != nil {
		return err
	}
	cfg = cfg.Redacted()

	if *asJSON {
		return writeJSON(env.Stdout, map[string]any{
			"config_file":                  cfg.ConfigFile,
			"database_url":                 cfg.DatabaseURL,
			"workspace_root":               cfg.WorkspaceRoot,
			"default_task_timeout":         cfg.DefaultTaskTimeout.String(),
			"default_verification_timeout": cfg.DefaultVerificationTimeout.String(),
			"agent_backend":                cfg.AgentBackend.String(),
			"opencode_command":             cfg.OpenCodeCommand,
			"opencode_model":               cfg.OpenCodeModel,
			"opencode_agent":               cfg.OpenCodeAgent,
			"codex_command":                cfg.CodexCommand,
			"codex_profile":                cfg.CodexProfile,
			"codex_model":                  cfg.CodexModel,
			"codex_sandbox":                cfg.CodexSandbox,
			"max_output_bytes":             cfg.MaxOutputBytes,
			"worktree_cleanup":             cfg.WorktreeCleanup.String(),
			"log_level":                    cfg.LogLevel.String(),
			"tracing_endpoint":             cfg.Tracing.Endpoint,
			"tracing_traces_endpoint":      cfg.Tracing.TracesEndpoint,
			"tracing_headers":              cfg.Tracing.Headers,
			"tracing_service_name":         cfg.Tracing.ServiceName,
			"tracing_sample_ratio":         cfg.Tracing.SampleRatio,
		})
	}

	model := cfg.OpenCodeModel
	if model == "" {
		model = "(let opencode choose)"
	}
	codexProfile := cfg.CodexProfile
	if codexProfile == "" {
		codexProfile = "(none)"
	}
	codexModel := cfg.CodexModel
	if codexModel == "" {
		codexModel = "(let codex choose)"
	}
	codexSandbox := cfg.CodexSandbox
	if codexSandbox == "" {
		codexSandbox = "(none)"
	}
	fmt.Fprintf(env.Stdout, "config file                   %s\n", cfg.ConfigFile)
	fmt.Fprintf(env.Stdout, "database url                  %s\n", cfg.DatabaseURL)
	fmt.Fprintf(env.Stdout, "workspace root                %s\n", cfg.WorkspaceRoot)
	fmt.Fprintf(env.Stdout, "default task timeout          %s\n", cfg.DefaultTaskTimeout)
	fmt.Fprintf(env.Stdout, "default verification timeout  %s\n", cfg.DefaultVerificationTimeout)
	fmt.Fprintf(env.Stdout, "agent backend                 %s\n", cfg.AgentBackend)
	fmt.Fprintf(env.Stdout, "opencode command              %s\n", cfg.OpenCodeCommand)
	fmt.Fprintf(env.Stdout, "opencode model                %s\n", model)
	fmt.Fprintf(env.Stdout, "opencode agent                %s\n", cfg.OpenCodeAgent)
	fmt.Fprintf(env.Stdout, "codex command                 %s\n", cfg.CodexCommand)
	fmt.Fprintf(env.Stdout, "codex profile                 %s\n", codexProfile)
	fmt.Fprintf(env.Stdout, "codex model                   %s\n", codexModel)
	fmt.Fprintf(env.Stdout, "codex sandbox                 %s\n", codexSandbox)
	fmt.Fprintf(env.Stdout, "max output bytes              %d\n", cfg.MaxOutputBytes)
	fmt.Fprintf(env.Stdout, "worktree cleanup              %s\n", cfg.WorktreeCleanup)
	fmt.Fprintf(env.Stdout, "log level                     %s\n", cfg.LogLevel)
	fmt.Fprintf(env.Stdout, "tracing endpoint              %s\n", valueOrNone(cfg.Tracing.Endpoint))
	fmt.Fprintf(env.Stdout, "tracing traces endpoint       %s\n", valueOrNone(cfg.Tracing.TracesEndpoint))
	fmt.Fprintf(env.Stdout, "tracing headers               %s\n", formatHeaders(cfg.Tracing.Headers))
	fmt.Fprintf(env.Stdout, "tracing service name          %s\n", valueOrNone(cfg.Tracing.ServiceName))
	fmt.Fprintf(env.Stdout, "tracing sample ratio          %s\n", formatSampleRatio(cfg.Tracing.SampleRatio))
	return nil
}

// runMigrate brings the database schema up to date.
func runMigrate(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return usagef("aidev migrate: %v", err)
	}

	cfg, err := config.Load(config.OSLookup)
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel)

	pending, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		return err
	}

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := store.Open(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	start := time.Now()
	result, err := db.Migrate(ctx, pending)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "migrations complete",
		"applied", len(result.Applied),
		"already_up_to_date", len(result.AlreadyUp),
		logging.FieldDurationMS, time.Since(start).Milliseconds())

	if *asJSON {
		return writeJSON(env.Stdout, map[string]any{
			"applied":    result.Applied,
			"already_up": result.AlreadyUp,
		})
	}

	if len(result.Applied) == 0 {
		fmt.Fprintf(env.Stdout, "database is up to date (%d migrations)\n", len(result.AlreadyUp))
		return nil
	}
	for _, v := range result.Applied {
		fmt.Fprintf(env.Stdout, "applied %s\n", v)
	}
	fmt.Fprintf(env.Stdout, "%d migration(s) applied\n", len(result.Applied))
	return nil
}

// writeJSON emits indented JSON with a trailing newline, the form that is
// pleasant both for a human reading a terminal and for `jq`.
func writeJSON(w interface{ Write([]byte) (int, error) }, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
