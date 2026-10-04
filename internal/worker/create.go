package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"aidev/internal/agent"
	"aidev/internal/event"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/verification"
)

// CreateTaskInput is what a caller supplies to create a task. It names the
// repository by path rather than by project id, because that is what a person or
// a planner actually has; the project is registered on demand.
type CreateTaskInput struct {
	RepoPath string

	Title              string
	Description        string
	AcceptanceCriteria string

	Agent        string
	Model        string
	Hardness     string
	Priority     int
	Verification []task.VerificationStep
	// ProtectedPaths are glob patterns the creator ring-fences; they are
	// normalized and validated here, at the one entry point every caller uses,
	// so the database only ever holds patterns verification can evaluate.
	ProtectedPaths []string
	// SetupSteps run before verification to prepare the checkout. Parsed by
	// the caller (argv, no shell); validated again by task.New.
	SetupSteps []task.VerificationStep
	// VerificationMode is the caller's choice of where verification runs —
	// "in_place", "clean", or empty to take the project's default. It is
	// resolved and frozen into the task row here, so later changes to the
	// project default cannot move the goalposts under this task.
	VerificationMode string
	MaxRetries       int
	RequiresApproval bool
	// ExpectFailOnBase runs the verification commands on the base commit
	// before the agent is called, and fails the task when they already pass
	// there: such commands cannot tell before from after (research B3).
	ExpectFailOnBase bool
	BaseRef          string
	Timeout          time.Duration
}

