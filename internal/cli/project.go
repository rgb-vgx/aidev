package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"

	"aidev/internal/store"
	"aidev/internal/task"
)

// runProject dispatches `aidev project ...`.
//
// A project is registered on demand when its first task is created, so there is
// nothing here to create one. What there is, is the handful of settings that
// belong to the repository rather than to any one task — at present, what a task
// worktree does about git submodules.
func runProject(ctx context.Context, env *Env, args []string) error {
	subcommands := map[string]struct {
		summary string
		run     func(context.Context, *Env, []string) error
	}{
		"list":       {"list the repositories aidev has run tasks against", projectList},
		"submodules": {"show or set how task worktrees treat this repository's submodules", projectSubmodules},
	}

	usage := func() {
		names := make([]string, 0, len(subcommands))
		for n := range subcommands {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(env.Stderr, "usage: aidev project <subcommand> [flags]\n\nsubcommands:\n")
		for _, n := range names {
			fmt.Fprintf(env.Stderr, "  %-11s  %s\n", n, subcommands[n].summary)
		}
	}

	if len(args) == 0 {
		usage()
		return usagef("aidev project: no subcommand given")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		return nil
	}
	sub, ok := subcommands[args[0]]
	if !ok {
		usage()
		return usagef("aidev project: unknown subcommand %q", args[0])
	}
	return sub.run(ctx, env, args[1:])
}

func projectList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("project list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	if _, err := parseInterspersed(fs, args); err != nil {
		return usagef("aidev project list: %v", err)
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	projects, err := app.store.ListProjects(ctx)
	if err != nil {
		return err
	}

	if *asJSON {
		return writeJSON(env.Stdout, projects)
	}
	if len(projects) == 0 {
		fmt.Fprintln(env.Stdout, "no projects yet; one is registered when you create its first task")
		return nil
	}
	for _, p := range projects {
		fmt.Fprintf(env.Stdout, "%s\n  branch %s  submodules %s\n", p.RepoPath, p.DefaultBranch, p.Submodules)
	}
	return nil
}

// projectSubmodules reads or writes one project's submodule mode.
//
// Without a mode it prints the current one, because the common question is
// "is this on?" and answering it should not require knowing the vocabulary.
func projectSubmodules(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("project submodules", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	repo := fs.String("repo", ".", "path inside the repository")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev project submodules: %v", err)
	}
	if len(positionals) > 1 {
		return usagef("aidev project submodules: expected at most one mode, got %d", len(positionals))
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	repository, err := app.orchestrator.Git.OpenRepository(ctx, *repo)
	if err != nil {
		return err
	}
	project, err := app.store.GetProjectByPath(ctx, repository.Path)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("aidev does not know %s yet; it is registered when you create its first task",
				repository.Path)
		}
		return err
	}

	if len(positionals) == 0 {
		fmt.Fprintf(env.Stdout, "%s\n", project.Submodules)
		return nil
	}

	mode, err := task.ParseSubmoduleMode(positionals[0])
	if err != nil {
		return err
	}
	updated, err := app.store.SetProjectSubmodules(ctx, project.ID, mode)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "%s: submodules %s\n", updated.RepoPath, updated.Submodules)
	if updated.Submodules == task.SubmodulesReadOnly {
		fmt.Fprintf(env.Stdout,
			"Each task worktree now gets a checkout of every submodule, at the commit this\n"+
				"repository pins. They are read-only: a task still leaves one commit on one\n"+
				"branch of this repository, and nothing inside a submodule is committed.\n")
	}
	return nil
}
