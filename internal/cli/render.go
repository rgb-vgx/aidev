package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"aidev/internal/event"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// taskView is the JSON shape of a task.
//
// It is a separate type from task.Task on purpose: the JSON is an interface that
// scripts and the MCP layer depend on, and deriving it from the domain struct
// would mean every internal rename silently became a breaking change.
type taskView struct {
	Ref                string   `json:"ref"`
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	Title              string   `json:"title"`
	Description        string   `json:"description,omitempty"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
	Agent              string   `json:"agent"`
	Priority           int      `json:"priority"`
	Verification       []string `json:"verification"`
	RequiresApproval   bool     `json:"requires_approval"`
	MaxRetries         int      `json:"max_retries"`
	BaseRef            string   `json:"base_ref,omitempty"`
	TimeoutSeconds     int      `json:"timeout_seconds,omitempty"`
	ProjectID          string   `json:"project_id"`
	CreatedAt          string   `json:"created_at"`
	UpdatedAt          string   `json:"updated_at"`
}

func newTaskView(t task.Task) taskView {
	commands := make([]string, 0, len(t.Verification))
	for _, step := range t.Verification {
		commands = append(commands, step.String())
	}
	return taskView{
		Ref:                t.Ref,
		ID:                 t.ID.String(),
		Status:             t.Status.String(),
		Title:              t.Title,
		Description:        t.Description,
		AcceptanceCriteria: t.AcceptanceCriteria,
		Agent:              t.Agent,
		Priority:           t.Priority,
		Verification:       commands,
		RequiresApproval:   t.RequiresApproval,
		MaxRetries:         t.MaxRetries,
		BaseRef:            t.BaseRef,
		TimeoutSeconds:     int(t.Timeout.Seconds()),
		ProjectID:          t.ProjectID.String(),
		CreatedAt:          t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:          t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// resultView is the JSON shape of a task's outcome: what a planner reads to decide
// what to do next.
type resultView struct {
	Task         taskView           `json:"task"`
	Attempt      *attemptView       `json:"attempt,omitempty"`
	Worker       *workerView        `json:"worker,omitempty"`
	Verification []verificationView `json:"verification,omitempty"`
	Worktree     *worktreeView      `json:"worktree,omitempty"`
	Approval     *approvalView      `json:"approval,omitempty"`
	Message      string             `json:"message,omitempty"`
}

type attemptView struct {
	ID          string `json:"id"`
	Number      int    `json:"number"`
	Status      string `json:"status"`
	FailureKind string `json:"failure_kind,omitempty"`
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
}

type workerView struct {
	Backend      string `json:"backend"`
	Status       string `json:"status"`
	FailureKind  string `json:"failure_kind,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Summary      string `json:"summary,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	ChangedFiles int    `json:"changed_files"`
	DurationMS   int64  `json:"duration_ms"`

	// Stdout and stderr are omitted here and available through `aidev task result
	// --logs`: a result a planner reads should not be dominated by an agent's
	// transcript.
	StdoutTruncated bool `json:"stdout_truncated,omitempty"`
	StderrTruncated bool `json:"stderr_truncated,omitempty"`
}

type verificationView struct {
	Step       int    `json:"step"`
	Command    string `json:"command"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
}

type worktreeView struct {
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	Status     string `json:"status"`
	BaseCommit string `json:"base_commit,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`
}

