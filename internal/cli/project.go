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
		"approval":   {"show or set whether every task of this repository needs approval first", projectApproval},
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
		fmt.Fprintf(env.Stdout, "%s\n  branch %s  submodules %s  approval %s\n",
			p.RepoPath, p.DefaultBranch, p.Submodules, onOff(p.RequiresApproval))
	}
	return nil
}

// onOff renders a boolean the way the settings that take on/off words read.
func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// projectApproval reads or writes one project's approval policy.
//
// It lives only here, with no MCP counterpart: whoever creates tasks —
// including a planner through MCP — must not decide whether its own work is
// gated, so the switch belongs to the operator at the CLI. Without an
// argument it prints the current state, because the common question is
// "is this on?".
func projectApproval(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("project approval", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	repo := fs.String("repo", ".", "path inside the repository")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev project approval: %v", err)
	}
	if len(positionals) > 1 {
		return usagef("aidev project approval: expected at most one of on or off, got %d", len(positionals))
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
		fmt.Fprintf(env.Stdout, "%s\n", onOff(project.RequiresApproval))
		return nil
	}

	var required bool
	switch positionals[0] {
	case "on":
		required = true
	case "off":
		required = false
	default:
		return usagef("aidev project approval: expected on or off, got %q", positionals[0])
	}

	updated, err := app.store.SetProjectRequiresApproval(ctx, project.ID, required)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "%s: approval %s\n", updated.RepoPath, onOff(updated.RequiresApproval))
	if updated.RequiresApproval {
		fmt.Fprintf(env.Stdout,
			"Every task of this repository now stops at WAITING_APPROVAL before it runs,\n"+
				"whether or not the task itself asked for one. Release a task with\n"+
				"aidev task approve <task> --by <name>.\n")
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
