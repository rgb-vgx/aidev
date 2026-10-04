package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/view"
	"aidev/internal/worker"
)

// approvalNextStep says what has to happen before a gated task runs. With
// mcp.allow_approval off — the default — the tool it would name refuses the
// decision, so pointing the planner at it would send it into a wall; the
// answer then is the operator's CLI.
func approvalNextStep(cfg config.Config, ref string) string {
	if cfg.MCPAllowApproval {
		return fmt.Sprintf("this task requires approval; a human must call %s before it can run", ToolApprove)
	}
	return fmt.Sprintf("this task requires approval; a human must run: aidev task approve %s --by <name>", ref)
}

// Tool names. They are prefixed so they are unambiguous in a client that has
// several servers connected.
const (
	ToolCreateTask = "aidev_create_task"
	ToolGetTask    = "aidev_get_task"
	ToolListTasks  = "aidev_list_tasks"
	ToolRunTask    = "aidev_run_task"
	ToolCancelTask = "aidev_cancel_task"
	ToolGetResult  = "aidev_get_task_result"
	ToolGetEvents  = "aidev_get_task_events"
	ToolApprove    = "aidev_approve_task"
)

// register adds every tool. Input and output schemas are generated from the Go
// types, so a schema cannot drift from the code that produces the value.
func (s *Server) register(server *sdk.Server) {
	sdk.AddTool(server, &sdk.Tool{
		Name: ToolCreateTask,
		Description: "Create an implementation task for a git repository. " +
			"At least one verification command is required: aidev runs those commands itself " +
			"to decide whether the task succeeded, and will not accept a task it cannot check. " +
			"State how hard the task is with hardness so aidev can pick the right model. " +
			"Creating a task does not run it; use " + ToolRunTask + " for that.",
	}, s.createTask)

	sdk.AddTool(server, &sdk.Tool{
		Name:        ToolGetTask,
		Description: "Fetch one task by its reference (for example TASK-000001) or UUID.",
	}, s.getTask)

	sdk.AddTool(server, &sdk.Tool{
		Name:        ToolListTasks,
		Description: "List tasks, newest first, optionally filtered by repository and status.",
	}, s.listTasks)

	sdk.AddTool(server, &sdk.Tool{
		Name: ToolRunTask,
		Description: "Run a task: aidev creates an isolated git worktree, runs the coding agent " +
			"inside it, then runs the task's verification commands itself and records the outcome. " +
			"This takes as long as the agent does, often minutes. The tool waits for a bounded " +
			"time and then returns with the task still RUNNING; poll " + ToolGetResult + " for the " +
			"final outcome. A task that requires approval is not run and is reported as " +
			"WAITING_APPROVAL.",
	}, s.runTask)

	sdk.AddTool(server, &sdk.Tool{
		Name: ToolGetResult,
		Description: "Read the outcome of a task's most recent attempt: what the agent did, what " +
			"aidev's verification found, and where the work is. The verification results are the " +
			"evidence; the agent's summary is only its own claim.",
	}, s.getResult)

	sdk.AddTool(server, &sdk.Tool{
		Name: ToolGetEvents,
		Description: "Read a task's event history in order. Useful to see how far a running task " +
			"has got, and to follow along by passing the highest seq already seen as after_seq.",
	}, s.getEvents)

	sdk.AddTool(server, &sdk.Tool{
		Name: ToolCancelTask,
		Description: "Cancel a task that has not finished. Any worktree is kept so the partial " +
			"work can be inspected.",
	}, s.cancelTask)

	sdk.AddTool(server, &sdk.Tool{
		Name: ToolApprove,
		Description: "Grant or deny approval for a task marked as requiring it. Granting returns " +
			"the task to READY so it can be run; denying fails it. This is a human decision: do " +
			"not call it on your own initiative.",
	}, s.approveTask)
}

