package git

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ChangedPaths lists every path that differs between the base commit and the
// files on disk, so verification can refuse to run a runner the agent wrote.
//
// A diff against the index is blind to work the agent committed (the index
// then matches the files) and to ignored files (never in the index at all),
// which are exactly the places a replacement runner would go unseen
// (docs/research.md 7e). Comparing against the base commit and asking git for
// ignored files covers both.
func (w *Worktree) ChangedPaths(ctx context.Context) ([]string, error) {
	if strings.TrimSpace(w.BaseCommit) == "" {
		return nil, fmt.Errorf("changed paths in %s: no base commit recorded", w.Path)
	}

	tempIndex, cleanup, err := w.temporaryIndex()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tempIndex}

	if res, err := w.m.run(ctx, w.Path, env, "add", "-N", "--", "."); err != nil {
		return nil, err
	} else if !res.Succeeded() {
		return nil, fmt.Errorf("stage intent-to-add in %s: %s", w.Path, firstLine(res.Stderr))
	}

	diffRes, err := w.m.run(ctx, w.Path, env, "diff", "--name-only", "-z", w.BaseCommit)
	if err != nil {
		return nil, err
	}
	if !diffRes.Succeeded() {
		return nil, fmt.Errorf("git diff in %s: %s", w.Path, firstLine(diffRes.Stderr))
	}

	lsRes, err := w.m.run(ctx, w.Path, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, err
	}
	if !lsRes.Succeeded() {
		return nil, fmt.Errorf("git ls-files in %s: %s", w.Path, firstLine(lsRes.Stderr))
	}

	seen := map[string]bool{}
	var out []string
	for _, p := range splitNul(diffRes.Stdout) {
		if p == "" {
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range splitNul(lsRes.Stdout) {
		if p == "" {
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func splitNul(s string) []string {
	return strings.Split(s, "\x00")
}
