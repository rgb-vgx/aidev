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
)

// CreateTaskInput is what a caller supplies to create a task. It names the
// repository by path rather than by project id, because that is what a person or
// a planner actually has; the project is registered on demand.
type CreateTaskInput struct {
	RepoPath string

	Title              string
	Description        string
	AcceptanceCriteria string

	Agent            string
	Priority         int
	Verification     []task.VerificationStep
	MaxRetries       int
	RequiresApproval bool
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

	project, err := o.Store.EnsureProject(ctx, filepath.Base(repo.Path), repo.Path, o.Git.CurrentBranch(ctx, repo))
	if err != nil {
		return task.Task{}, err
	}

	built, err := task.New(task.NewTaskInput{
		ProjectID:          project.ID,
		Title:              in.Title,
		Description:        in.Description,
		Agent:              agentName,
		Priority:           in.Priority,
		AcceptanceCriteria: in.AcceptanceCriteria,
		Verification:       in.Verification,
		MaxRetries:         in.MaxRetries,
		RequiresApproval:   in.RequiresApproval,
		BaseRef:            in.BaseRef,
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
			"title":             stored.Title,
			"agent":             stored.Agent,
			"priority":          stored.Priority,
			"requires_approval": stored.RequiresApproval,
			"verification":      commands,
			"repo_path":         repo.Path,
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
