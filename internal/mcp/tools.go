package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/view"
	"aidev/internal/worker"
)

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

	Description        string `json:"description,omitempty" jsonschema:"the full instruction for the agent: what to build and where. Be specific about file names and signatures"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty" jsonschema:"what done looks like, in prose"`
	Agent              string `json:"agent,omitempty" jsonschema:"agent to use; defaults to the configured one (build)"`
	Priority           int    `json:"priority,omitempty" jsonschema:"higher runs first; defaults to 0"`
	RequiresApproval   bool   `json:"requires_approval,omitempty" jsonschema:"when true the task will not run until a human approves it"`
	BaseRef            string `json:"base_ref,omitempty" jsonschema:"git ref the task's branch starts from; defaults to the repository's current branch"`
	TimeoutSeconds     int    `json:"timeout_seconds,omitempty" jsonschema:"bound this task's agent run; defaults to the configured timeout"`
}

// CreateTaskOutput is the output of aidev_create_task.
type CreateTaskOutput struct {
	Task     view.Task `json:"task" jsonschema:"the created task, in status PENDING"`
	NextStep string    `json:"next_step" jsonschema:"what to do next"`
}

func (s *Server) createTask(ctx context.Context, _ *sdk.CallToolRequest, in CreateTaskInput) (*sdk.CallToolResult, CreateTaskOutput, error) {
	orchestrator, _, err := s.connected(ctx)
	if err != nil {
		return nil, CreateTaskOutput{}, err
	}

	steps, err := task.ParseVerificationSteps(in.Verification)
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
		RequiresApproval:   in.RequiresApproval,
		BaseRef:            in.BaseRef,
		Timeout:            time.Duration(in.TimeoutSeconds) * time.Second,
	})
	if err != nil {
		return nil, CreateTaskOutput{}, err
	}

	next := fmt.Sprintf("run it with %s", ToolRunTask)
	if created.RequiresApproval {
		next = fmt.Sprintf("this task requires approval; a human must call %s before it can run", ToolApprove)
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
	orchestrator, st, err := s.connected(ctx)
	if err != nil {
		return nil, ListTasksOutput{}, err
	}
	filter := store.TaskFilter{Limit: in.Limit}

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
	orchestrator, st, err := s.connected(ctx)
	if err != nil {
		return nil, RunTaskOutput{}, err
	}
	t, err := s.resolve(ctx, st, in.Task)
	if err != nil {
		return nil, RunTaskOutput{}, err
	}

	wait := DefaultWaitSeconds
	if in.WaitSeconds != nil {
		wait = *in.WaitSeconds
	}
	switch {
	case wait < 0:
		return nil, RunTaskOutput{}, fmt.Errorf("wait_seconds must not be negative")
	case wait > MaxWaitSeconds:
		wait = MaxWaitSeconds
	}

	run, started := s.startRun(orchestrator, t.ID)
	if started {
		s.logger.InfoContext(ctx, "task run started from mcp",
			"task_ref", t.Ref, "wait_seconds", wait)
	}

	select {
	case <-run.done:
		return s.finishedRunOutput(ctx, st, t.ID, run)

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
		// The client gave up on this call. The run itself is unaffected, because
		// it is bound to the server's context rather than this one.
		return nil, RunTaskOutput{}, ctx.Err()
	}
}

// finishedRunOutput builds the output for a run that has completed.
func (s *Server) finishedRunOutput(ctx context.Context, st *store.Store, taskID uuid.UUID, run *backgroundRun) (*sdk.CallToolResult, RunTaskOutput, error) {
	if run.err != nil {
		// An approval gate is reported as a result, not an error: the planner
		// needs to know a human is required, which is not a malfunction.
		if errors.Is(run.err, worker.ErrApprovalRequired) {
			result, err := s.buildResult(ctx, st, taskID, false)
			if err != nil {
				return nil, RunTaskOutput{}, err
			}
			return nil, RunTaskOutput{
				Result: result,
				NextStep: fmt.Sprintf("this task requires approval; a human must call %s before it will run",
					ToolApprove),
			}, nil
		}
		return nil, RunTaskOutput{}, run.err
	}

	result, err := s.buildResult(ctx, st, taskID, false)
	if err != nil {
		return nil, RunTaskOutput{}, err
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
	Task        string `json:"task" jsonschema:"task reference such as TASK-000001, or the task's UUID"`
	IncludeLogs bool   `json:"include_logs,omitempty" jsonschema:"include the agent's full transcript, the collected diff, and the complete verification output. These can be large; ask for them when diagnosing a failure"`
}

// GetResultOutput is the output of aidev_get_task_result.
type GetResultOutput struct {
	Result view.Result `json:"result" jsonschema:"everything known about the task's latest attempt"`

	AgentStdout string `json:"agent_stdout,omitempty" jsonschema:"the agent's raw event stream, only when include_logs is set"`
	AgentStderr string `json:"agent_stderr,omitempty" jsonschema:"the agent's error output, only when include_logs is set"`
	Diff        string `json:"diff,omitempty" jsonschema:"the change aidev collected from git, only when include_logs is set"`

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

	result, err := s.buildResult(ctx, st, t.ID, in.IncludeLogs)
	if err != nil {
		return nil, GetResultOutput{}, err
	}
	out := GetResultOutput{Result: result, StillRunning: t.Status.Active()}

	if in.IncludeLogs {
		if attempt, err := st.LatestAttempt(ctx, t.ID); err == nil {
			if runs, err := st.ListWorkerRuns(ctx, attempt.ID); err == nil && len(runs) > 0 {
				latest := runs[len(runs)-1]
				out.AgentStdout = latest.Stdout
				out.AgentStderr = latest.Stderr
				out.Diff = latest.Diff
			}
		}
	}
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
	decidedBy := strings.TrimSpace(in.DecidedBy)
	if decidedBy == "" {
		decidedBy = "mcp client"
	}
	outcome, err := orchestrator.Approve(ctx, in.Task, in.Approve, decidedBy, in.Reason)
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
		attempt   *task.TaskAttempt
		workerRun *task.WorkerRun
		worktree  *task.Worktree
		approval  *task.Approval
		runs      []task.VerificationRun
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
	return view.NewResult(t, attempt, workerRun, runs, worktree, approval, message, includeOutput), nil
}

// startRun begins a background execution, or joins one already in flight.
//
// Joining rather than starting a second is what makes a repeated aidev_run_task
// call harmless: the orchestrator would reject the second anyway, but reporting
// the progress of the first is more useful than an error.
func (s *Server) startRun(orchestrator *worker.Orchestrator, taskID uuid.UUID) (*backgroundRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.runs[taskID]; ok {
		select {
		case <-existing.done:
			// Finished; a new call may start a fresh run.
			delete(s.runs, taskID)
		default:
			return existing, false
		}
	}

	run := &backgroundRun{done: make(chan struct{})}
	s.runs[taskID] = run

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(run.done)

		// Bound to the server's context, not a tool call's: the run must outlive
		// the call that started it, and must stop when the server stops. The
		// kept orchestrator is used so a run started after a deferred connect
		// records against the same connection.
		outcome, err := orchestrator.RunTask(s.baseCtx, taskID.String())
		run.outcome = outcome
		run.err = err
	}()
	return run, true
}