// CreateTaskInput is the input of aidev_create_task.
type CreateTaskInput struct {
	RepoPath string `json:"repo_path" jsonschema:"absolute path to the git repository the task applies to"`
	Title    string `json:"title" jsonschema:"one short line stating what to do"`

	Verification []string `json:"verification" jsonschema:"commands aidev will run itself to decide whether the task succeeded, for example [\"go test ./...\", \"go vet ./...\"]. At least one is required. They run in the task's worktree, without a shell, so pipes and redirection are not available"`

	ProtectedPaths []string `json:"protected_paths,omitempty" jsonschema:"glob paths the agent must not change, for example [\".env*\", \"migrations/*\", \"ci\"]; an attempt that changes a matching path fails verification without running any check"`

	SetupSteps []string `json:"setup_steps,omitempty" jsonschema:"commands run before verification to prepare the checkout, for example [\"npm ci\"]; they run where verification runs, without a shell, and a failing setup fails the task"`

	VerificationMode string `json:"verification_mode,omitempty" jsonschema:"where verification runs: in_place (default, in the agent's worktree) or clean (a fresh checkout of the result, so files git ignores or the agent never committed cannot make the checks pass); empty takes the project's default and the choice is frozen into the task"`

	Description        string `json:"description,omitempty" jsonschema:"the full instruction for the agent: what to build and where. Be specific about file names and signatures"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty" jsonschema:"what done looks like, in prose"`
	Agent              string `json:"agent,omitempty" jsonschema:"agent to use; defaults to the configured one (build)"`
	Priority           int    `json:"priority,omitempty" jsonschema:"higher runs first; defaults to 0"`
	RequiresApproval   bool   `json:"requires_approval,omitempty" jsonschema:"when true the task will not run until a human approves it"`
	ExpectFailOnBase   bool   `json:"expect_fail_on_base,omitempty" jsonschema:"when true the verification commands run on the base commit before the agent starts; if they already pass there they cannot distinguish before from after, so the attempt fails with kind VERIFICATION and the agent is never called (for bug-fix tasks)"`
	BaseRef            string `json:"base_ref,omitempty" jsonschema:"git ref the task's branch starts from; defaults to the repository's current branch"`
	TimeoutSeconds     int    `json:"timeout_seconds,omitempty" jsonschema:"bound this task's agent run; defaults to the configured timeout"`
	Hardness           string `json:"hardness,omitempty" jsonschema:"how hard the task is: TRIVIAL, STANDARD or HARD, case-insensitive; picks the model from agent.routing in conf.json unless model is given"`
	Model              string `json:"model,omitempty" jsonschema:"model to run on, overriding agent.routing and agent.opencode.model"`
}

// CreateTaskOutput is the output of aidev_create_task.
type CreateTaskOutput struct {
	Task     view.Task `json:"task" jsonschema:"the created task, in status PENDING"`
	NextStep string    `json:"next_step" jsonschema:"what to do next"`
}

func (s *Server) createTask(ctx context.Context, _ *sdk.CallToolRequest, in CreateTaskInput) (*sdk.CallToolResult, CreateTaskOutput, error) {
	// Checked before connecting: a request that cannot work should be answered with
	// what is wrong with it, not with "the database is not reachable", and it should
	// not spend a connection attempt.
	steps, err := task.ParseVerificationSteps(in.Verification)
	if err != nil {
		return nil, CreateTaskOutput{}, err
	}
	if len(steps) == 0 {
		return nil, CreateTaskOutput{}, task.ErrVerificationRequired
	}
	setupSteps, err := task.ParseVerificationSteps(in.SetupSteps)
	if err != nil {
		return nil, CreateTaskOutput{}, fmt.Errorf("setup_steps: %w", err)
	}

	orchestrator, _, err := s.connected(ctx)
	if err != nil {
		return nil, CreateTaskOutput{}, err
	}

	created, err := orchestrator.CreateTask(ctx, worker.CreateTaskInput{
		RepoPath:           in.RepoPath,
		Title:              in.Title,
		Description:        in.Description,
		AcceptanceCriteria: in.AcceptanceCriteria,
		Agent:              in.Agent,
		Priority:           in.Priority,
		Verification:       steps,
		ProtectedPaths:     in.ProtectedPaths,
		SetupSteps:         setupSteps,
		VerificationMode:   in.VerificationMode,
		RequiresApproval:   in.RequiresApproval,
		ExpectFailOnBase:   in.ExpectFailOnBase,
		BaseRef:            in.BaseRef,
		Hardness:           in.Hardness,
		Model:              in.Model,
		Timeout:            time.Duration(in.TimeoutSeconds) * time.Second,
	})
	if err != nil {
		return nil, CreateTaskOutput{}, err
	}

	next := fmt.Sprintf("run it with %s", ToolRunTask)
	if created.RequiresApproval {
		next = approvalNextStep(orchestrator.Config, created.Ref)
	}
	return nil, CreateTaskOutput{Task: view.NewTask(created), NextStep: next}, nil
}

