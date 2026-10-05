package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"aidev/internal/config"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/view"
	"aidev/internal/worker"
)

// runTask dispatches the `aidev task ...` subcommands.
func runTask(ctx context.Context, env *Env, args []string) error {
	subcommands := map[string]struct {
		summary string
		run     func(context.Context, *Env, []string) error
	}{
		"create":  {"create a task", taskCreate},
		"list":    {"list tasks", taskList},
		"get":     {"show one task", taskGet},
		"run":     {"run a task: isolate, delegate, verify, record", taskRun},
		"result":  {"show the outcome of a task's latest attempt", taskResult},
		"events":  {"show a task's event history", taskEvents},
		"cancel":  {"cancel a task that has not finished", taskCancel},
		"approve": {"approve or deny a task that requires approval", taskApprove},
		"recover": {"cancel tasks whose lease expired", taskRecover},
		"delete":  {"delete a finished task and its history", taskDelete},
		"apply":   {"merge a succeeded task's branch into your checked-out branch", taskApply},
		"undo":    {"revert what aidev task apply merged", taskUndo},
	}

	writeTaskUsage := func(w *Env) {
		names := make([]string, 0, len(subcommands))
		width := 0
		for n := range subcommands {
			names = append(names, n)
			if len(n) > width {
				width = len(n)
			}
		}
		sort.Strings(names)
		fmt.Fprintf(w.Stderr, "usage: aidev task <subcommand> [flags]\n\nsubcommands:\n")
		for _, n := range names {
			fmt.Fprintf(w.Stderr, "  %-*s  %s\n", width, n, subcommands[n].summary)
		}
		fmt.Fprintf(w.Stderr, "\nRun `aidev task <subcommand> -h` for the flags of one subcommand.\n")
	}

	if len(args) == 0 {
		writeTaskUsage(env)
		return usagef("aidev task: no subcommand given")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		writeTaskUsage(env)
		return nil
	}

	sub, ok := subcommands[args[0]]
	if !ok {
		writeTaskUsage(env)
		return usagef("aidev task: unknown subcommand %q", args[0])
	}
	return sub.run(ctx, env, args[1:])
}

// parseInterspersed parses flags that may appear before, after, or between
// positional arguments.
//
// Go's flag package stops at the first non-flag argument, which would make
// `aidev task result TASK-000001 --json` silently treat --json as a second task
// identifier. That argument order is the natural one, so the parser consumes
// positionals and keeps going rather than making the user learn otherwise.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

// repeatable collects a flag that may be given more than once, which is how
// multiple verification commands are supplied.
type repeatable []string

func (r *repeatable) String() string { return strings.Join(*r, ", ") }