type approvalView struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`
}

type eventView struct {
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	AttemptID string `json:"attempt_id,omitempty"`
	Payload   any    `json:"payload,omitempty"`
}

func newAttemptView(a task.TaskAttempt) *attemptView {
	v := &attemptView{
		ID:          a.ID.String(),
		Number:      a.AttemptNumber,
		Status:      a.Status.String(),
		FailureKind: a.FailureKind.String(),
		Error:       a.Error,
		StartedAt:   a.StartedAt.UTC().Format(time.RFC3339),
		DurationMS:  a.Duration().Milliseconds(),
	}
	if a.FinishedAt != nil {
		v.FinishedAt = a.FinishedAt.UTC().Format(time.RFC3339)
	}
	return v
}

func newWorkerView(r task.WorkerRun) *workerView {
	return &workerView{
		Backend:         r.Backend,
		Status:          r.Status.String(),
		FailureKind:     r.FailureKind.String(),
		ExitCode:        r.ExitCode,
		SessionID:       r.SessionID,
		Summary:         r.Summary,
		FinishReason:    r.FinishReason,
		ChangedFiles:    r.ChangedFiles,
		DurationMS:      r.Duration().Milliseconds(),
		StdoutTruncated: r.StdoutTruncated,
		StderrTruncated: r.StderrTruncated,
	}
}

func newVerificationViews(runs []task.VerificationRun, includeOutput bool) []verificationView {
	views := make([]verificationView, 0, len(runs))
	for _, r := range runs {
		v := verificationView{
			Step:       r.StepIndex,
			Command:    r.Command,
			Status:     r.Status.String(),
			ExitCode:   r.ExitCode,
			DurationMS: r.Duration.Milliseconds(),
		}
		if includeOutput {
			v.Stdout = r.Stdout
			v.Stderr = r.Stderr
		} else {
			// A failure's output is the first thing anyone wants, so it is
			// included even in the compact form, trimmed to stay readable.
			if r.Status != task.VerificationPassed && r.Status != task.VerificationSkipped {
				v.Stdout = tail(r.Stdout, 2000)
				v.Stderr = tail(r.Stderr, 2000)
			}
		}
		views = append(views, v)
	}
	return views
}

func newWorktreeView(w task.Worktree) *worktreeView {
	return &worktreeView{
		Path:       w.Path,
		Branch:     w.Branch,
		Status:     w.Status.String(),
		BaseCommit: w.BaseCommit,
		HeadCommit: w.HeadCommit,
	}
}

func newApprovalView(a task.Approval) *approvalView {
	return &approvalView{
		ID:        a.ID.String(),
		Status:    a.Status.String(),
		Reason:    a.Reason,
		DecidedBy: a.DecidedBy,
	}
}

func newEventView(e event.Event, includePayload bool) eventView {
	v := eventView{
		Seq:       e.Seq,
		Type:      e.Type.String(),
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if e.AttemptID != nil {
		v.AttemptID = e.AttemptID.String()
	}
	if includePayload && len(e.Payload) > 0 {
		v.Payload = rawJSON(e.Payload)
	}
	return v
}

// rawJSON passes stored JSONB through without re-encoding it.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// writeTaskLine prints one task as a single line, for listings.
func writeTaskLine(w io.Writer, t task.Task) {
	fmt.Fprintf(w, "%-12s  %-16s  %-6s  %s\n", t.Ref, t.Status, t.Agent, truncate(t.Title, 60))
}

// writeTaskDetail prints a task for a human.
func writeTaskDetail(w io.Writer, t task.Task) {
	fmt.Fprintf(w, "%s  %s\n", t.Ref, t.Title)
	fmt.Fprintf(w, "  status       %s\n", t.Status)
	fmt.Fprintf(w, "  agent        %s\n", t.Agent)
	fmt.Fprintf(w, "  priority     %d\n", t.Priority)
	if t.RequiresApproval {
		fmt.Fprintf(w, "  approval     required\n")
	}
	if t.BaseRef != "" {
		fmt.Fprintf(w, "  base ref     %s\n", t.BaseRef)
	}
	if t.Timeout > 0 {
		fmt.Fprintf(w, "  timeout      %s\n", t.Timeout)
	}
	fmt.Fprintf(w, "  id           %s\n", t.ID)
	fmt.Fprintf(w, "  created      %s\n", t.CreatedAt.UTC().Format(time.RFC3339))

	if t.Description != "" {
		fmt.Fprintf(w, "\n  description\n%s\n", indent(t.Description, "    "))
	}
	if t.AcceptanceCriteria != "" {
		fmt.Fprintf(w, "\n  acceptance criteria\n%s\n", indent(t.AcceptanceCriteria, "    "))
	}

	fmt.Fprintf(w, "\n  verification (run by aidev, not by the agent)\n")
	for _, step := range t.Verification {
		fmt.Fprintf(w, "    %s\n", step.String())
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// tail keeps the end of a long output, which is where a failure's explanation
// usually is.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// writeRunOutcome reports what a run did, leading with the decision.
func writeRunOutcome(env *Env, outcome worker.Outcome, runs []task.VerificationRun) {
	w := env.Stdout
	fmt.Fprintf(w, "\n%s\n", outcome.Message)

	if outcome.WorkerRun != nil {
		r := outcome.WorkerRun
		fmt.Fprintf(w, "\nagent (%s)\n", r.Backend)
		fmt.Fprintf(w, "  outcome       %s", r.Status)
		if r.FailureKind != task.FailureNone {
			fmt.Fprintf(w, " (%s)", r.FailureKind)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  changed files %d\n", r.ChangedFiles)
		fmt.Fprintf(w, "  took          %s\n", r.Duration().Round(time.Second))
		if r.Summary != "" {
			// The agent's own account, shown because it helps diagnosis and
			// labelled so nobody mistakes it for evidence.
			fmt.Fprintf(w, "  says          %s\n", truncate(oneLine(r.Summary), 140))
		}
	}

	writeVerificationTable(w, runs)

	if outcome.Worktree != nil {
		fmt.Fprintf(w, "\nworktree\n")
		fmt.Fprintf(w, "  %s  %s\n", outcome.Worktree.Status, outcome.Worktree.Path)
		fmt.Fprintf(w, "  branch  %s\n", outcome.Worktree.Branch)
	}

	writeNextSteps(w, outcome)
}

// writeResult reports a stored outcome.
func writeResult(env *Env, outcome worker.Outcome, runs []task.VerificationRun) {
	w := env.Stdout
	t := outcome.Task
	fmt.Fprintf(w, "%s  %s\n", t.Ref, t.Title)
	fmt.Fprintf(w, "  status  %s\n", t.Status)

	if outcome.Attempt == nil {
		fmt.Fprintf(w, "\nThis task has not run yet. Run it with: aidev task run %s\n", t.Identifier())
		if outcome.Approval != nil && outcome.Approval.Status == task.ApprovalPending {
			fmt.Fprintf(w, "It is waiting for approval: aidev task approve %s\n", t.Identifier())
		}
		return
	}

	a := outcome.Attempt
	fmt.Fprintf(w, "\nattempt %d  %s", a.AttemptNumber, a.Status)
	if a.FailureKind != task.FailureNone {
		fmt.Fprintf(w, " (%s)", a.FailureKind)
	}
	fmt.Fprintf(w, "  took %s\n", a.Duration().Round(time.Second))
	if a.Error != "" {
		fmt.Fprintf(w, "  %s\n", oneLine(a.Error))
	}

	if outcome.WorkerRun != nil {
		r := outcome.WorkerRun
		fmt.Fprintf(w, "\nagent (%s)  %s  %d file(s) changed\n", r.Backend, r.Status, r.ChangedFiles)
		if r.Summary != "" {
			fmt.Fprintf(w, "  says  %s\n", truncate(oneLine(r.Summary), 140))
		}
		if r.SessionID != "" {
			fmt.Fprintf(w, "  session  %s\n", r.SessionID)
		}
	}

	writeVerificationTable(w, runs)

	if outcome.Worktree != nil {
		fmt.Fprintf(w, "\nworktree  %s  %s\n", outcome.Worktree.Status, outcome.Worktree.Path)
		fmt.Fprintf(w, "  branch  %s", outcome.Worktree.Branch)
		if outcome.Worktree.HeadCommit != "" {
			fmt.Fprintf(w, " @ %s", shortCommit(outcome.Worktree.HeadCommit))
		}
		fmt.Fprintln(w)
	}

	writeNextSteps(w, outcome)
}

// writeVerificationTable prints the only evidence that decides a task's outcome.
func writeVerificationTable(w io.Writer, runs []task.VerificationRun) {
	if len(runs) == 0 {
		return
	}
	fmt.Fprintf(w, "\nverification (run by aidev)\n")
	for _, r := range runs {
		mark := "·"
		switch r.Status {
		case task.VerificationPassed:
			mark = "✓"
		case task.VerificationSkipped:
			mark = "-"
		default:
			mark = "✗"
		}
		fmt.Fprintf(w, "  %s %-9s %s", mark, r.Status, r.Command)
		if r.ExitCode != nil && r.Status != task.VerificationPassed {
			fmt.Fprintf(w, "  (exit %d)", *r.ExitCode)
		}
		fmt.Fprintln(w)

		// A failure's output is what the reader needs next, so it is shown
		// inline rather than behind another command.
		if r.Status != task.VerificationPassed && r.Status != task.VerificationSkipped {
			if out := strings.TrimSpace(r.Stdout); out != "" {
				fmt.Fprintf(w, "%s\n", indent(tail(out, 1200), "      "))
			}
			if errOut := strings.TrimSpace(r.Stderr); errOut != "" {
				fmt.Fprintf(w, "%s\n", indent(tail(errOut, 1200), "      "))
			}
		}
	}
}

// writeNextSteps tells the reader what they can do now, which differs by outcome.
func writeNextSteps(w io.Writer, outcome worker.Outcome) {
	switch outcome.Task.Status {
	case task.StatusSucceeded:
		if outcome.Worktree != nil {
			fmt.Fprintf(w, "\nreview the work\n")
			fmt.Fprintf(w, "  git log --oneline %s\n", outcome.Worktree.Branch)
			fmt.Fprintf(w, "  git diff %s..%s\n", baseBranchGuess(outcome), outcome.Worktree.Branch)
		}
	case task.StatusFailed, task.StatusCancelled:
		if outcome.Worktree != nil && outcome.Worktree.Status == task.WorktreeRetained {
			fmt.Fprintf(w, "\nthe work was kept for inspection\n  cd %s\n", outcome.Worktree.Path)
		}
		fmt.Fprintf(w, "\nfull output: aidev task result %s --logs\n", outcome.Task.Identifier())
	case task.StatusWaitingApproval:
		fmt.Fprintf(w, "\napprove it: aidev task approve %s\n", outcome.Task.Identifier())
	}
}

// baseBranchGuess names something useful to diff against without claiming to know
// the repository's conventions.
func baseBranchGuess(outcome worker.Outcome) string {
	if outcome.Task.BaseRef != "" {
		return outcome.Task.BaseRef
	}
	return "HEAD"
}

// writeLogs prints the agent transcript and the diff, which are large enough that
// they are only shown on request.
func writeLogs(env *Env, stdout, stderr, diff string) {
	w := env.Stdout
	if strings.TrimSpace(stdout) != "" {
		fmt.Fprintf(w, "\n--- agent stdout ---\n%s\n", stdout)
	}
	if strings.TrimSpace(stderr) != "" {
		fmt.Fprintf(w, "\n--- agent stderr ---\n%s\n", stderr)
	}
	if strings.TrimSpace(diff) != "" {
		fmt.Fprintf(w, "\n--- diff collected by aidev ---\n%s\n", diff)
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func shortCommit(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	return c
}