// TaskInput identifies one task.
type TaskInput struct {
	Task string `json:"task" jsonschema:"task reference such as TASK-000001, or the task's UUID"`
}

// TaskOutput carries one task.
type TaskOutput struct {
	Task view.Task `json:"task" jsonschema:"the task"`
}

func (s *Server) getTask(ctx context.Context, _ *sdk.CallToolRequest, in TaskInput) (*sdk.CallToolResult, TaskOutput, error) {
	_, st, err := s.connected(ctx)
	if err != nil {
		return nil, TaskOutput{}, err
	}
	t, err := s.resolve(ctx, st, in.Task)
	if err != nil {
		return nil, TaskOutput{}, err
	}
	return nil, TaskOutput{Task: view.NewTask(t)}, nil
}

// ListTasksInput is the input of aidev_list_tasks.
type ListTasksInput struct {
	RepoPath string   `json:"repo_path,omitempty" jsonschema:"only tasks for this git repository"`
	Statuses []string `json:"statuses,omitempty" jsonschema:"only these statuses, for example [\"FAILED\"]. Valid values: PENDING, READY, RUNNING, VERIFYING, SUCCEEDED, FAILED, CANCELLED, WAITING_APPROVAL"`
	Limit    int      `json:"limit,omitempty" jsonschema:"maximum tasks to return; defaults to 50"`
}

// ListTasksOutput is the output of aidev_list_tasks.
type ListTasksOutput struct {
	Tasks []view.Task `json:"tasks" jsonschema:"matching tasks, newest first"`
	Count int         `json:"count" jsonschema:"how many were returned"`
}

func (s *Server) listTasks(ctx context.Context, _ *sdk.CallToolRequest, in ListTasksInput) (*sdk.CallToolResult, ListTasksOutput, error) {
	filter := store.TaskFilter{Limit: in.Limit}

	// An unknown status is the caller's mistake, and naming it is more use than a
	// connection error: checked before connecting.
	for _, raw := range in.Statuses {
		trimmed := strings.ToUpper(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		parsed, err := task.ParseStatus(trimmed)
		if err != nil {
			return nil, ListTasksOutput{}, err
		}
		filter.Statuses = append(filter.Statuses, parsed)
	}

	orchestrator, st, err := s.connected(ctx)
	if err != nil {
		return nil, ListTasksOutput{}, err
	}

	if strings.TrimSpace(in.RepoPath) != "" {
		repo, err := orchestrator.Git.OpenRepository(ctx, in.RepoPath)
		if err != nil {
			return nil, ListTasksOutput{}, err
		}
		project, err := st.GetProjectByPath(ctx, repo.Path)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// A repository with no tasks yet is an empty result, not an error.
				return nil, ListTasksOutput{Tasks: []view.Task{}}, nil
			}
			return nil, ListTasksOutput{}, err
		}
		filter.ProjectID = project.ID
	}

	tasks, err := st.ListTasks(ctx, filter)
	if err != nil {
		return nil, ListTasksOutput{}, err
	}
	views := make([]view.Task, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, view.NewTask(t))
	}
	return nil, ListTasksOutput{Tasks: views, Count: len(views)}, nil
}

// RunTaskInput is the input of aidev_run_task.
type RunTaskInput struct {
	Task string `json:"task" jsonschema:"task reference such as TASK-000001, or the task's UUID"`

	// A pointer so that 0 ("start it and return immediately") is distinguishable
	// from the field being absent ("wait for the default").
	WaitSeconds *int `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the run to finish before returning; defaults to 120, maximum 900, and 0 starts the run and returns at once. The run continues regardless"`
}

// RunTaskOutput is the output of aidev_run_task.
type RunTaskOutput struct {
	Result view.Result `json:"result" jsonschema:"what is known so far; the verification section is what decides success"`

	StillRunning bool   `json:"still_running" jsonschema:"true if the wait elapsed before the run finished; the run continues in the background"`
	Succeeded    bool   `json:"succeeded" jsonschema:"true only if the task reached SUCCEEDED, which requires aidev's verification to have passed"`
	NextStep     string `json:"next_step" jsonschema:"what to do next"`
}

