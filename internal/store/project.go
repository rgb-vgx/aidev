package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"aidev/internal/task"
)

const projectColumns = `id, name, repo_path, default_branch, created_at, updated_at`

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

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanProject(row scanner) (task.Project, error) {
	var p task.Project
	err := row.Scan(&p.ID, &p.Name, &p.RepoPath, &p.DefaultBranch, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return task.Project{}, classify(err)
	}
	return p, nil
}
