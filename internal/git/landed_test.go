package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestLanded(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	path := newRepo(t)
	repo, err := m.OpenRepository(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	rev := func(ref string) string { return strings.TrimSpace(gitIn(t, path, "rev-parse", ref)) }
	base := rev("HEAD")

	// A task branch with one commit, off main.
	gitIn(t, path, "checkout", "-q", "-b", "task")
	write(t, filepath.Join(path, "task.txt"), "work\n")
	gitIn(t, path, "add", "task.txt")
	gitIn(t, path, "commit", "-qm", "task work")
	head := rev("HEAD")
	gitIn(t, path, "checkout", "-q", "main")

	check := func(want bool, why string) {
		t.Helper()
		got, err := m.Landed(ctx, repo, "main", base, head)
		if err != nil {
			t.Fatalf("%s: Landed: %v", why, err)
		}
		if got != want {
			t.Errorf("%s: Landed = %v, want %v", why, got, want)
		}
	}

	check(false, "nobody took it")
	if got, _ := m.Landed(ctx, repo, "main", base, base); got {
		t.Error("an empty range counts as landed")
	}

	// Someone else's commit on main first, then a cherry-pick: a new commit
	// with the same patch.
	write(t, filepath.Join(path, "other.txt"), "other\n")
	gitIn(t, path, "add", "other.txt")
	gitIn(t, path, "commit", "-qm", "unrelated")
	gitIn(t, path, "cherry-pick", head)
	check(true, "cherry-picked")

	// A real merge, in a second repository state: reset main and merge.
	gitIn(t, path, "reset", "-q", "--hard", base)
	check(false, "after resetting main")
	gitIn(t, path, "merge", "--no-ff", "-q", "-m", "merge", "task")
	check(true, "merged")
}