func (s *Server) runTask(ctx context.Context, _ *sdk.CallToolRequest, in RunTaskInput) (*sdk.CallToolResult, RunTaskOutput, error) {
	wait := DefaultWaitSeconds
	if in.WaitSeconds != nil {
		wait = *in.WaitSeconds
	}
	switch {
	case wait < 0:
		// Checked before connecting: no database can make a negative wait valid.
		return nil, RunTaskOutput{}, fmt.Errorf("wait_seconds must not be negative")
	case wait > MaxWaitSeconds:
		wait = MaxWaitSeconds
	}

	orchestrator, st, err := s.connected(ctx)
	if err != nil {
		return nil, RunTaskOutput{}, err
	}
	t, err := s.resolve(ctx, st, in.Task)
	if err != nil {
		return nil, RunTaskOutput{}, err
	}

	run, started, err := s.startRun(orchestrator, st, t)
	if err != nil {
		return nil, RunTaskOutput{}, err
	}
	if started {
		s.logger.InfoContext(ctx, "task run started from mcp",
			"task_ref", t.Ref, "wait_seconds", wait)
	}

	select {
	case <-run.done:
		return s.finishedRunOutput(ctx, orchestrator, st, t.ID, run)

	case <-time.After(time.Duration(wait) * time.Second):
		// Still going. Report where it has got to rather than an error: a long
		// task is the normal case, not a failure.
		result, err := s.buildResult(ctx, st, t.ID, false)
		if err != nil {
			return nil, RunTaskOutput{}, err
		}
		return nil, RunTaskOutput{
			Result:       result,
			StillRunning: true,
			NextStep: fmt.Sprintf(
				"the run is still going after %ds; call %s to see how it ended, or %s to follow progress",
				wait, ToolGetResult, ToolGetEvents),
		}, nil

	case <-ctx.Done():
		// The client gave up on this call. The run is unaffected: it is a
		// separate process that outlives this server (research C2), or, in
		// tests, bound to the server's context rather than this one.
		return nil, RunTaskOutput{}, ctx.Err()
	}
}

// finishedRunOutput builds the output for a run that has completed.
func (s *Server) finishedRunOutput(ctx context.Context, orchestrator *worker.Orchestrator, st *store.Store, taskID uuid.UUID, run *backgroundRun) (*sdk.CallToolResult, RunTaskOutput, error) {
	if run.err != nil {
		return nil, RunTaskOutput{}, run.err
	}

	result, err := s.buildResult(ctx, st, taskID, false)
	if err != nil {
		return nil, RunTaskOutput{}, err
	}

	// An approval gate is reported as a result, not an error: the planner
	// needs to know a human is required, which is not a malfunction. The
	// recorded status decides, because the run that hit the gate exited
	// cleanly — both as a child process and as an orchestrator call.
	if result.Task.Status == task.StatusWaitingApproval.String() {
		return nil, RunTaskOutput{
			Result:   result,
			NextStep: approvalNextStep(orchestrator.Config, result.Task.Ref),
		}, nil
	}

	succeeded := result.Task.Status == task.StatusSucceeded.String()
	next := fmt.Sprintf("verification failed or the agent did not finish; call %s with include_logs to see the output", ToolGetResult)
	if succeeded {
		branch := ""
		if result.Worktree != nil {
			branch = result.Worktree.Branch
		}
		next = fmt.Sprintf("the work is committed on branch %s; review it with git before merging anything", branch)
	}

	return nil, RunTaskOutput{
		Result:    result,
		Succeeded: succeeded,
		NextStep:  next,
	}, nil
}

// GetResultInput is the input of aidev_get_task_result.
type GetResultInput struct {
	Task        string   `json:"task" jsonschema:"task reference such as TASK-000001, or the task's UUID"`
	IncludeLogs bool     `json:"include_logs,omitempty" jsonschema:"include the agent's transcript, its error output, the collected diff and the complete verification output, each cut to max_bytes. Ask for them when diagnosing a failure"`
	Sections    []string `json:"sections,omitempty" jsonschema:"which logs to return: transcript (the agent's events as plain text), stdout (its raw event stream), stderr, diff, verification. Defaults to all but stdout; naming any implies include_logs"`
	MaxBytes    int      `json:"max_bytes,omitempty" jsonschema:"the most bytes returned per section (per step for verification output); defaults to 65536, at most 1048576"`
	Offset      int      `json:"offset,omitempty" jsonschema:"byte offset each section starts at; pass next_offset from a previous call to read the next page"`
}