func (r *repeatable) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func taskCreate(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	var verify repeatable
	var protect repeatable
	var setup repeatable
	repo := fs.String("repo", "", "path to the git repository (default: the current directory)")
	title := fs.String("title", "", "short statement of what to do (required)")
	description := fs.String("description", "", "the full instruction for the agent")
	acceptance := fs.String("acceptance", "", "what done looks like")
	agentName := fs.String("agent", "", "agent to use (default: agent.opencode.agent in conf.json)")
	model := fs.String("model", "", "model to use (default: agent.opencode.model in conf.json)")
	hardness := fs.String("hardness", "", "how hard the task is: TRIVIAL, STANDARD or HARD (picks a model from agent.routing in conf.json)")
	priority := fs.Int("priority", 0, "higher runs first")
	maxRetries := fs.Int("max-retries", 0, "retry up to N more times when the checks fail or the agent stops early (0-10)")
	requiresApproval := fs.Bool("requires-approval", false, "do not run until a human approves")
	expectFailOnBase := fs.Bool("expect-fail-on-base", false, "run the verification commands on the base commit before the agent starts; fail immediately if they already pass, because commands that pass on the base cannot distinguish before from after (for bug-fix tasks)")
	baseRef := fs.String("base-ref", "", "git ref to branch from (default: the project's default branch)")
	timeout := fs.Duration("timeout", 0, "bound this task's agent run (default: tasks.timeout in conf.json)")
	asJSON := fs.Bool("json", false, "print the created task as JSON")
	fs.Var(&verify, "verify", "command aidev will run to verify the task; repeat for more than one (required)")
	fs.Var(&protect, "protect", "glob path or directory the agent must not change (.env*, migrations/*, docs); an attempt that touches a match fails verification before any check runs; repeat for more than one")
	fs.Var(&setup, "setup", "command run before verification to prepare the checkout (npm ci); repeat for more than one")
	verifyMode := fs.String("verify-mode", "", "where verification runs: in_place (default, in the agent's worktree) or clean (a fresh checkout of the result, so ignored or uncommitted files cannot make the checks pass); empty takes the project default")

	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev task create --title <title> --verify <command> [flags]

At least one --verify command is required. Only those commands decide whether a
task succeeded; the agent's own report does not.

example:
  aidev task create \
    --repo . \
    --title "Add a Greet function" \
    --description "Create greet.go with Greet(name string) string" \
    --verify 'go test ./...' \
    --verify 'go vet ./...'

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usagef("aidev task create: %v", err)
	}

	steps, err := task.ParseVerificationSteps(verify)
	if err != nil {
		return err
	}
	setupSteps, err := task.ParseVerificationSteps(setup)
	if err != nil {
		return fmt.Errorf("--setup: %w", err)
	}

	repoPath := *repo
	if strings.TrimSpace(repoPath) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("determine the current directory: %w", err)
		}
		repoPath = cwd
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	created, err := app.orchestrator.CreateTask(ctx, worker.CreateTaskInput{
		RepoPath:           repoPath,
		Title:              *title,
		Description:        *description,
		AcceptanceCriteria: *acceptance,
		Agent:              *agentName,
		Model:              *model,
		Hardness:           *hardness,
		Priority:           *priority,
		Verification:       steps,
		ProtectedPaths:     protect,
		SetupSteps:         setupSteps,
		VerificationMode:   *verifyMode,
		MaxRetries:         *maxRetries,
		RequiresApproval:   *requiresApproval,
		ExpectFailOnBase:   *expectFailOnBase,
		BaseRef:            *baseRef,
		Timeout:            *timeout,
	})
	if err != nil {
		return err
	}

	if *asJSON {
		return writeJSON(env.Stdout, view.NewTask(created))
	}
	fmt.Fprintf(env.Stdout, "created %s  %s\n", created.Ref, created.Title)
	fmt.Fprintf(env.Stdout, "  verification: %s\n", strings.Join(view.NewTask(created).Verification, ", "))
	if created.RequiresApproval {
		fmt.Fprintf(env.Stdout, "  approval required before it can run: aidev task approve %s\n", created.Ref)
	} else {
		fmt.Fprintf(env.Stdout, "  run it with: aidev task run %s\n", created.Ref)
	}
	return nil
}

func taskList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	statusFilter := fs.String("status", "", "comma-separated statuses to include")
	repo := fs.String("repo", "", "only tasks for this repository")
	limit := fs.Int("limit", 0, "maximum tasks to return")
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return usagef("aidev task list: %v", err)
	}

	var statuses []task.Status
	for _, raw := range strings.Split(*statusFilter, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parsed, err := task.ParseStatus(strings.ToUpper(raw))
		if err != nil {
			return fmt.Errorf("%w (valid: %s)", err, strings.Join(statusNames(), ", "))
		}
		statuses = append(statuses, parsed)
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	filter := store.TaskFilter{Statuses: statuses, Limit: *limit}
	if strings.TrimSpace(*repo) != "" {
		repository, err := app.orchestrator.Git.OpenRepository(ctx, *repo)
		if err != nil {
			return err
		}
		project, err := app.store.GetProjectByPath(ctx, repository.Path)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				if *asJSON {
					return writeJSON(env.Stdout, []view.Task{})
				}
				fmt.Fprintf(env.Stdout, "no tasks for %s yet\n", repository.Path)
				return nil
			}
			return err
		}
		filter.ProjectID = project.ID
	}

	tasks, err := app.store.ListTasks(ctx, filter)
	if err != nil {
		return err
	}

	if *asJSON {
		views := make([]view.Task, 0, len(tasks))
		for _, t := range tasks {
			views = append(views, view.NewTask(t))
		}
		return writeJSON(env.Stdout, views)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(env.Stdout, "no tasks")
		return nil
	}
	for _, t := range tasks {
		writeTaskLine(env.Stdout, t)
	}
	return nil
}

