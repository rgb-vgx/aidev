package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"aidev/internal/event"
	"aidev/internal/git"
	"aidev/internal/store"
	"aidev/internal/task"
)

// Applying a task's result to the branch a person works on — the last step
// of delegating, and the one aidev never takes on its own (AGENTS.md: no
// automatic merge). `aidev task apply` merges the task's branch into the
// checked-out branch of its repository; `aidev task undo` reverts that merge
// with a new commit, which is safe even after a push. Both are recorded as
// events and leave the task's status alone.

// ApplyResult says what Apply or Undo did.
type ApplyResult struct {
	Task task.Task
	// Into is the branch of the person's checkout the change went into or
	// came out of.
	Into string
	// Branch is the task's branch that was applied.
	Branch string
	// Commit is the merge commit (or, for a re-apply, the commit undoing the
	// earlier revert); for Undo, the revert commit.
	Commit string
	// Reapplied is set when Apply undid an earlier Undo rather than merging.
	Reapplied bool
	Message   string
}

// applyRecord is the payload of a task.applied event, and what Undo needs.
type applyRecord struct {
	Into      string `json:"into"`
	Branch    string `json:"branch"`
	Commit    string `json:"commit"`
	Reapplied bool   `json:"reapplied,omitempty"`
}

// undoRecord is the payload of a task.apply_undone event.
type undoRecord struct {
	Into   string `json:"into"`
	Commit string `json:"commit"`
	Undid  string `json:"undid"`
}

// applyState reads the last apply or undo from the task's history.
func applyState(events []event.Event) (applied *applyRecord, undone *undoRecord, err error) {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case event.TypeTaskApplied:
			var rec applyRecord
			if err := json.Unmarshal(events[i].Payload, &rec); err != nil {
				return nil, nil, fmt.Errorf("read the task.applied event: %w", err)
			}
			return &rec, nil, nil
		case event.TypeTaskApplyUndone:
			var rec undoRecord
			if err := json.Unmarshal(events[i].Payload, &rec); err != nil {
				return nil, nil, fmt.Errorf("read the task.apply_undone event: %w", err)
			}
			return nil, &rec, nil
		}
	}
	return nil, nil, nil
}

// applyContext is what both directions resolve first.
type applyContext struct {
	task   task.Task
	repo   git.Repository
	wt     task.Worktree
	into   string
	events []event.Event
}

// prepareApply loads the task, its delivered branch and the person's
// checkout, and refuses what neither direction can work with.
func (o *Orchestrator) prepareApply(ctx context.Context, tx *store.Store, idOrRef string) (applyContext, error) {
	t, err := tx.ResolveTask(ctx, idOrRef)
	if err != nil {
		return applyContext{}, err
	}
	if err := tx.LockTask(ctx, t.ID); err != nil {
		return applyContext{}, err
	}
	if t.Status != task.StatusSucceeded {
		return applyContext{}, fmt.Errorf("%s is %s; only a SUCCEEDED task has a verified result to apply", t.Identifier(), t.Status)
	}
	attempt, err := tx.LatestAttempt(ctx, t.ID)
	if err != nil {
		return applyContext{}, err
	}
	wt, err := tx.GetWorktreeByAttempt(ctx, attempt.ID)
	if err != nil {
		return applyContext{}, err
	}
	if wt.HeadCommit == "" {
		return applyContext{}, fmt.Errorf("%s succeeded without changing anything; there is nothing to apply", t.Identifier())
	}
	project, err := tx.GetProject(ctx, t.ProjectID)
	if err != nil {
		return applyContext{}, err
	}
	repo, err := o.Git.OpenRepository(ctx, project.RepoPath)
	if err != nil {
		return applyContext{}, err
	}
	into, err := o.Git.CheckedOutBranch(ctx, repo)
	if err != nil {
		return applyContext{}, err
	}
	changed, err := o.Git.TrackedChanges(ctx, repo)
	if err != nil {
		return applyContext{}, err
	}
	if len(changed) > 0 {
		return applyContext{}, fmt.Errorf("%s has uncommitted changes (%s); commit or stash them first, so applying cannot mix with them",
			repo.Path, strings.Join(changed, ", "))
	}
	events, err := tx.ListEvents(ctx, store.EventFilter{TaskID: t.ID})
	if err != nil {
		return applyContext{}, err
	}
	return applyContext{task: t, repo: repo, wt: wt, into: into, events: events}, nil
}

