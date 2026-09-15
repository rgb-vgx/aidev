package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"aidev/internal/task"
	"aidev/internal/worker"
)

// writeTaskLine prints one task as a single line, for listings.
func writeTaskLine(w io.Writer, t task.Task) {
	fmt.Fprintf(w, "%-12s  %-16s  %-6s  %s\n", t.Ref, t.Status, t.Agent, truncate(t.Title, 60))
}

// writeTaskDetail prints a task for a human.
func writeTaskDetail(w io.Writer, t task.Task) {
	fmt.Fprintf(w, "%s  %s\n", t.Ref, t.Title)
	fmt.Fprintf(w, "  status       %s\n", t.Status)
	fmt.Fprintf(w, "  agent        %s\n", t.Agent)
	if t.Model != "" {
		fmt.Fprintf(w, "  model        %s\n", t.Model)
	}
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