// CreateTask validates and persists a task.
//
// Three checks happen before anything is written, because each one is cheaper to
// report now than to discover halfway through a run:
//
//   - the path must be a real git repository, since a task that cannot be
//     isolated cannot be run;
//   - the base ref must resolve, so a typo is caught at creation rather than at
//     worktree creation;
//   - the agent name must be one the backend recognises. Phase 0 measured
//     OpenCode accepting an unknown name, warning, silently using its default and
//     exiting 0 (docs/research.md §2.5), so aidev refuses it up front.
func (o *Orchestrator) CreateTask(ctx context.Context, in CreateTaskInput) (task.Task, error) {
	repo, err := o.Git.OpenRepository(ctx, in.RepoPath)
	if err != nil {
		return task.Task{}, err
	}

	if in.BaseRef != "" {
		if _, err := o.Git.ResolveCommit(ctx, repo, in.BaseRef); err != nil {
			return task.Task{}, err
		}
	}

	// Trim before deciding whether an agent was supplied: "  " means the caller
	// named nothing, so the configured default applies. What must never happen is
	// falling back to a different agent than one that was actually named — that is
	// the OpenCode behaviour aidev exists to prevent (docs/research.md §2.5).
	agentName := strings.TrimSpace(in.Agent)
	if agentName == "" {
		agentName = strings.TrimSpace(o.Config.OpenCodeAgent)
	}
	if agentName == "" {
		return task.Task{}, fmt.Errorf("no agent named and no default configured")
	}
	if validator, ok := o.Backend.(agent.Validator); ok {
		if err := validator.ValidateAgentName(ctx, agentName); err != nil {
			return task.Task{}, err
		}
	}

	// Reject an unusable protection list before anything is written: a pattern
	// that cannot match would guard nothing while claiming to, and discovering
	// it at verification time would fail a task the agent never had a chance
	// on.
	protected, err := verification.NormalizeProtected(in.ProtectedPaths)
	if err != nil {
		return task.Task{}, err
	}

	project, err := o.Store.EnsureProject(ctx, filepath.Base(repo.Path), repo.Path, o.Git.CurrentBranch(ctx, repo))
	if err != nil {
		return task.Task{}, err
	}

	// Record what the base ref points at now (research D2), resolved by the
	// same rule the run uses — the task's ref, else the project's default
	// branch — so the two commits are comparable and a ref that moved in
	// between is a real move, not two different rules disagreeing.
	baseAtCreate, err := o.Git.ResolveCommit(ctx, repo, effectiveBaseRef(in.BaseRef, project.DefaultBranch))
	if err != nil {
		return task.Task{}, err
	}

	// Resolve the mode now and freeze it into the task: an explicit choice
	// wins, otherwise the project's default applies as of this moment.
	// Later changes to the project row affect only tasks created after
	// them, so verification stays a pure function of the task.
	mode := project.VerificationMode
	if strings.TrimSpace(in.VerificationMode) != "" {
		mode, err = task.ParseVerificationMode(in.VerificationMode)
		if err != nil {
			return task.Task{}, err
		}
	}

	// Clean verification cannot honour submodules: the detached checkout of
	// the snapshot would contain empty directories, and the checks would run
	// against sources that are not there. The base check of an
	// expect_fail_on_base task runs in the same kind of detached checkout —
	// the base commit before the agent ran — so a repository pinning
	// submodules would make its base pass read red for the wrong reason and
	// the gate would never fire. Refuse now — with the offending path —
	// rather than letting such a task fail, or silently stop protecting,
	// with a result nobody can explain.
	if mode == task.VerificationClean || in.ExpectFailOnBase {
		base := baseAtCreate
		if found, path, err := o.Git.HasGitlinks(ctx, repo, base); err != nil {
			return task.Task{}, err
		} else if found {
			var refused []string
			if mode == task.VerificationClean {
				refused = append(refused, "verification mode clean")
			}
			if in.ExpectFailOnBase {
				refused = append(refused, "expect_fail_on_base")
			}
			return task.Task{}, fmt.Errorf(
				"%s does not support submodules: %s is a gitlink at %s; "+
					"the detached checkouts those runs use cannot hold submodule content",
				strings.Join(refused, " and "), path, base)
		}
	}

	built, err := task.New(task.NewTaskInput{
		ProjectID:          project.ID,
		Title:              in.Title,
		Description:        in.Description,
		Agent:              agentName,
		Model:              in.Model,
		Hardness:           in.Hardness,
		Priority:           in.Priority,
		AcceptanceCriteria: in.AcceptanceCriteria,
		Verification:       in.Verification,
		ProtectedPaths:     protected,
		SetupSteps:         in.SetupSteps,
		VerificationMode:   mode,
		MaxRetries:         in.MaxRetries,
		RequiresApproval:   in.RequiresApproval,
		ExpectFailOnBase:   in.ExpectFailOnBase,
		BaseRef:            in.BaseRef,
		BaseCommitAtCreate: baseAtCreate,
		Timeout:            in.Timeout,
	}, o.Config.OpenCodeAgent)
	if err != nil {
		return task.Task{}, err
	}

	var created task.Task
	err = o.Store.InTx(ctx, func(tx *store.Store) error {
		stored, err := tx.CreateTask(ctx, built)
		if err != nil {
			return err
		}
		created = stored

		commands := make([]string, 0, len(stored.Verification))
		for _, step := range stored.Verification {
			commands = append(commands, step.String())
		}
		return appendEvent(ctx, tx, stored.ID, nil, event.TypeTaskCreated, map[string]any{
			"title":               stored.Title,
			"agent":               stored.Agent,
			"priority":            stored.Priority,
			"requires_approval":   stored.RequiresApproval,
			"expect_fail_on_base": stored.ExpectFailOnBase,
			"verification":        commands,
			"verification_mode":   string(stored.VerificationMode),
			"repo_path":           repo.Path,
			"base_commit":         stored.BaseCommitAtCreate,
		})
	})
	if err != nil {
		return task.Task{}, err
	}

	o.Logger.InfoContext(ctx, "task created",
		logging.FieldTaskID, created.ID.String(),
		logging.FieldTaskRef, created.Ref,
		logging.FieldProjectID, created.ProjectID.String(),
		"agent", created.Agent,
		"verification_steps", len(created.Verification))

	return created, nil
}

// effectiveBaseRef is the ref a task's worktree branches from: the task's own
// base_ref, or the project's default branch when it names none. CreateTask
// and the run both use it, so the commit recorded at creation and the one
// the worktree starts from are resolved the same way.
func effectiveBaseRef(taskRef, defaultBranch string) string {
	if strings.TrimSpace(taskRef) != "" {
		return taskRef
	}
	return defaultBranch
}