// Apply merges a SUCCEEDED task's branch into the checked-out branch of its
// repository with a merge commit. After an Undo it re-applies by reverting
// the revert — merging a branch whose merge was reverted would silently do
// nothing. A conflict is aborted and reported; the checkout is left as it
// was.
func (o *Orchestrator) Apply(ctx context.Context, idOrRef string) (ApplyResult, error) {
	var out ApplyResult
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*git.DefaultTimeout+persistTimeout)
	defer cancel()
	err := o.Store.InTx(writeCtx, func(tx *store.Store) error {
		ac, err := o.prepareApply(writeCtx, tx, idOrRef)
		if err != nil {
			return err
		}
		applied, undone, err := applyState(ac.events)
		if err != nil {
			return err
		}
		if applied != nil {
			return fmt.Errorf("%s is already applied to %s (%s); undo it first with aidev task undo %s",
				ac.task.Identifier(), applied.Into, shortCommit(applied.Commit), ac.task.Identifier())
		}

		before, err := o.Git.ResolveCommit(writeCtx, ac.repo, "HEAD")
		if err != nil {
			return err
		}
		rec := applyRecord{Into: ac.into, Branch: ac.wt.Branch}
		if undone != nil && undone.Into == ac.into {
			rec.Reapplied = true
			rec.Commit, err = o.Git.Revert(writeCtx, ac.repo, undone.Commit, 0)
		} else {
			message := fmt.Sprintf("aidev: apply %s %s\n\nMerges %s, verified by aidev.", ac.task.Identifier(), ac.task.Title, ac.wt.Branch)
			rec.Commit, err = o.Git.MergeNoFF(writeCtx, ac.repo, ac.wt.HeadCommit, message)
		}
		if err != nil {
			return err
		}
		if rec.Commit == before {
			return fmt.Errorf("%s already contains %s's commits, so there was nothing to apply", ac.into, ac.task.Identifier())
		}
		if err := appendEvent(writeCtx, tx, ac.task.ID, nil, event.TypeTaskApplied, rec); err != nil {
			return fmt.Errorf("%s was applied as %s but the record could not be written: %w", ac.task.Identifier(), shortCommit(rec.Commit), err)
		}
		verb := "merged into"
		if rec.Reapplied {
			verb = "re-applied to"
		}
		out = ApplyResult{Task: ac.task, Into: ac.into, Branch: ac.wt.Branch, Commit: rec.Commit, Reapplied: rec.Reapplied,
			Message: fmt.Sprintf("%s %s %s as %s; undo with aidev task undo %s",
				ac.task.Identifier(), verb, ac.into, shortCommit(rec.Commit), ac.task.Identifier())}
		return nil
	})
	return out, err
}

// Undo reverts the last Apply with a new commit on the branch it went into.
func (o *Orchestrator) Undo(ctx context.Context, idOrRef string) (ApplyResult, error) {
	var out ApplyResult
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), git.DefaultTimeout+persistTimeout)
	defer cancel()
	err := o.Store.InTx(writeCtx, func(tx *store.Store) error {
		ac, err := o.prepareApply(writeCtx, tx, idOrRef)
		if err != nil {
			return err
		}
		applied, _, err := applyState(ac.events)
		if err != nil {
			return err
		}
		if applied == nil {
			return fmt.Errorf("%s is not applied; there is nothing to undo", ac.task.Identifier())
		}
		if ac.into != applied.Into {
			return fmt.Errorf("%s was applied to %s, but %s is checked out; check out %s first",
				ac.task.Identifier(), applied.Into, ac.into, applied.Into)
		}
		mainline := 1 // a merge commit, reverted to the branch it was merged into
		if applied.Reapplied {
			mainline = 0 // a re-apply is an ordinary revert commit
		}
		commit, err := o.Git.Revert(writeCtx, ac.repo, applied.Commit, mainline)
		if err != nil {
			return err
		}
		rec := undoRecord{Into: ac.into, Commit: commit, Undid: applied.Commit}
		if err := appendEvent(writeCtx, tx, ac.task.ID, nil, event.TypeTaskApplyUndone, rec); err != nil {
			return fmt.Errorf("%s was reverted as %s but the record could not be written: %w", ac.task.Identifier(), shortCommit(commit), err)
		}
		out = ApplyResult{Task: ac.task, Into: ac.into, Branch: ac.wt.Branch, Commit: commit,
			Message: fmt.Sprintf("%s reverted on %s as %s", ac.task.Identifier(), ac.into, shortCommit(commit))}
		return nil
	})
	if errors.Is(err, git.ErrConflict) {
		return out, fmt.Errorf("%w; the change no longer reverts cleanly, so undo it by hand", err)
	}
	return out, err
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
