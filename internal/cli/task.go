package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

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
	repo := fs.String("repo", "", "path to the git repository (default: the current directory)")
	title := fs.String("title", "", "short statement of what to do (required)")
	description := fs.String("description", "", "the full instruction for the agent")
	acceptance := fs.String("acceptance", "", "what done looks like")
	agentName := fs.String("agent", "", "agent to use (default: agent.opencode.agent in conf.json)")
	model := fs.String("model", "", "model to use (default: agent.opencode.model in conf.json)")
	hardness := fs.String("hardness", "", "how hard the task is: TRIVIAL, STANDARD or HARD (picks a model from agent.routing in conf.json)")
	priority := fs.Int("priority", 0, "higher runs first")
	maxRetries := fs.Int("max-retries", 0, "recorded for a future retry feature; the MVP never retries")
	requiresApproval := fs.Bool("requires-approval", false, "do not run until a human approves")
	baseRef := fs.String("base-ref", "", "git ref to branch from (default: the project's default branch)")
	timeout := fs.Duration("timeout", 0, "bound this task's agent run (default: tasks.timeout in conf.json)")
	asJSON := fs.Bool("json", false, "print the created task as JSON")
	fs.Var(&verify, "verify", "command aidev will run to verify the task; repeat for more than one (required)")

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
		MaxRetries:         *maxRetries,
		RequiresApproval:   *requiresApproval,
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
cancellation; the worktree is kept.
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
	// behaves the way a shell user expects.
	if outcome.Task.Status != task.StatusSucceeded {
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

	if *asJSON {
		resultView := buildResultView(outcome, runs, *withLogs)
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

	writeResult(env, outcome, runs)
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
		decidedBy = os.Getenv("USER")
	}
	if decidedBy == "" {
		decidedBy = "unknown"
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	outcome, err := app.orchestrator.Approve(ctx, identifier, !*deny, decidedBy, *reason)
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
	return view.NewResult(outcome.Task, outcome.Attempt, outcome.WorkerRun, runs, outcome.Worktree, outcome.Approval, outcome.Message, includeOutput)
}

// exitError carries a specific exit status without being an error message.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Code reports the status main should exit with.
func (e *exitError) Code() int { return e.code }
