package worker

import (
	"context"

	"aidev/internal/task"
)

// ResolveApply completes a task's apply state with what git knows. The event
// log only sees `aidev task apply`; a SUCCEEDED task never applied through
// aidev whose work is already on the repository's default branch — merged,
// cherry-picked or rebased by hand — is ApplyLanded, into that branch.
//
// Any other state is returned unchanged: "applied" and "undone" are facts the
// log recorded, and say more. A task whose repository or worktree record
// cannot be read is left as it was; the git question is a courtesy, and a
// listing must not fail over it.
func (o *Orchestrator) ResolveApply(ctx context.Context, t task.Task, apply task.Apply) task.Apply {
	if t.Status != task.StatusSucceeded || apply.State != task.ApplyNever {
		return apply
	}
	attempt, err := o.Store.LatestAttempt(ctx, t.ID)
	if err != nil {
		return apply
	}
	wt, err := o.Store.GetWorktreeByAttempt(ctx, attempt.ID)
	if err != nil || wt.HeadCommit == "" || wt.HeadCommit == wt.BaseCommit {
		return apply
	}
	project, err := o.Store.GetProject(ctx, t.ProjectID)
	if err != nil || project.DefaultBranch == "" {
		return apply
	}
	repo, err := o.Git.OpenRepository(ctx, project.RepoPath)
	if err != nil {
		return apply
	}
	landed, err := o.Git.Landed(ctx, repo, project.DefaultBranch, wt.BaseCommit, wt.HeadCommit)
	if err != nil || !landed {
		return apply
	}
	return task.Apply{State: task.ApplyLanded, Into: project.DefaultBranch}
}
