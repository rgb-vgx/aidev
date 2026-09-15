package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// gitIn runs git in dir with the same isolation newRepo uses.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// covers reports whether changed accounts for path: an exact entry, or an entry
// ending in "/" that stands for everything beneath it. That second form is part
// of the contract so that an ignored build directory can be reported as one
// line instead of every file in it.
func covers(changed []string, path string) bool {
	for _, c := range changed {
		if c == path || (strings.HasSuffix(c, "/") && strings.HasPrefix(path, c)) {
			return true
		}
	}
	return false
}

// ChangedPaths is what aidev decides verification interception from, so it has
// to see every way an attempt can change a file — including the ways that
// `git status` and a diff against the index do not show.
func TestChangedPathsSeesEveryKindOfChangeSinceTheBaseCommit(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)
	write(t, filepath.Join(repoPath, ".gitignore"), "*.pyc\nbuild/\n")
	write(t, filepath.Join(repoPath, "keep.txt"), "untouched\n")
	gitIn(t, repoPath, "add", "-A")
	gitIn(t, repoPath, "commit", "-qm", "ignore rules and an untouched file")

	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "changed-probe", Branch: "aidev/changed-probe"})
	if err != nil {
		t.Fatal(err)
	}

	// Modified, created, deleted: the ordinary cases.
	write(t, filepath.Join(wt.Path, "main.go"), "package main\n\nfunc main() { println() }\n")
	write(t, filepath.Join(wt.Path, "pytest.py"), "print('not the real pytest')\n")
	if err := os.Remove(filepath.Join(wt.Path, "go.mod")); err != nil {
		t.Fatal(err)
	}

	// Ignored by .gitignore: invisible to `git add -N .` and to status, and so
	// exactly where a file that shadows a verification runner would go unseen.
	write(t, filepath.Join(wt.Path, "pytest.pyc"), "compiled shadow\n")
	if err := os.MkdirAll(filepath.Join(wt.Path, "build", "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, "build", "out", "runner"), "#!/bin/sh\nexit 0\n")

	// Committed inside the worktree: the index then matches the files, so a diff
	// against the index reports nothing. The prompt asks agents not to commit;
	// aidev must not depend on that request being obeyed.
	if err := os.MkdirAll(filepath.Join(wt.Path, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, "sub", "committed.txt"), "slipped in\n")
	gitIn(t, wt.Path, "add", "sub/committed.txt")
	gitIn(t, wt.Path, "commit", "-qm", "agent committed")

	changed, err := wt.ChangedPaths(ctx)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}

	for _, want := range []string{"main.go", "pytest.py", "go.mod", "pytest.pyc", "build/out/runner", "sub/committed.txt"} {
		if !covers(changed, want) {
			t.Errorf("ChangedPaths does not account for %s: %q", want, changed)
		}
	}
	for _, unchanged := range []string{"keep.txt", ".gitignore"} {
		if covers(changed, unchanged) {
			t.Errorf("ChangedPaths reports %s, which the attempt did not touch: %q", unchanged, changed)
		}
	}

	if !sort.StringsAreSorted(changed) {
		t.Errorf("ChangedPaths is not sorted: %q", changed)
	}
	seen := map[string]bool{}
	for _, c := range changed {
		if seen[c] {
			t.Errorf("ChangedPaths reports %s twice: %q", c, changed)
		}
		seen[c] = true
		if c == "" || filepath.IsAbs(c) || strings.HasPrefix(c, "../") || strings.Contains(c, `\`) {
			t.Errorf("ChangedPaths entry %q is not a slash-separated path relative to the worktree", c)
		}
	}

	// Like Diff, it must not stage anything in the real index: a human
	// inspecting a retained attempt should see what the agent left, untouched.
	status, err := wt.Status(ctx)
	if err != nil {
		t.Fatalf("Status after ChangedPaths: %v", err)
	}
	for _, e := range status.Entries {
		if e.Path == "pytest.py" && e.Code != "??" {
			t.Errorf("pytest.py status is %q after ChangedPaths, want %q", e.Code, "??")
		}
	}
}

func TestChangedPathsOnAnUntouchedWorktreeIsEmpty(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "untouched", Branch: "aidev/untouched"})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := wt.ChangedPaths(ctx)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("ChangedPaths on a fresh worktree = %q, want nothing", changed)
	}
}

// A worktree re-attached from a stored path has no recorded base. Comparing
// against HEAD instead would silently hide anything the agent committed, so the
// answer has to be an error rather than a smaller list.
func TestChangedPathsRefusesAWorktreeWithoutAKnownBase(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "attached", Branch: "aidev/attached"})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := m.Attach(ctx, repo, wt.Path, wt.Branch)
	if err != nil {
		t.Fatal(err)
	}

	if changed, err := attached.ChangedPaths(ctx); err == nil {
		t.Fatalf("ChangedPaths without a base commit returned %q and no error", changed)
	}
}

// The recorded diff is what a reviewer reads. Measured: after a commit inside
// the worktree, a diff against the index is empty, so the agent's work vanished
// from the record exactly when it was committed.
func TestDiffIncludesWorkTheAgentCommitted(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "committed-diff", Branch: "aidev/committed-diff"})
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(wt.Path, "feature.go"), "package main\n\nfunc Feature() {}\n")
	gitIn(t, wt.Path, "add", "feature.go")
	gitIn(t, wt.Path, "commit", "-qm", "agent committed")

	diff, err := wt.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff.Patch, "func Feature()") {
		t.Errorf("the diff lost work the agent committed:\n%s", diff.Patch)
	}
	if diff.ChangedFiles != 1 {
		t.Errorf("ChangedFiles = %d, want 1", diff.ChangedFiles)
	}
}

// Interception decides what aidev must NOT run from this list, so an incomplete
// list lets a changed runner judge the agent that changed it: truncation must be
// an error, never a shorter answer. The same package already carries truncation
// for Diff (Diff.Truncated), and procexec reports it per stream, so the
// information is there to be used; ChangedPaths is the caller that cannot afford
// to ignore it. An attempt that unpacks a dependency tree with no .gitignore
// produces tens of thousands of untracked paths, which is how a real run reaches
// the 4 MiB cap.
func TestChangedPathsFailsClosedWhenGitOutputIsTruncated(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)
	write(t, filepath.Join(repoPath, ".gitignore"), "*.ignored\n")
	gitIn(t, repoPath, "add", "-A")
	gitIn(t, repoPath, "commit", "-qm", "ignore rules")

	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}

	// Each case caps output after the worktree exists, so only the two commands
	// ChangedPaths itself runs are capped, and fills the stream that case needs:
	// the diff against the base commit, or the listing of ignored files.
	cases := []struct {
		name  string
		files func(worktree string)
	}{
		{"the diff against the base commit", func(worktree string) {
			for i := 0; i < 60; i++ {
				write(t, filepath.Join(worktree, fmt.Sprintf("a-changed-file-with-a-long-name-%02d.txt", i)), "x\n")
			}
		}},
		// Ignored files are listed one by one unless they sit under an ignored
		// directory, which is collapsed to a single entry, so top-level ignored
		// files are what fills this stream.
		{"the listing of ignored files", func(worktree string) {
			for i := 0; i < 60; i++ {
				write(t, filepath.Join(worktree, fmt.Sprintf("an-ignored-file-with-a-long-name-%02d.ignored", i)), "x\n")
			}
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("truncation-probe-%d", i)
			wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: name, Branch: "aidev/" + name})
			if err != nil {
				t.Fatal(err)
			}
			tc.files(wt.Path)

			m.MaxOutputBytes = 256 // far smaller than 60 long path names
			defer func() { m.MaxOutputBytes = DefaultMaxOutputBytes }()

			changed, err := wt.ChangedPaths(ctx)
			if err == nil {
				t.Fatalf("ChangedPaths returned %d paths built from truncated output of %s; "+
					"a short list makes interception miss a changed runner", len(changed), tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "truncat") {
				t.Errorf("err = %v, want it to say the output was truncated so the cause is obvious", err)
			}
		})
	}
}
