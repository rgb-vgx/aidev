package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sharedStateFixture is a complete snapshot with one entry in every collection,
// so a mutation in a test can never be mistaken for "the key was absent".
func sharedStateFixture() SharedState {
	return SharedState{
		CommonDir: "/repo/.git",
		Refs: map[string]string{
			"refs/heads/main":    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"refs/heads/feature": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"refs/tags/v1":       "cccccccccccccccccccccccccccccccccccccccc",
		},
		HeadRef: "refs/heads/feature",
		Config:  "config-hash",
		Info: map[string]string{
			"attributes": "attr-hash",
			"exclude":    "exclude-hash",
		},
		Hooks: map[string]string{
			"pre-commit": "hook-hash",
		},
	}
}

// The comparison must report nothing for two snapshots of the same state,
// whatever they contain — otherwise every task would fail its containment check.
func TestSharedStateDiffFindsNothingWhenNothingChanged(t *testing.T) {
	before := sharedStateFixture()
	changes := before.Diff(sharedStateFixture())
	if !changes.Empty() {
		t.Errorf("Diff between identical snapshots = %+v, want empty", changes)
	}
}

// Each vector of the shared-state attack surface (docs/research.md §7i) must be
// classified on its own: the worker turns each of these into either a breach or
// a warning, so a missed detection is a silent escape.
func TestSharedStateDiffClassifiesEachVector(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SharedState)
		check  func(t *testing.T, changes SharedChanges)
	}{
		{
			name:   "common dir moved",
			mutate: func(a *SharedState) { a.CommonDir = "/elsewhere/.git" },
			check: func(t *testing.T, c SharedChanges) {
				if !c.CommonDirMoved {
					t.Error("a moved gitdir was not reported")
				}
			},
		},
		{
			name:   "config content changed",
			mutate: func(a *SharedState) { a.Config = "other-hash" },
			check: func(t *testing.T, c SharedChanges) {
				if !c.ConfigChanged {
					t.Error("a changed config was not reported")
				}
			},
		},
		{
			name:   "config appeared",
			mutate: func(a *SharedState) { a.Config = "" },
			check: func(t *testing.T, c SharedChanges) {
				// The fixture's config must read as present: "" means the file
				// is absent, and absence is not equal to content.
				if !c.ConfigChanged {
					t.Error("an appearing config file was not reported")
				}
			},
		},
		{
			name:   "info file added",
			mutate: func(a *SharedState) { a.Info["filter"] = "new-hash" },
			check: func(t *testing.T, c SharedChanges) {
				if want := []string{"filter"}; strings.Join(c.InfoChanged, ",") != strings.Join(want, ",") {
					t.Errorf("InfoChanged = %v, want %v", c.InfoChanged, want)
				}
			},
		},
		{
			name:   "info file removed",
			mutate: func(a *SharedState) { delete(a.Info, "attributes") },
			check: func(t *testing.T, c SharedChanges) {
				if want := []string{"attributes"}; strings.Join(c.InfoChanged, ",") != strings.Join(want, ",") {
					t.Errorf("InfoChanged = %v, want %v", c.InfoChanged, want)
				}
			},
		},
		{
			name:   "hook replaced",
			mutate: func(a *SharedState) { a.Hooks["pre-commit"] = "evil-hash" },
			check: func(t *testing.T, c SharedChanges) {
				if want := []string{"pre-commit"}; strings.Join(c.HooksChanged, ",") != strings.Join(want, ",") {
					t.Errorf("HooksChanged = %v, want %v", c.HooksChanged, want)
				}
			},
		},
		{
			name:   "hook added",
			mutate: func(a *SharedState) { a.Hooks["post-commit"] = "evil-hash" },
			check: func(t *testing.T, c SharedChanges) {
				if want := []string{"post-commit"}; strings.Join(c.HooksChanged, ",") != strings.Join(want, ",") {
					t.Errorf("HooksChanged = %v, want %v", c.HooksChanged, want)
				}
			},
		},
		{
			name:   "ref moved",
			mutate: func(a *SharedState) { a.Refs["refs/heads/main"] = "dddddddddddddddddddddddddddddddddddddddd" },
			check: func(t *testing.T, c SharedChanges) {
				ch, ok := c.RefChanges["refs/heads/main"]
				if !ok || ch.Before == "" || ch.After == "" {
					t.Errorf("RefChanges = %+v, want refs/heads/main with a before and an after", c.RefChanges)
				}
			},
		},
		{
			name:   "ref added",
			mutate: func(a *SharedState) { a.Refs["refs/heads/side"] = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" },
			check: func(t *testing.T, c SharedChanges) {
				ch, ok := c.RefChanges["refs/heads/side"]
				if !ok || ch.Before != "" || ch.After == "" {
					t.Errorf("RefChanges = %+v, want an added refs/heads/side", c.RefChanges)
				}
			},
		},
		{
			name:   "ref deleted",
			mutate: func(a *SharedState) { delete(a.Refs, "refs/tags/v1") },
			check: func(t *testing.T, c SharedChanges) {
				ch, ok := c.RefChanges["refs/tags/v1"]
				if !ok || ch.Before == "" || ch.After != "" {
					t.Errorf("RefChanges = %+v, want a deleted refs/tags/v1", c.RefChanges)
				}
			},
		},
		{
			name:   "head detached",
			mutate: func(a *SharedState) { a.HeadRef = "" },
			check: func(t *testing.T, c SharedChanges) {
				if !c.HeadMoved {
					t.Error("a detached HEAD was not reported")
				}
			},
		},
		{
			name:   "head switched branch",
			mutate: func(a *SharedState) { a.HeadRef = "refs/heads/main" },
			check: func(t *testing.T, c SharedChanges) {
				if !c.HeadMoved {
					t.Error("HEAD moving to another branch was not reported")
				}
			},
		},
		{
			name: "changed names come back sorted",
			mutate: func(a *SharedState) {
				a.Info["zzz"] = "1"
				a.Info["aaa"] = "1"
				delete(a.Info, "exclude")
			},
			check: func(t *testing.T, c SharedChanges) {
				if want := []string{"aaa", "exclude", "zzz"}; strings.Join(c.InfoChanged, ",") != strings.Join(want, ",") {
					t.Errorf("InfoChanged = %v, want %v", c.InfoChanged, want)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := sharedStateFixture()
			after := sharedStateFixture()
			tc.mutate(&after)
			tc.check(t, before.Diff(after))
		})
	}
}

// gitFrom runs a git command the way an agent would: plain git, in the
// worktree, with no aidev flags.
func gitFrom(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// Each way an agent can reach out of its worktree must show up in the next
// snapshot — this is the detection layer the worker's CONTAINMENT failure
// classification reads (docs/research.md §7i).
func TestSnapshotSharedStateSeesMutationsFromInsideTheWorktree(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "TASK-000003-a1", Branch: "aidev/TASK-000003"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	snapshot := func() SharedState {
		t.Helper()
		state, err := wt.SnapshotSharedState(ctx)
		if err != nil {
			t.Fatalf("SnapshotSharedState: %v", err)
		}
		return state
	}
	writeShared := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	baseline := snapshot()
	if baseline.CommonDir == "" || baseline.HeadRef != "refs/heads/aidev/TASK-000003" {
		t.Fatalf("baseline = %+v, want a resolved gitdir and the worktree's own HEAD", baseline)
	}
	if baseline.Config == "" {
		t.Error("baseline Config is empty; a fresh repository has a config file, so an empty hash would hide its appearance")
	}
	if _, ok := baseline.Refs["refs/heads/main"]; !ok {
		t.Errorf("baseline Refs = %v, want refs/heads/main listed", baseline.Refs)
	}
	if len(baseline.Hooks) == 0 {
		t.Error("baseline Hooks is empty; git installs sample hooks, so an empty map would hide a new one")
	}

	tests := []struct {
		name  string
		do    func(t *testing.T)
		check func(t *testing.T, changes SharedChanges)
	}{
		{
			name: "config written from the worktree",
			do: func(t *testing.T) {
				gitFrom(t, wt.Path, "config", "aidev.probe", "shared")
			},
			check: func(t *testing.T, c SharedChanges) {
				if !c.ConfigChanged {
					t.Error("a shared config write was not detected")
				}
			},
		},
		{
			name: "info/attributes planted",
			do: func(t *testing.T) {
				writeShared(filepath.Join(baseline.CommonDir, "info", "attributes"), "* filter=probe\n")
			},
			check: func(t *testing.T, c SharedChanges) {
				if want := []string{"attributes"}; strings.Join(c.InfoChanged, ",") != strings.Join(want, ",") {
					t.Errorf("InfoChanged = %v, want %v", c.InfoChanged, want)
				}
			},
		},
		{
			name: "executable hook planted",
			do: func(t *testing.T) {
				writeShared(filepath.Join(baseline.CommonDir, "hooks", "post-commit"), "#!/bin/sh\nexit 0\n")
			},
			check: func(t *testing.T, c SharedChanges) {
				if want := []string{"post-commit"}; strings.Join(c.HooksChanged, ",") != strings.Join(want, ",") {
					t.Errorf("HooksChanged = %v, want %v", c.HooksChanged, want)
				}
			},
		},
		{
			name: "tag created",
			do: func(t *testing.T) {
				gitFrom(t, wt.Path, "tag", "probe-tag")
			},
			check: func(t *testing.T, c SharedChanges) {
				if ch, ok := c.RefChanges["refs/tags/probe-tag"]; !ok || ch.After == "" {
					t.Errorf("RefChanges = %+v, want refs/tags/probe-tag added", c.RefChanges)
				}
			},
		},
		{
			name: "foreign branch ref created",
			do: func(t *testing.T) {
				gitFrom(t, wt.Path, "update-ref", "refs/heads/probe-side", wt.BaseCommit)
			},
			check: func(t *testing.T, c SharedChanges) {
				if ch, ok := c.RefChanges["refs/heads/probe-side"]; !ok || ch.After == "" {
					t.Errorf("RefChanges = %+v, want refs/heads/probe-side added", c.RefChanges)
				}
			},
		},
		{
			name: "HEAD detached",
			do: func(t *testing.T) {
				gitFrom(t, wt.Path, "checkout", "--detach")
			},
			check: func(t *testing.T, c SharedChanges) {
				if !c.HeadMoved {
					t.Error("a detached HEAD was not detected")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshot()
			tc.do(t)
			tc.check(t, before.Diff(snapshot()))
		})
	}
}

// The base commit must still be reachable from HEAD after the agent ran: a
// rewritten history makes every diff and verification against the base a
// statement about commits that no longer exist.
func TestBaseIsAncestorTracksHistoryUnderTheWorktree(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "TASK-000004-a1", Branch: "aidev/TASK-000004"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ok, err := wt.BaseIsAncestor(ctx)
	if err != nil {
		t.Fatalf("BaseIsAncestor on a fresh worktree: %v", err)
	}
	if !ok {
		t.Error("a worktree checked out at its base commit does not contain its base")
	}

	// An orphan branch is a root history: nothing the worktree was branched
	// from is an ancestor of it.
	gitFrom(t, wt.Path, "checkout", "--orphan", "probe-fresh")
	write(t, filepath.Join(wt.Path, "probe.txt"), "probe\n")
	gitFrom(t, wt.Path, "add", "probe.txt")
	gitFrom(t, wt.Path, "commit", "-qm", "orphan root")

	ok, err = wt.BaseIsAncestor(ctx)
	if err != nil {
		t.Fatalf("BaseIsAncestor after rewriting history: %v", err)
	}
	if ok {
		t.Error("the base commit is still an ancestor after HEAD moved to an unrelated root")
	}
}
