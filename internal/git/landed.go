package git

import (
	"context"
	"fmt"
	"strings"
)

// Landed reports whether the work between base and head is already on the
// branch into: head is an ancestor of it (merged, fast-forwarded), or every
// commit in base..head has an equivalent change there — the same patch,
// cherry-picked or rebased (`git cherry`, which compares patch ids). An empty
// range has delivered nothing, so it has not landed.
//
// It is how aidev recognises work a person took with plain git rather than
// with `aidev task apply`, which records its own event.
func (m *Manager) Landed(ctx context.Context, repo Repository, into, base, head string) (bool, error) {
	if strings.TrimSpace(into) == "" || strings.TrimSpace(base) == "" || strings.TrimSpace(head) == "" {
		return false, fmt.Errorf("landed: branch, base and head are all required")
	}
	if base == head {
		return false, nil
	}
	anc, err := m.run(ctx, repo.Path, nil, "merge-base", "--is-ancestor", head, into)
	if err != nil {
		return false, err
	}
	if anc.Succeeded() {
		return true, nil
	}
	if anc.ExitCode == nil || *anc.ExitCode != 1 {
		return false, fmt.Errorf("merge-base --is-ancestor %s %s in %s: %s", head, into, repo.Path, firstLine(anc.Stderr))
	}

	cherry, err := m.run(ctx, repo.Path, nil, "cherry", into, head, base)
	if err != nil {
		return false, err
	}
	if !cherry.Succeeded() {
		return false, fmt.Errorf("git cherry %s %s %s in %s: %s", into, head, base, repo.Path, firstLine(cherry.Stderr))
	}
	lines := strings.Fields(strings.ReplaceAll(cherry.Stdout, "\n", " "))
	if len(lines) == 0 {
		return false, nil
	}
	// Output alternates sign and commit: "- <sha>" for a commit with an
	// equivalent upstream, "+ <sha>" for one without.
	for i := 0; i < len(lines); i += 2 {
		if lines[i] != "-" {
			return false, nil
		}
	}
	return true, nil
}
