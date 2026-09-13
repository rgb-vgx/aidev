package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"aidev/internal/event"
	"aidev/internal/git"
	"aidev/internal/store"
	"aidev/internal/task"
)

// runWorktree dispatches `aidev worktree ...`.
//
// These commands exist because the cleanup policy keeps a failed attempt's
// worktree on purpose. Without a way to find and remove those, the policy would
// accumulate directories an operator could not account for, and "the work is kept
// for inspection" would be a promise the tool does not help anyone act on.
func runWorktree(ctx context.Context, env *Env, args []string) error {
	subcommands := map[string]struct {
		summary string
		run     func(context.Context, *Env, []string) error
	}{
		"list":   {"list worktrees and the tasks that own them", worktreeList},
		"remove": {"remove a task's worktree once you are done with it", worktreeRemove},
	}

	usage := func() {
		names := make([]string, 0, len(subcommands))
		for n := range subcommands {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(env.Stderr, "usage: aidev worktree <subcommand> [flags]\n\nsubcommands:\n")
		for _, n := range names {
			fmt.Fprintf(env.Stderr, "  %-8s  %s\n", n, subcommands[n].summary)
		}
		fmt.Fprintf(env.Stderr, "\nA failed or cancelled task keeps its worktree so the partial work can be\n"+
			"inspected. These commands are how you review and then reclaim them.\n")
	}

	if len(args) == 0 {
		usage()
		return usagef("aidev worktree: no subcommand given")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		return nil
	}
	sub, ok := subcommands[args[0]]
	if !ok {
		usage()
		return usagef("aidev worktree: unknown subcommand %q", args[0])
	}
	return sub.run(ctx, env, args[1:])
}

func worktreeList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("worktree list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	all := fs.Bool("all", false, "include worktrees aidev has already removed")
	asJSON := fs.Bool("json", false, "print as JSON")
	if _, err := parseInterspersed(fs, args); err != nil {
		return usagef("aidev worktree list: %v", err)
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	statuses := []task.WorktreeStatus{task.WorktreeActive, task.WorktreeRetained}
	if *all {
		statuses = nil
	}

	items, err := app.store.ListWorktrees(ctx, statuses)
	if err != nil {
		return err
	}

	if *asJSON {
		type entry struct {
			Task          string `json:"task"`
			TaskStatus    string `json:"task_status"`
			Title         string `json:"title"`
			Attempt       int    `json:"attempt"`
			Path          string `json:"path"`
			Branch        string `json:"branch"`
			Status        string `json:"status"`
			OnDisk        bool   `json:"on_disk"`
			HeadCommit    string `json:"head_commit,omitempty"`
			CreatedAt     string `json:"created_at"`
			DiskSizeBytes int64  `json:"disk_size_bytes,omitempty"`
		}
		out := make([]entry, 0, len(items))
		for _, item := range items {
			present, size := inspectPath(item.Worktree.Path)
			out = append(out, entry{
				Task:          item.TaskRef,
				TaskStatus:    item.TaskStatus.String(),
				Title:         item.TaskTitle,
				Attempt:       item.AttemptNumber,
				Path:          item.Worktree.Path,
				Branch:        item.Worktree.Branch,
				Status:        item.Worktree.Status.String(),
				OnDisk:        present,
				HeadCommit:    item.Worktree.HeadCommit,
				CreatedAt:     item.Worktree.CreatedAt.UTC().Format(time.RFC3339),
				DiskSizeBytes: size,
			})
		}
		return writeJSON(env.Stdout, out)
	}

	if len(items) == 0 {
		fmt.Fprintln(env.Stdout, "no worktrees")
		return nil
	}

	var total int64
	for _, item := range items {
		present, size := inspectPath(item.Worktree.Path)
		total += size

		marker := " "
		note := ""
		switch {
		case !present && item.Worktree.Status != task.WorktreeRemoved:
			// Recorded as present but gone from disk: something outside aidev
			// deleted it. Saying so is more useful than showing a path that does
			// not exist.
			marker = "!"
			note = "  (missing from disk)"
		case item.Worktree.Status == task.WorktreeRetained:
			marker = "•"
		}

		fmt.Fprintf(env.Stdout, "%s %-12s %-10s attempt %d  %-9s %s%s\n",
			marker, item.TaskRef, item.TaskStatus, item.AttemptNumber,
			item.Worktree.Status, item.Worktree.Path, note)
		fmt.Fprintf(env.Stdout, "    %s  branch %s\n", truncate(item.TaskTitle, 56), item.Worktree.Branch)
		if present && size > 0 {
			fmt.Fprintf(env.Stdout, "    %s on disk\n", humanBytes(size))
		}
	}
	if total > 0 {
		fmt.Fprintf(env.Stdout, "\n%s total under %s\n", humanBytes(total), app.cfg.WorkspaceRoot)
		fmt.Fprintf(env.Stdout, "remove one with: aidev worktree remove <task>\n")
	}
	return nil
}

func worktreeRemove(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("worktree remove", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "discard uncommitted work in the worktree")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev worktree remove <task> [--force]

Removes the worktree of a task's latest attempt.

Without --force, git refuses to delete a worktree holding uncommitted or untracked
work, which is what protects a failed attempt's output. Passing --force discards
that work permanently. The task's branch is left alone either way.
`)
		fs.PrintDefaults()
	}
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev worktree remove: %v", err)
	}
	identifier, err := oneIdentifier("worktree remove", positionals)
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
	if t.Status.Active() {
		return fmt.Errorf("%s is %s: cancel it before removing its worktree", t.Identifier(), t.Status)
	}

	attempt, err := app.store.LatestAttempt(ctx, t.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%s has never run, so it has no worktree", t.Identifier())
		}
		return err
	}
	record, err := app.store.GetWorktreeByAttempt(ctx, attempt.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%s attempt %d has no worktree recorded", t.Identifier(), attempt.AttemptNumber)
		}
		return err
	}
	if record.Status == task.WorktreeRemoved {
		fmt.Fprintf(env.Stdout, "%s worktree was already removed\n", t.Identifier())
		return nil
	}

	project, err := app.store.GetProject(ctx, t.ProjectID)
	if err != nil {
		return err
	}
	repo, err := app.orchestrator.Git.OpenRepository(ctx, project.RepoPath)
	if err != nil {
		return err
	}

	present, _ := inspectPath(record.Path)
	if present {
		wt, err := app.orchestrator.Git.Attach(ctx, repo, record.Path, record.Branch)
		if err != nil {
			return err
		}
		if err := app.orchestrator.Git.Remove(ctx, wt, *force); err != nil {
			if errors.Is(err, git.ErrWorktreeDirty) {
				return fmt.Errorf("%w\n\nThe work in %s is not committed. Review it, or pass --force to discard it permanently",
					err, record.Path)
			}
			return err
		}
	} else {
		// Already gone from disk: reconcile the record rather than refusing, and
		// prune git's administrative entry.
		if err := app.orchestrator.Git.Prune(ctx, repo); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "%s was already gone from disk; reconciling the record\n", record.Path)
	}

	if err := app.store.SetWorktreeStatus(ctx, record.ID, task.WorktreeRemoved); err != nil {
		return err
	}
	appendRemovalEvent(ctx, app, t, record, *force)

	fmt.Fprintf(env.Stdout, "removed %s\n", record.Path)
	fmt.Fprintf(env.Stdout, "the branch %s is untouched\n", record.Branch)
	return nil
}

// appendRemovalEvent records the removal in the task's history. A failure to write
// it is reported but does not undo the removal, which has already happened.
func appendRemovalEvent(ctx context.Context, app *app, t task.Task, record task.Worktree, forced bool) {
	e, err := event.New(t.ID, &record.AttemptID, event.TypeWorktreeRemoved, map[string]any{
		"path":   record.Path,
		"branch": record.Branch,
		"forced": forced,
		"by":     "operator",
		"reason": "removed with aidev worktree remove",
	})
	if err == nil {
		if _, err = app.store.AppendEvent(ctx, e); err == nil {
			return
		}
	}
	app.orchestrator.Logger.WarnContext(ctx, "could not record the worktree removal in the task's history",
		"task_ref", t.Ref, "error", err.Error())
}

// inspectPath reports whether a path exists and how much disk it uses.
func inspectPath(path string) (present bool, size int64) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false, 0
	}
	// Walk rather than shelling out to du: one fewer external process, and it
	// cannot be affected by the environment.
	_ = filepathWalk(path, func(fi os.FileInfo) {
		if fi.Mode().IsRegular() {
			size += fi.Size()
		}
	})
	return true, size
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

// filepathWalk visits every entry under root. Errors on individual entries are
// ignored: a size report is a convenience, and one unreadable file should not stop
// it.
func filepathWalk(root string, visit func(os.FileInfo)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if entry.IsDir() {
			_ = filepathWalk(root+string(os.PathSeparator)+entry.Name(), visit)
			continue
		}
		visit(info)
	}
	return nil
}