// GetResultOutput is the output of aidev_get_task_result.
type GetResultOutput struct {
	Result view.Result `json:"result" jsonschema:"everything known about the task's latest attempt"`

	AgentTranscript string `json:"agent_transcript,omitempty" jsonschema:"the agent's events as plain text, one line per message, tool call or error; only with include_logs"`
	AgentStdout     string `json:"agent_stdout,omitempty" jsonschema:"the agent's raw event stream; only when the stdout section is asked for"`
	AgentStderr     string `json:"agent_stderr,omitempty" jsonschema:"the agent's error output; only with include_logs"`
	Diff            string `json:"diff,omitempty" jsonschema:"the change aidev collected from git; only with include_logs"`

	// The whole size of each returned section, so a cut section is
	// distinguishable from a short one (research D1).
	AgentTranscriptTotalBytes int `json:"agent_transcript_total_bytes,omitempty" jsonschema:"size of the whole transcript"`
	AgentStdoutTotalBytes     int `json:"agent_stdout_total_bytes,omitempty" jsonschema:"size of the whole raw event stream"`
	AgentStderrTotalBytes     int `json:"agent_stderr_total_bytes,omitempty" jsonschema:"size of the whole error output"`
	DiffTotalBytes            int `json:"diff_total_bytes,omitempty" jsonschema:"size of the whole diff"`
	NextOffset                int `json:"next_offset,omitempty" jsonschema:"set when some section has more past this page: pass it back as offset to read on"`

	StillRunning bool `json:"still_running" jsonschema:"true if the task is currently executing"`
}

func (s *Server) getResult(ctx context.Context, _ *sdk.CallToolRequest, in GetResultInput) (*sdk.CallToolResult, GetResultOutput, error) {
	_, st, err := s.connected(ctx)
	if err != nil {
		return nil, GetResultOutput{}, err
	}
	t, err := s.resolve(ctx, st, in.Task)
	if err != nil {
		return nil, GetResultOutput{}, err
	}

	req, logs, err := parseLogRequest(in.IncludeLogs, in.Sections, in.Offset, in.MaxBytes)
	if err != nil {
		return nil, GetResultOutput{}, err
	}

	result, err := s.buildResult(ctx, st, t.ID, logs && req.wants(sectionVerification))
	if err != nil {
		return nil, GetResultOutput{}, err
	}
	out := GetResultOutput{Result: result, StillRunning: t.Status.Active()}
	if !logs {
		return nil, out, nil
	}

	p := &pager{req: req}
	if req.wants(sectionVerification) {
		for i := range out.Result.Verification {
			v := &out.Result.Verification[i]
			v.Stdout, v.StdoutTotalBytes = p.page(v.Stdout)
			v.Stderr, v.StderrTotalBytes = p.page(v.Stderr)
		}
	}
	if attempt, err := st.LatestAttempt(ctx, t.ID); err == nil {
		if runs, err := st.ListWorkerRuns(ctx, attempt.ID); err == nil && len(runs) > 0 {
			latest := runs[len(runs)-1]
			if req.wants(sectionTranscript) {
				transcript, _ := agent.CondenseTranscript(latest.Backend, latest.Stdout)
				out.AgentTranscript, out.AgentTranscriptTotalBytes = p.page(transcript)
			}
			if req.wants(sectionStdout) {
				out.AgentStdout, out.AgentStdoutTotalBytes = p.page(latest.Stdout)
			}
			if req.wants(sectionStderr) {
				out.AgentStderr, out.AgentStderrTotalBytes = p.page(latest.Stderr)
			}
			if req.wants(sectionDiff) {
				out.Diff, out.DiffTotalBytes = p.page(latest.Diff)
			}
		}
	}
	out.NextOffset = p.nextOffset()
	return nil, out, nil
}

// GetEventsInput is the input of aidev_get_task_events.
type GetEventsInput struct {
	Task     string `json:"task" jsonschema:"task reference such as TASK-000001, or the task's UUID"`
	AfterSeq int64  `json:"after_seq,omitempty" jsonschema:"return only events after this sequence number; pass the highest seq you have already seen to follow along"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum events to return; defaults to 200"`
}