func taskGet(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task get", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task get: %v", err)
	}
	identifier, err := oneIdentifier("get", positionals)
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

	if *asJSON {
		return writeJSON(env.Stdout, view.NewTask(t))
	}
	writeTaskDetail(env.Stdout, t)
	return nil
}

// runBudgetMargin is the slack in the total deadline a run puts on itself, on
// top of the task's timeout and the verification budget. Those bounds cover the
// agent and the checks; the work around them — creating the worktree, the git
// operations, waiting on a pool — has no bound of its own, and a run that hangs
// between phases must still not outlive invariant 6.
const runBudgetMargin = 10 * time.Minute

// runTotalBudget is the deadline a run imposes on itself. Every attempt the
// task may take — the first and up to max_retries more, all in this one run —
// gets the agent's timeout and the verification budget. A task that expects
// its verification to fail on the base spends one more verification pass
// there, before the first attempt only.
func runTotalBudget(cfg config.Config, t task.Task) time.Duration {
	attempts := time.Duration(1 + max(t.MaxRetries, 0))
	budget := attempts*(t.EffectiveTimeout(cfg.DefaultTaskTimeout)+cfg.VerificationTotalTimeout) + runBudgetMargin
	if t.ExpectFailOnBase {
		budget += cfg.VerificationTotalTimeout
	}
	return budget
}

func taskRun(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task run", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print the outcome as JSON")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev task run <task> [--json]

Runs the task to completion: creates an isolated git worktree, runs the agent in
it, then runs the task's own verification commands and records the outcome.

