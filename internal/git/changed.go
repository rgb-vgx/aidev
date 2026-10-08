package git

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Changes is what an attempt changed since the base commit, split by whether a
// commit of the worktree would carry it.
//
// The split exists because the two halves answer different questions. Whether
// a verification runner was replaced must see ignored files too — a runner
// shadowed by an ignored file is exactly what a diff misses (docs/research.md
// 7e). Whether the agent touched a protected path, or edited a test, is a
// question about the work, and git never carries an ignored file into it: a
// __pycache__ or a report the checks wrote is not something the agent changed
// (TASK-000093).
type Changes struct {
	// Committable are the paths a commit of the worktree would carry: tracked
	// files modified, deleted or committed, and new files git does not ignore.
	Committable []string
	// Ignored are files and directories git ignores, as `git ls-files
	// --ignored --directory` reports them (a wholly ignored directory once,
	// with a trailing slash).
	Ignored []string
}

// All is every changed path, sorted and without duplicates.
func (c Changes) All() []string {
	return mergeSorted(c.Committable, c.Ignored)
}

// ChangedPaths reports every path that differs between the base commit and the
// files on disk, so verification can refuse to run a runner the agent wrote and
// check the work against the paths a task protects.
//
// A diff against the index is blind to work the agent committed (the index
// then matches the files) and to ignored files (never in the index at all),
// which are exactly the places a replacement runner would go unseen
// (docs/research.md 7e). Comparing against the base commit and asking git for
// ignored files covers both.
func (w *Worktree) ChangedPaths(ctx context.Context) (Changes, error) {
	if strings.TrimSpace(w.BaseCommit) == "" {
		return Changes{}, fmt.Errorf("changed paths in %s: no base commit recorded", w.Path)
	}

	tempIndex, cleanup, err := w.temporaryIndex()
	if err != nil {
		return Changes{}, err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tempIndex}

	if res, err := w.m.run(ctx, w.Path, env, "add", "-N", "--", "."); err != nil {
		return Changes{}, err
	} else if !res.Succeeded() {
		return Changes{}, fmt.Errorf("stage intent-to-add in %s: %s", w.Path, firstLine(res.Stderr))
	}

	// The same diff-driver caveats as Worktree.Diff apply: reading the file
	// list must not execute code the agent installed in the shared repository.
	diffRes, err := w.m.run(ctx, w.Path, env,
		"diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", w.BaseCommit)
	if err != nil {
		return Changes{}, err
	}
	if !diffRes.Succeeded() {
		return Changes{}, fmt.Errorf("git diff in %s: %s", w.Path, firstLine(diffRes.Stderr))
	}
	// A short list is worse than no list: interception decides what aidev must not
	// run from it, so a path lost to the output cap would let a changed runner judge
	// the agent that changed it.
	if diffRes.StdoutTruncated {
		return Changes{}, fmt.Errorf("changed paths in %s: git diff produced more output than aidev captures, "+
			"so the list of changed files is truncated and cannot be trusted", w.Path)
	}

	lsRes, err := w.m.run(ctx, w.Path, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return Changes{}, err
	}
	if !lsRes.Succeeded() {
		return Changes{}, fmt.Errorf("git ls-files in %s: %s", w.Path, firstLine(lsRes.Stderr))
	}
	if lsRes.StdoutTruncated {
		return Changes{}, fmt.Errorf("changed paths in %s: git ls-files produced more output than aidev captures, "+
			"so the list of ignored files is truncated and cannot be trusted", w.Path)
	}

	return Changes{
		Committable: mergeSorted(splitNul(diffRes.Stdout)),
		Ignored:     mergeSorted(splitNul(lsRes.Stdout)),
	}, nil
}

// mergeSorted joins path lists into one sorted list without empty entries or
// duplicates.
func mergeSorted(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, list := range lists {
		for _, p := range list {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

func splitNul(s string) []string {
	return strings.Split(s, "\x00")
}