// GetEventsOutput is the output of aidev_get_task_events.
type GetEventsOutput struct {
	Events  []view.Event `json:"events" jsonschema:"the task's history in order"`
	Count   int          `json:"count" jsonschema:"how many were returned"`
	LastSeq int64        `json:"last_seq" jsonschema:"the highest sequence number returned; pass it back as after_seq to continue"`
}

func (s *Server) getEvents(ctx context.Context, _ *sdk.CallToolRequest, in GetEventsInput) (*sdk.CallToolResult, GetEventsOutput, error) {
	_, st, err := s.connected(ctx)
	if err != nil {
		return nil, GetEventsOutput{}, err
	}
	t, err := s.resolve(ctx, st, in.Task)
	if err != nil {
		return nil, GetEventsOutput{}, err
	}

	events, err := st.ListEvents(ctx, store.EventFilter{
		TaskID:   t.ID,
		AfterSeq: in.AfterSeq,
		Limit:    in.Limit,
	})
	if err != nil {
		return nil, GetEventsOutput{}, err
	}

	out := GetEventsOutput{Events: make([]view.Event, 0, len(events))}
	for _, e := range events {
		out.Events = append(out.Events, view.NewEvent(e, true))
		out.LastSeq = e.Seq
	}
	out.Count = len(out.Events)
	return nil, out, nil
}

// CancelTaskInput is the input of aidev_cancel_task.
type CancelTaskInput struct {
	Task   string `json:"task" jsonschema:"task reference such as TASK-000001, or the task's UUID"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is being cancelled; recorded in the task's history"`
}

// CancelTaskOutput is the output of aidev_cancel_task.
type CancelTaskOutput struct {
	Task    view.Task `json:"task" jsonschema:"the cancelled task"`
	Message string    `json:"message" jsonschema:"what happened, including whether a worktree was kept"`
}

func (s *Server) cancelTask(ctx context.Context, _ *sdk.CallToolRequest, in CancelTaskInput) (*sdk.CallToolResult, CancelTaskOutput, error) {
	orchestrator, _, err := s.connected(ctx)
	if err != nil {
		return nil, CancelTaskOutput{}, err
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "cancelled through MCP"
	}
	outcome, err := orchestrator.Cancel(ctx, in.Task, reason)
	if err != nil {
		return nil, CancelTaskOutput{}, err
	}
	return nil, CancelTaskOutput{Task: view.NewTask(outcome.Task), Message: outcome.Message}, nil
}

// ApproveTaskInput is the input of aidev_approve_task.
type ApproveTaskInput struct {
	Task string `json:"task" jsonschema:"task reference such as TASK-000001, or the task's UUID"`

	// Required rather than defaulted: approving is a decision with consequences,
	// and it must not happen because a field was omitted.
	Approve bool `json:"approve" jsonschema:"true to grant approval, false to deny it and fail the task"`

	DecidedBy string `json:"decided_by,omitempty" jsonschema:"who made the decision; recorded in the task's history"`
	Reason    string `json:"reason,omitempty" jsonschema:"why"`
}

// ApproveTaskOutput is the output of aidev_approve_task.
type ApproveTaskOutput struct {
	Task     view.Task      `json:"task" jsonschema:"the task after the decision"`
	Approval *view.Approval `json:"approval,omitempty" jsonschema:"the recorded decision"`
	Message  string         `json:"message" jsonschema:"what happened"`
}

func (s *Server) approveTask(ctx context.Context, _ *sdk.CallToolRequest, in ApproveTaskInput) (*sdk.CallToolResult, ApproveTaskOutput, error) {
	orchestrator, _, err := s.connected(ctx)
	if err != nil {
		return nil, ApproveTaskOutput{}, err
	}
	// mcp.allow_approval is off by default: the MCP client may be the planner
	// that created this task, and a party must not wave through (or fail) its
	// own work. The operator's path is the CLI, and both approve and deny are
	// blocked here so a planner cannot fail someone else's task either.
	if !orchestrator.Config.MCPAllowApproval {
		return nil, ApproveTaskOutput{}, fmt.Errorf(
			"approvals over MCP are disabled (set mcp.allow_approval to allow them); decide with the CLI instead: aidev task approve %s --by <name>%s",
			in.Task, map[bool]string{true: " --deny", false: ""}[in.Approve])
	}
	decidedBy := strings.TrimSpace(in.DecidedBy)
	if decidedBy == "" {
		decidedBy = "mcp client"
	}
	outcome, err := orchestrator.Approve(ctx, in.Task, in.Approve, decidedBy, in.Reason, "mcp")
	if err != nil {
		return nil, ApproveTaskOutput{}, err
	}
	out := ApproveTaskOutput{Task: view.NewTask(outcome.Task), Message: outcome.Message}
	if outcome.Approval != nil {
		out.Approval = view.NewApproval(*outcome.Approval)
	}
	return nil, out, nil
}

