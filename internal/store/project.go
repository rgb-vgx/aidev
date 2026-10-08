package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"aidev/internal/task"
)

const projectColumns = `id, name, repo_path, default_branch, submodules, requires_approval, verification_mode, created_at, updated_at, read_dirs`

// EnsureProject registers repoPath as a project, or returns the existing
// project for that path unchanged.
//
// The insert-or-select is done in one statement so that two callers racing to
// register the same repository cannot produce a unique-violation for one of
// them; whichever loses the insert reads the winner's row.
func (s *Store) EnsureProject(ctx context.Context, name, repoPath, defaultBranch string) (task.Project, error) {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	row := s.db.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO projects (id, name, repo_path, default_branch)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (repo_path) DO NOTHING
			RETURNING `+projectColumns+`
		)
		SELECT `+projectColumns+` FROM inserted
		UNION ALL
		SELECT `+projectColumns+` FROM projects
		WHERE repo_path = $3 AND NOT EXISTS (SELECT 1 FROM inserted)`,
		uuid.Must(uuid.NewV7()), name, repoPath, defaultBranch)

	p, err := scanProject(row)
	if err != nil {
		return task.Project{}, fmt.Errorf("ensure project %s: %w", repoPath, err)
	}
	return p, nil
}

// GetProject returns a project by id.
func (s *Store) GetProject(ctx context.Context, id uuid.UUID) (task.Project, error) {
	row := s.db.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id)
	p, err := scanProject(row)
	if err != nil {
		return task.Project{}, fmt.Errorf("get project %s: %w", id, err)
	}
	return p, nil
}

// GetProjectByPath returns a project by repository path.
func (s *Store) GetProjectByPath(ctx context.Context, repoPath string) (task.Project, error) {
	row := s.db.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE repo_path = $1`, repoPath)
	p, err := scanProject(row)
	if err != nil {
		return task.Project{}, fmt.Errorf("get project at %s: %w", repoPath, err)
	}
	return p, nil
}

// ListProjects returns every project, newest first.
func (s *Store) ListProjects(ctx context.Context) ([]task.Project, error) {
	rows, err := s.db.Query(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", classify(err))
	}
	defer rows.Close()

	var projects []task.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", classify(err))
	}
	return projects, nil
}

// SetProjectSubmodules changes how a project's task worktrees treat git
// submodules and returns the project as it now stands.
//
// It is a separate call rather than an argument to EnsureProject because
// EnsureProject runs on every task creation: passing the mode there would let a
// task creation silently reset a setting an operator had chosen.
func (s *Store) SetProjectSubmodules(ctx context.Context, id uuid.UUID, mode task.SubmoduleMode) (task.Project, error) {
	row := s.db.QueryRow(ctx, `
		UPDATE projects SET submodules = $2, updated_at = now()
		WHERE id = $1
		RETURNING `+projectColumns, id, string(mode))

	p, err := scanProject(row)
	if err != nil {
		return task.Project{}, fmt.Errorf("set submodules for project %s: %w", id, err)
	}
	return p, nil
}

// SetProjectRequiresApproval switches the project's approval policy and
// returns the project as it now stands.
//
// It exists as its own call, and has no MCP counterpart on purpose: the
// policy answers "who may turn the gate off", and the answer is the operator
// at the CLI. The party creating tasks — including a planner through MCP —
// must not be able to reach it.
func (s *Store) SetProjectRequiresApproval(ctx context.Context, id uuid.UUID, required bool) (task.Project, error) {
	row := s.db.QueryRow(ctx, `
		UPDATE projects SET requires_approval = $2, updated_at = now()
		WHERE id = $1
		RETURNING `+projectColumns, id, required)

	p, err := scanProject(row)
	if err != nil {
		return task.Project{}, fmt.Errorf("set approval policy for project %s: %w", id, err)
	}
	return p, nil
}

// SetProjectVerificationMode switches the default where this project's tasks
// verify and returns the project as it now stands.
//
// Like SetProjectRequiresApproval it is its own call with no MCP counterpart:
// the project row is a template read at task creation, and the party creating
// tasks must not be able to rewrite the template other tasks will inherit.
// Tasks already created keep the mode frozen into their own row.
func (s *Store) SetProjectVerificationMode(ctx context.Context, id uuid.UUID, mode task.VerificationMode) (task.Project, error) {
	row := s.db.QueryRow(ctx, `
		UPDATE projects SET verification_mode = $2, updated_at = now()
		WHERE id = $1
		RETURNING `+projectColumns, id, string(mode))

	p, err := scanProject(row)
	if err != nil {
		return task.Project{}, fmt.Errorf("set verification mode for project %s: %w", id, err)
	}
	return p, nil
}

// SetProjectReadDirs replaces the directories outside the repository that the
// project's agent may read, and returns the project as it now stands. Like the
// approval policy it is its own call with no MCP counterpart: the party
// creating tasks must not widen what its own agent can reach.
func (s *Store) SetProjectReadDirs(ctx context.Context, id uuid.UUID, dirs []string) (task.Project, error) {
	if dirs == nil {
		dirs = []string{}
	}
	row := s.db.QueryRow(ctx, `
		UPDATE projects SET read_dirs = $2, updated_at = now()
		WHERE id = $1
		RETURNING `+projectColumns, id, dirs)

	p, err := scanProject(row)
	if err != nil {
		return task.Project{}, fmt.Errorf("set read dirs for project %s: %w", id, err)
	}
	return p, nil
}

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanProject(row scanner) (task.Project, error) {
	var p task.Project
	err := row.Scan(&p.ID, &p.Name, &p.RepoPath, &p.DefaultBranch, &p.Submodules, &p.RequiresApproval,
		&p.VerificationMode, &p.CreatedAt, &p.UpdatedAt, &p.ReadDirs)
	if err != nil {
		return task.Project{}, classify(err)
	}
	return p, nil
}
