package git

import (
	"context"
	"fmt"
	"strings"
)

// DiffCommits returns the patch from one commit to another, so that a
// delivered task can be reviewed with ordinary git output.
func (m *Manager) DiffCommits(ctx context.Context, repo Repository, from, to string) (string, error) {
	if strings.TrimSpace(from) == "" {
		return "", fmt.Errorf("diff commits in %s: from commit is required", repo.Path)
	}
	if strings.TrimSpace(to) == "" {
		return "", fmt.Errorf("diff commits in %s: to commit is required", repo.Path)
	}
	res, err := m.run(ctx, repo.Path, nil, "diff", "--no-ext-diff", "--no-textconv", from, to)
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("git diff in %s: %s", repo.Path, firstLine(res.Stderr))
	}
	return res.Stdout, nil
}
