package coverage

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aidev/internal/procexec"
)

// Options says where and how to measure.
type Options struct {
	// Dir is the checkout the tests run in: the clean checkout in clean
	// mode, the agent's worktree otherwise. Paths in Diff are relative to it.
	Dir string
	// Diff is `git diff -U0` from the base commit to the attempt's tree.
	Diff string
	// Timeout bounds each `go test` run; MaxOutputBytes its captured output.
	Timeout        time.Duration
	MaxOutputBytes int
}

// Analyze runs `go test -coverprofile` for the packages holding the changed
// non-test Go files, module by module, and compares the profile with the
// changed lines. A change with no Go source returns an empty report. An error
// means the measurement could not be made; it is never a verdict on the work.
func Analyze(ctx context.Context, opts Options) (Report, error) {
	added := map[string][]int{}
	for file, lines := range AddedLines(opts.Diff) {
		if strings.HasSuffix(file, ".go") && !strings.HasSuffix(file, "_test.go") {
			added[file] = lines
		}
	}
	if len(added) == 0 {
		return Report{}, nil
	}

	// Group the changed files by the module that owns them: a repository
	// may hold several (a core-go/ beside other languages, say).
	type module struct {
		dir  string // relative to opts.Dir; "." for the root
		path string // the module path go.mod declares
		pkgs map[string]bool
	}
	modules := map[string]*module{}
	for file := range added {
		dir, modPath, ok := owningModule(opts.Dir, path.Dir(file))
		if !ok {
			continue
		}
		m := modules[dir]
		if m == nil {
			m = &module{dir: dir, path: modPath, pkgs: map[string]bool{}}
			modules[dir] = m
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path.Dir(file), dir), "/")
		if dir == "." {
			rel = path.Dir(file)
		}
		pkg := "./" + rel
		if rel == "." || rel == "" {
			pkg = "."
		}
		m.pkgs[pkg] = true
	}

	var blocks []Block
	dirs := make([]string, 0, len(modules))
	for d := range modules {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		m := modules[d]
		got, err := profileModule(ctx, opts, m.dir, sortedKeys(m.pkgs))
		if err != nil {
			return Report{}, err
		}
		// The profile names files by import path; the diff by path in the
		// repository. Map one onto the other.
		prefix := m.path + "/"
		for _, b := range got {
			rest, ok := strings.CutPrefix(b.File, prefix)
			if !ok {
				continue
			}
			if m.dir != "." {
				rest = m.dir + "/" + rest
			}
			b.File = rest
			blocks = append(blocks, b)
		}
	}
	return Compare(added, blocks), nil
}

// owningModule finds the nearest go.mod at or above dir (relative to root,
// never above it) and returns its directory and module path.
func owningModule(root, dir string) (string, string, bool) {
	for {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "go.mod"))
		if err == nil {
			if mp := ModulePath(string(raw)); mp != "" {
				return dir, mp, true
			}
			return "", "", false
		}
		if dir == "." || dir == "/" || dir == "" {
			return "", "", false
		}
		dir = path.Dir(dir)
	}
}

// profileModule runs the module's tests for pkgs with a coverage profile and
// returns its blocks.
func profileModule(ctx context.Context, opts Options, dir string, pkgs []string) ([]Block, error) {
	tmp, err := os.MkdirTemp("", "aidev-coverage-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "cover.out")

	args := append([]string{"test", "-count=1", "-coverprofile=" + out}, pkgs...)
	proc, err := procexec.Run(ctx, procexec.Spec{
		Command:        "go",
		Args:           args,
		Dir:            filepath.Join(opts.Dir, filepath.FromSlash(dir)),
		Timeout:        opts.Timeout,
		MaxOutputBytes: opts.MaxOutputBytes,
		// The tests are the task's; they must not see aidev's configuration,
		// and an application a test starts goes when the run does.
		DropEnv:        []string{"AIDEV_"},
		FollowSessions: true,
	})
	if err != nil {
		return nil, fmt.Errorf("go test for coverage in %s: %w", dir, err)
	}
	if !proc.Succeeded() {
		return nil, fmt.Errorf("go test for coverage in %s failed: %s", dir, lastLine(proc.Stdout+proc.Stderr))
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("go test for coverage in %s wrote no profile: %w", dir, err)
	}
	return ParseProfile(string(raw))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
