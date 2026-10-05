package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"aidev/internal/store"
	"aidev/internal/task"
)

// taskDiff shows what a task changed: the committed diff for a delivered
// task, or the uncommitted change its agent left behind when nothing was
// delivered.
func taskDiff(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task diff", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev task diff <task>

Shows what a task changed: the committed diff from its base commit to its head
commit when the work was delivered, or the uncommitted change its agent left in
the worktree, marked as not delivered, when it was not.
`)
		fs.PrintDefaults()
	}
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev task diff: %v", err)
	}
	identifier, err := oneIdentifier("diff", positionals)
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
	ref := t.Identifier()

	attempt, err := app.store.LatestAttempt(ctx, t.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%s has not run, so there is no change to show", ref)
		}
		return err
	}

	// Only a SUCCEEDED task delivered its work to its branch. A head past the
	// base is not enough: after a retry that ended in failure it is the
	// earlier attempt's unverified partial commit.
	if wt, err := app.store.GetWorktreeByAttempt(ctx, attempt.ID); err == nil {
		if t.Status == task.StatusSucceeded && wt.HeadCommit != "" && wt.HeadCommit != wt.BaseCommit {
			project, err := app.store.GetProject(ctx, t.ProjectID)
			if err != nil {
				return err
			}
			repo, err := app.orchestrator.Git.OpenRepository(ctx, project.RepoPath)
			if err != nil {
				return err
			}
			diff, truncated, err := app.orchestrator.Git.DiffCommits(ctx, repo, wt.BaseCommit, wt.HeadCommit)
			if err != nil {
				return err
			}
			fmt.Fprint(env.Stdout, diff)
			if truncated {
				fmt.Fprintf(env.Stderr, "the diff was truncated at aidev's output limit; see all of it with: git -C %s diff %s %s\n",
					repo.Path, wt.BaseCommit, wt.HeadCommit)
			}
			return nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	runs, err := app.store.ListWorkerRuns(ctx, attempt.ID)
	if err != nil {
		return err
	}
	var diff string
	if len(runs) > 0 {
		diff = runs[len(runs)-1].Diff
	}
	if diff == "" {
		fmt.Fprintf(env.Stdout, "%s has no recorded change\n", ref)
		return nil
	}
	fmt.Fprintf(env.Stdout, "# not delivered: the change %s's agent left uncommitted in its worktree\n", ref)
	fmt.Fprint(env.Stdout, diff)
	return nil
}