// resolve looks a task up by reference or id.
func (s *Server) resolve(ctx context.Context, st *store.Store, identifier string) (task.Task, error) {
	t, err := st.ResolveTask(ctx, identifier)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return task.Task{}, fmt.Errorf("no task %q; list tasks with %s", identifier, ToolListTasks)
		}
		return task.Task{}, err
	}
	return t, nil
}

// buildResult assembles the current state of a task from the database, so that a
// poll after a background run sees the same thing a fresh process would.
func (s *Server) buildResult(ctx context.Context, st *store.Store, taskID uuid.UUID, includeOutput bool) (view.Result, error) {
	t, err := st.GetTask(ctx, taskID)
	if err != nil {
		return view.Result{}, err
	}

	var (
		attempt       *task.TaskAttempt
		workerRun     *task.WorkerRun
		worktree      *task.Worktree
		approval      *task.Approval
		runs          []task.VerificationRun
		testsModified []string
	)

	latest, err := st.LatestAttempt(ctx, taskID)
	switch {
	case err == nil:
		attempt = &latest
		runs, _ = st.ListVerificationRuns(ctx, latest.ID)
		if workerRuns, err := st.ListWorkerRuns(ctx, latest.ID); err == nil && len(workerRuns) > 0 {
			workerRun = &workerRuns[len(workerRuns)-1]
		}
		if wt, err := st.GetWorktreeByAttempt(ctx, latest.ID); err == nil {
			worktree = &wt
		}
		// Best-effort, like the approval read below: the report is a courtesy
		// to the reader and must not decide whether the result can be shown.
		if tests, err := st.TestsModifiedPaths(ctx, taskID, latest.ID); err == nil {
			testsModified = tests
		}
	case errors.Is(err, store.ErrNotFound):
		// Never run: the absent sections say so.
	default:
		return view.Result{}, err
	}

	if a, err := st.LatestApproval(ctx, taskID); err == nil {
		approval = &a
	}

	message := ""
	if attempt != nil && attempt.Error != "" {
		message = attempt.Error
	}
	return view.NewResult(t, attempt, workerRun, runs, worktree, approval, testsModified, message, includeOutput), nil
}

// startRun begins a background execution, or joins one already in flight.
//
// Joining rather than starting a second is what makes a repeated aidev_run_task
// call harmless: the orchestrator would reject the second anyway, but reporting
// the progress of the first is more useful than an error.
//
// A task that is already terminal or already being driven somewhere else is
// refused here, with the orchestrator's own message, before any process is
// spawned for it — spawning one just to watch it refuse would be wasteful, and
// the refusal is the answer either way.
func (s *Server) startRun(orchestrator *worker.Orchestrator, st *store.Store, t task.Task) (*backgroundRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.runs[t.ID]; ok {
		select {
		case <-existing.done:
			// Finished; a new call may start a fresh run.
			delete(s.runs, t.ID)
		default:
			return existing, false, nil
		}
	}
	if t.Status.Terminal() || t.Status.Active() {
		return nil, false, fmt.Errorf("%s is already %s: %w", t.Identifier(), t.Status, worker.ErrNotRunnable)
	}

	// Started under the lock so two concurrent calls for the same task cannot
	// both get past the checks above and race to claim it.
	wait, logPath, err := s.launch(s.baseCtx, orchestrator, t)
	if err != nil {
		return nil, false, err
	}

	run := &backgroundRun{done: make(chan struct{})}
	s.runs[t.ID] = run

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(run.done)

		// The watcher is bound to the server's context, not a tool call's: the
		// call may return with still_running, or its client go away, long
		// before the run ends. The kept orchestrator is used so a run started
		// after a deferred connect records against the same connection.
		run.err = s.awaitOutcome(s.baseCtx, st, t.ID, wait, logPath)
	}()
	return run, true, nil
}
