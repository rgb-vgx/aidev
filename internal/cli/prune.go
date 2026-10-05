package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"aidev/internal/agent"
	"aidev/internal/store"
	"aidev/internal/worker"
)

// runPrune reclaims the space captured output takes (research C6). It clears
// stdout, stderr and diff on runs of finished tasks older than a cutoff and
// leaves everything else — the run records, their outcomes and the event log
// — in place. The cutoff is required: a default would make the first
// careless invocation the one that empties the database.
func runPrune(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	olderThan := fs.String("logs-older-than", "", "clear output of runs that finished longer ago than this, e.g. 30d or 72h (required)")
	dryRun := fs.Bool("dry-run", false, "report what would be cleared, without clearing anything")
	asJSON := fs.Bool("json", false, "print as JSON")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev prune --logs-older-than AGE [--dry-run] [--json]

Clears the captured stdout, stderr and diff of agent and verification runs that
finished more than AGE ago, on tasks that have finished, and marks them
logs_pruned. Outcomes, exit codes, timings, the agent's summary and the event
log are kept. AGE is a number of days (30d) or a Go duration (72h).
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usagef("aidev prune: %v", err)
	}
	if fs.NArg() > 0 {
		return usagef("aidev prune takes no arguments")
	}
	if strings.TrimSpace(*olderThan) == "" {
		return usagef("aidev prune: --logs-older-than is required, e.g. --logs-older-than 30d")
	}
	age, err := parseAge(*olderThan)
	if err != nil {
		return usagef("aidev prune: --logs-older-than: %v", err)
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	cutoff := time.Now().Add(-age)
	report, err := app.store.PruneLogs(ctx, cutoff, *dryRun)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, map[string]any{
			"cutoff": cutoff.UTC().Format(time.RFC3339),
			"report": report,
		})
	}
	verb := "cleared"
	if *dryRun {
		verb = "would clear"
	}
	fmt.Fprintf(env.Stdout, "%s %s from %d agent run(s) and %d verification step(s) finished before %s\n",
		verb, humanBytes(report.Bytes), report.WorkerRuns, report.VerificationRuns,
		cutoff.UTC().Format(time.RFC3339))
	return nil
}

// parseAge reads "30d" as thirty days, and anything else as a Go duration.
// Days are the unit retention is thought about in; time.ParseDuration stops
// at hours.
func parseAge(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	var age time.Duration
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number of days", raw)
		}
		age = time.Duration(n) * 24 * time.Hour
	} else {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("%q is neither a number of days (30d) nor a duration (72h)", raw)
		}
		age = d
	}
	if age <= 0 {
		return 0, fmt.Errorf("%q must be positive", raw)
	}
	return age, nil
}

// taskDelete removes a finished task and its whole history (research C6).
// It is for retention, not for hiding outcomes: only a task that can no
// longer change may go, and only once its worktree is off the disk, so no
// directory is left that nothing points at.
func taskDelete(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task delete", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev task delete <task>

Deletes a finished task (SUCCEEDED, FAILED or CANCELLED) with its attempts, runs,
approvals and events. A worktree still on disk must be removed first with
aidev worktree remove. The task's branch in the repository is left alone.
`)
		fs.PrintDefaults()
	}
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task delete: %v", err)
	}
	identifier, err := oneIdentifier("task delete", positionals)
	if err != nil {
		return err
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	t, err := app.store.ResolveTask(ctx, identifier)
	if err != nil {
		return err
	}
	err = app.store.DeleteTask(ctx, t.ID)
	switch {
	case errors.Is(err, store.ErrTaskNotFinished):
		return fmt.Errorf("%s is %s and can still change; cancel it first (aidev task cancel %s): %w",
			t.Identifier(), t.Status, t.Identifier(), err)
	case errors.Is(err, store.ErrWorktreeOnDisk):
		return fmt.Errorf("%s still has its worktree on disk; remove it first (aidev worktree remove %s): %w",
			t.Identifier(), t.Identifier(), err)
	case err != nil:
		return err
	}
	// The task's OpenCode database holds its agent sessions; with the task
	// gone nothing can continue them.
	if db := agent.TaskDBPath(opencodeDBDir(app.cfg), t.Identifier()); db != "" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(db + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(env.Stderr, "could not remove %s: %v\n", db+suffix, err)
			}
		}
	}
	fmt.Fprintf(env.Stdout, "deleted %s and its history\n", t.Identifier())
	fmt.Fprintf(env.Stdout, "Its branch, if it delivered one, is still in the repository.\n")
	return nil
}

// taskApply merges a SUCCEEDED task's branch into the repository's
// checked-out branch: the step from "verified" to "in my project", taken only
// when a person asks.
func taskApply(ctx context.Context, env *Env, args []string) error {
	return applyCommand(ctx, env, args, "apply", `usage: aidev task apply <task> [--json]

Merges a SUCCEEDED task's branch into the branch checked out in its repository,
with a merge commit so the change stays one commit you can find and revert. The
checkout must have no uncommitted changes to tracked files. A conflict is aborted
and reported; nothing is changed then. Undo with aidev task undo <task>.
`, func(app *app, id string) (worker.ApplyResult, error) { return app.orchestrator.Apply(ctx, id) })
}

// taskUndo reverts what taskApply merged, with a new commit.
func taskUndo(ctx context.Context, env *Env, args []string) error {
	return applyCommand(ctx, env, args, "undo", `usage: aidev task undo <task> [--json]

Reverts the merge aidev task apply made, with a new commit on the same branch —
safe even after you pushed. That branch must be checked out, with no uncommitted
changes to tracked files. Running aidev task apply again re-applies it.
`, func(app *app, id string) (worker.ApplyResult, error) { return app.orchestrator.Undo(ctx, id) })
}

func applyCommand(ctx context.Context, env *Env, args []string, name, usage string,
	do func(*app, string) (worker.ApplyResult, error)) error {
	fs := flag.NewFlagSet("task "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, usage)
		fs.PrintDefaults()
	}
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task %s: %v", name, err)
	}
	identifier, err := oneIdentifier("task "+name, positionals)
	if err != nil {
		return err
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	res, err := do(app, identifier)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, map[string]any{
			"task":      res.Task.Identifier(),
			"into":      res.Into,
			"branch":    res.Branch,
			"commit":    res.Commit,
			"reapplied": res.Reapplied,
			"message":   res.Message,
		})
	}
	fmt.Fprintln(env.Stdout, res.Message)
	return nil
}
