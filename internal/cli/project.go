package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"sort"

	"aidev/internal/store"
	"aidev/internal/task"
)

// runProject dispatches `aidev project ...`.
//
// A project is registered by `project add`, or on demand by its first
// `aidev task create`. The MCP server does not register repositories unless
// mcp.auto_register_projects says it may (research D3): the list of projects
// is then the list of repositories a planner is allowed to send an agent
// into. The other subcommands are the handful of settings that belong to the
// repository rather than to any one task.
func runProject(ctx context.Context, env *Env, args []string) error {
	subcommands := map[string]struct {
		summary string
		run     func(context.Context, *Env, []string) error
	}{
		"add":         {"register a repository, so tasks for it may be created through MCP", projectAdd},
		"list":        {"list the repositories aidev knows", projectList},
		"submodules":  {"show or set how task worktrees treat this repository's submodules", projectSubmodules},
		"approval":    {"show or set whether every task of this repository needs approval first", projectApproval},
		"verify-mode": {"show or set where new tasks verify: in the agent's worktree or a clean checkout", projectVerifyMode},
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
		fmt.Fprintln(env.Stdout, "no projects yet; register one with aidev project add <path>")
		return nil
	}
	for _, p := range projects {
		fmt.Fprintf(env.Stdout, "%s\n  branch %s  submodules %s  approval %s  verify %s\n",
			p.RepoPath, p.DefaultBranch, p.Submodules, onOff(p.RequiresApproval), p.VerificationMode)
	}
	return nil
}

// projectAdd registers a repository. It is how a person tells aidev that a
// planner may send agents into this repository through MCP (research D3);
// registering one that is already known changes nothing and says so.
func projectAdd(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("project add", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev project add: %v", err)
	}
	if len(positionals) > 1 {
		return usagef("aidev project add: expected at most one repository path, got %d", len(positionals))
	}
	path := "."
	if len(positionals) == 1 {
		path = positionals[0]
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	// Resolved through git, so a path inside the repository registers the
	// repository itself, under the same key every later lookup uses.
	repository, err := app.orchestrator.Git.OpenRepository(ctx, path)
	if err != nil {
		return err
	}
	if existing, err := app.store.GetProjectByPath(ctx, repository.Path); err == nil {
		fmt.Fprintf(env.Stdout, "%s is already registered (branch %s)\n", existing.RepoPath, existing.DefaultBranch)
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	project, err := app.store.EnsureProject(ctx, filepath.Base(repository.Path), repository.Path,
		app.orchestrator.Git.CurrentBranch(ctx, repository))
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "registered %s (branch %s)\n", project.RepoPath, project.DefaultBranch)
	fmt.Fprintf(env.Stdout, "Tasks for it may now be created through MCP as well as the CLI.\n")
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
			return fmt.Errorf("aidev does not know %s yet; register it with aidev project add %s",
				repository.Path, repository.Path)
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
			return fmt.Errorf("aidev does not know %s yet; register it with aidev project add %s",
				repository.Path, repository.Path)
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

// projectVerifyMode reads or writes where this project's new tasks verify.
//
// The mode is a template, not policy: each task freezes its own copy at
// creation, so changing it here never re-judges a task that already exists.
// Like approval it has no MCP counterpart — the project row is read by the
// party creating tasks, and that party must not be able to rewrite the
// template other tasks inherit.
func projectVerifyMode(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("project verify-mode", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	repo := fs.String("repo", ".", "path inside the repository")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return usagef("aidev project verify-mode: %v", err)
	}
	if len(positionals) > 1 {
		return usagef("aidev project verify-mode: expected at most one of in_place or clean, got %d", len(positionals))
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
			return fmt.Errorf("aidev does not know %s yet; register it with aidev project add %s",
				repository.Path, repository.Path)
		}
		return err
	}

	if len(positionals) == 0 {
		fmt.Fprintf(env.Stdout, "%s\n", project.VerificationMode)
		return nil
	}

	mode, err := task.ParseVerificationMode(positionals[0])
	if err != nil {
		return err
	}
	updated, err := app.store.SetProjectVerificationMode(ctx, project.ID, mode)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "%s: verify-mode %s\n", updated.RepoPath, updated.VerificationMode)
	if updated.VerificationMode == task.VerificationClean {
		fmt.Fprintf(env.Stdout,
			"Every task created from now on verifies in a fresh checkout of its result:\n"+
				"files git ignores, or files the agent never added, cannot make the checks pass.\n"+
				"Tasks already created keep the mode they were created with.\n")
	}
	return nil
}