This can take several minutes. The first run against a repository OpenCode has
not seen before can take longer still. Ctrl-C cancels it and records the
cancellation; the worktree is kept. A task with --max-retries that fails in a
way another try can fix (the checks failed, or the agent stopped early) starts
its next attempt within the same run, in the same worktree. A total deadline
also applies — the task's timeout plus the verification budget for every
attempt it may take, plus a margin — so a run that hangs cannot last forever.
`)
		fs.PrintDefaults()
	}
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task run: %v", err)
	}
	identifier, err := oneIdentifier("run", positionals)
	if err != nil {
		return err
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	// The run bounds itself, because nothing else will: the agent has its
	// timeout and verification has its budget, but nothing bounded the two
	// together, and this command — which the MCP server spawns as a detached
	// child (research C2) — must return either way. Resolving here costs one
	// query; RunTask resolves the task again itself.
	t, err := app.store.ResolveTask(ctx, identifier)
	if err != nil {
		return err
	}
	budget := runTotalBudget(app.cfg, t)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	outcome, runErr := app.orchestrator.RunTask(ctx, identifier)

	// An approval gate is not a failure: report it and exit 0 so that a script
	// can distinguish "blocked, waiting for a human" from "something broke".
	if errors.Is(runErr, worker.ErrApprovalRequired) {
		if *asJSON {
			return writeJSON(env.Stdout, buildResultView(outcome, nil, false))
		}
		fmt.Fprintln(env.Stdout, outcome.Message)
		fmt.Fprintf(env.Stdout, "approve it with: aidev task approve %s\n", outcome.Task.Identifier())
		return nil
	}
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("the run for %s exceeded its total budget of %s: %w", t.Identifier(), budget, runErr)
		}
		return runErr
	}

	var runs []task.VerificationRun
	if outcome.Attempt != nil {
		runs, _ = app.store.ListVerificationRuns(ctx, outcome.Attempt.ID)
	}

	if *asJSON {
		if err := writeJSON(env.Stdout, buildResultView(outcome, runs, false)); err != nil {
			return err
		}
	} else {
		writeRunOutcome(env, outcome, runs)
	}

	// A task that did not succeed exits non-zero, so `aidev task run X && deploy`
	// behaves the way a shell user expects. When the deadline is what stopped
	// it, say so: the outcome above is recorded, but the reason deserves to be
	// on stderr too, and it must not look like a plain verification failure.
	if outcome.Task.Status != task.StatusSucceeded {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("the run for %s exceeded its total budget of %s (task timeout + verification budget + margin)", t.Identifier(), budget)
		}
		return &exitError{code: 1}
	}
	return nil
}

func taskResult(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task result", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	withLogs := fs.Bool("logs", false, "include the agent transcript and full verification output")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task result: %v", err)
	}
	identifier, err := oneIdentifier("result", positionals)
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

	outcome := worker.Outcome{Task: t}
	var runs []task.VerificationRun
	var stdout, stderr, diff string

	attempt, err := app.store.LatestAttempt(ctx, t.ID)
	switch {
	case err == nil:
		outcome.Attempt = &attempt
		runs, _ = app.store.ListVerificationRuns(ctx, attempt.ID)
		// Best-effort, like the approval read below: the report is a courtesy
		// to the reader and must not decide whether the result can be shown.
		if tests, err := app.store.TestsModifiedPaths(ctx, t.ID, attempt.ID); err == nil {
			outcome.TestsModified = tests
		}

		if workerRuns, err := app.store.ListWorkerRuns(ctx, attempt.ID); err == nil && len(workerRuns) > 0 {
			latest := workerRuns[len(workerRuns)-1]
			outcome.WorkerRun = &latest
			stdout, stderr, diff = latest.Stdout, latest.Stderr, latest.Diff
		}
		if wt, err := app.store.GetWorktreeByAttempt(ctx, attempt.ID); err == nil {
			outcome.Worktree = &wt
		}
	case errors.Is(err, store.ErrNotFound):
		// A task that has never run has no attempt; that is not an error.
	default:
		return err
	}

	if approval, err := app.store.LatestApproval(ctx, t.ID); err == nil {
		outcome.Approval = &approval
	}

	var earlier []task.TaskAttempt
	if outcome.Attempt != nil {
		if all, err := app.store.ListAttempts(ctx, t.ID); err == nil {
			for _, a := range all {
				if a.ID == outcome.Attempt.ID {
					continue
				}
				earlier = append(earlier, a)
			}
			// ListAttempts returns newest first; earlier attempts read oldest first.
			for i, j := 0, len(earlier)-1; i < j; i, j = i+1, j-1 {
				earlier[i], earlier[j] = earlier[j], earlier[i]
			}
		}
	}

	if *asJSON {
		resultView := buildResultView(outcome, runs, *withLogs)
		if len(earlier) > 0 {
			views := make([]view.Attempt, 0, len(earlier))
			for _, a := range earlier {
				views = append(views, *view.NewAttempt(a))
			}
			resultView.EarlierAttempts = views
		}
		if *withLogs {
			return writeJSON(env.Stdout, struct {
				view.Result
				Stdout string `json:"agent_stdout,omitempty"`
				Stderr string `json:"agent_stderr,omitempty"`
				Diff   string `json:"diff,omitempty"`
			}{Result: resultView, Stdout: stdout, Stderr: stderr, Diff: diff})
		}
		return writeJSON(env.Stdout, resultView)
	}

	writeResult(env, outcome, runs, earlier)
	if *withLogs {
		writeLogs(env, stdout, stderr, diff)
	}
	return nil
}

func taskEvents(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task events", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	limit := fs.Int("limit", 0, "maximum events to return")
	afterSeq := fs.Int64("after", 0, "only events after this sequence number")
	withPayload := fs.Bool("payload", false, "include each event's payload")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task events: %v", err)
	}
	identifier, err := oneIdentifier("events", positionals)
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
	events, err := app.store.ListEvents(ctx, store.EventFilter{
		TaskID:   t.ID,
		AfterSeq: *afterSeq,
		Limit:    *limit,
	})
	if err != nil {
		return err
	}

	if *asJSON {
		views := make([]view.Event, 0, len(events))
		for _, e := range events {
			views = append(views, view.NewEvent(e, true))
		}
		return writeJSON(env.Stdout, views)
	}
	if len(events) == 0 {
		fmt.Fprintln(env.Stdout, "no events")
		return nil
	}
	for _, e := range events {
		fmt.Fprintf(env.Stdout, "%-6d  %-22s  %s\n",
			e.Seq, e.CreatedAt.UTC().Format("15:04:05.000"), e.Type)
		if *withPayload && len(e.Payload) > 2 {
			fmt.Fprintf(env.Stdout, "        %s\n", string(e.Payload))
		}
	}
	return nil
}

func taskCancel(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task cancel", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	reason := fs.String("reason", "cancelled from the command line", "why it is being cancelled")
	asJSON := fs.Bool("json", false, "print as JSON")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task cancel: %v", err)
	}
	identifier, err := oneIdentifier("cancel", positionals)
	if err != nil {
		return err
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	outcome, err := app.orchestrator.Cancel(ctx, identifier, *reason)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, buildResultView(outcome, nil, false))
	}
	fmt.Fprintln(env.Stdout, outcome.Message)
	return nil
}

func taskRecover(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task recover", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dryRun := fs.Bool("dry-run", false, "list the tasks that would be cancelled, without cancelling anything")
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return usagef("aidev task recover: %v", err)
	}
	if fs.NArg() > 0 {
		return usagef("aidev task recover takes no arguments")
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	recovered, err := app.orchestrator.Recover(ctx, *dryRun)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, map[string]any{
			"dry_run": *dryRun,
			"tasks":   recovered,
		})
	}
	if len(recovered) == 0 {
		fmt.Fprintln(env.Stdout, "no tasks with an expired lease")
		return nil
	}
	for _, r := range recovered {
		action := "cancelled"
		if r.Action == "would_cancel" {
			action = "would cancel"
		}
		fmt.Fprintf(env.Stdout, "%s  %-14s  %s\n", r.Ref, action, r.Reason)
	}
	return nil
}

func taskApprove(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task approve", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	deny := fs.Bool("deny", false, "deny instead of approving, which fails the task")
	by := fs.String("by", "", "who is deciding (default: $USER)")
	reason := fs.String("reason", "", "why")
	asJSON := fs.Bool("json", false, "print as JSON")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task approve: %v", err)
	}
	identifier, err := oneIdentifier("approve", positionals)
	if err != nil {
		return err
	}

	decidedBy := strings.TrimSpace(*by)
	if decidedBy == "" {
		decidedBy = config.EnvUser()
	}
	if decidedBy == "" {
		decidedBy = "unknown"
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	outcome, err := app.orchestrator.Approve(ctx, identifier, !*deny, decidedBy, *reason, "cli")
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, buildResultView(outcome, nil, false))
	}
	fmt.Fprintln(env.Stdout, outcome.Message)
	if !*deny {
		fmt.Fprintf(env.Stdout, "run it with: aidev task run %s\n", outcome.Task.Identifier())
	}
	return nil
}

// oneIdentifier enforces that exactly one task identifier was given, so a typo
// like `aidev task get TASK-1 TASK-2` is reported rather than half-obeyed.
func oneIdentifier(command string, args []string) (string, error) {
	switch len(args) {
	case 1:
		return args[0], nil
	case 0:
		return "", usagef("aidev task %s: a task reference or id is required (for example TASK-000001)", command)
	default:
		return "", usagef("aidev task %s: expected one task, got %d: %s", command, len(args), strings.Join(args, " "))
	}
}

func statusNames() []string {
	names := make([]string, 0, len(task.AllStatuses()))
	for _, s := range task.AllStatuses() {
		names = append(names, s.String())
	}
	return names
}

func buildResultView(outcome worker.Outcome, runs []task.VerificationRun, includeOutput bool) view.Result {
	return view.NewResult(outcome.Task, outcome.Attempt, outcome.WorkerRun, runs, outcome.Worktree, outcome.Approval, outcome.TestsModified, outcome.Message, includeOutput)
}

// exitError carries a specific exit status without being an error message.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Code reports the status main should exit with.
func (e *exitError) Code() int { return e.code }
