package git

import (
	"context"
	"fmt"
	"strings"
)

// DiffCommits returns the patch from one commit to another, so that a
// delivered task can be reviewed with ordinary git output. truncated reports
// that the patch was longer than aidev captures (the Manager's output cap):
// the caller must say so rather than show half a diff as if it were all.
func (m *Manager) DiffCommits(ctx context.Context, repo Repository, from, to string) (patch string, truncated bool, err error) {
	if strings.TrimSpace(from) == "" {
		return "", false, fmt.Errorf("diff commits in %s: from commit is required", repo.Path)
	}
	if strings.TrimSpace(to) == "" {
		return "", false, fmt.Errorf("diff commits in %s: to commit is required", repo.Path)
	}
	// "--" ends the revisions, so neither can be read as a path.
	res, err := m.run(ctx, repo.Path, nil, "diff", "--no-ext-diff", "--no-textconv", from, to, "--")
	if err != nil {
		return "", false, err
	}
	if !res.Succeeded() {
		return "", false, fmt.Errorf("git diff in %s: %s", repo.Path, firstLine(res.Stderr))
	}
	return res.Stdout, res.StdoutTruncated, nil
}
