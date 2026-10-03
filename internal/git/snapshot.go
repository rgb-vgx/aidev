package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SharedState is a snapshot of the state a linked worktree shares with the main
// repository: everything under the shared .git that is not the worktree's own
// working tree or its private administrative files (docs/research.md §7i). An
// agent inside the worktree can write all of it, and parts of it — config,
// hooks, info/attributes — are a delayed command-execution channel into every
// later git command in the repository, including the operator's own checkout.
//
// Snapshotting before the agent runs and comparing afterwards is layer 2 of the
// containment defence. Layer 1 (Manager.run) disables the execution vectors for
// the git commands aidev itself starts; this layer detects the edits themselves,
// whether or not they would have executed.
type SharedState struct {
	// CommonDir is the resolved path of the shared administrative directory.
	// The worktree's .git file points at it, so redirecting that file moves
	// this path even when the replacement mirrors everything else.
	CommonDir string

	// Refs maps every ref name to the object it currently points at.
	Refs map[string]string

	// HeadRef is the symbolic ref HEAD resolves to (refs/heads/...), or the
	// empty string when HEAD is detached.
	HeadRef string

	// Config is a content hash of the shared config file, empty when the file
	// is absent. A linked worktree has no local config of its own: a plain
	// `git config` from inside it lands here (measured, research §7i).
	Config string

	// Info hashes each file in the shared info directory by name. That
	// directory holds info/attributes, which can attach an executable filter
	// to every path, and info/exclude, which hides paths from status.
	Info map[string]string

	// Hooks hashes each file in the default shared hooks directory (common
	// dir + /hooks) by name to a digest of its mode and content, so granting
	// the executable bit counts as much as replacing the text. A
	// core.hooksPath override moves the effective directory by editing config,
	// which Config covers.
	Hooks map[string]string
}

// SnapshotSharedState reads the state this worktree shares with the main
// repository. Every command it runs is read-only; nothing here writes, updates
// or checks out.
func (w *Worktree) SnapshotSharedState(ctx context.Context) (SharedState, error) {
	// measured: absolute when run from a linked worktree (research §7i). The
	// hooks directory is derived from this path rather than asked of
	// `git rev-parse --git-path hooks`, because layer 1 runs every aidev
	// command with core.hooksPath=/dev/null and --git-path would faithfully
	// report /dev/null. An agent moving the effective hooksPath writes it into
	// config, which the Config hash catches; the default directory is what
	// gets hashed here.
	commonRes, err := w.m.run(ctx, w.Path, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return SharedState{}, err
	}
	if !commonRes.Succeeded() || commonRes.StdoutTruncated {
		return SharedState{}, fmt.Errorf("resolve shared git dir in %s: %s", w.Path, firstLine(commonRes.Stderr))
	}
	lines := linesOf(commonRes.Stdout)
	if len(lines) != 1 {
		return SharedState{}, fmt.Errorf("resolve shared git dir in %s: expected 1 line, got %d", w.Path, len(lines))
	}
	commonDir := resolvePath(w.Path, lines[0])
	hooksDir := filepath.Join(commonDir, "hooks")

	// for-each-ref reports the logical refs, so loose-versus-packed churn is
	// not a change while a moved ref is.
	refsRes, err := w.m.run(ctx, w.Path, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return SharedState{}, err
	}
	if !refsRes.Succeeded() || refsRes.StdoutTruncated {
		return SharedState{}, fmt.Errorf("list refs in %s: %s", w.Path, firstLine(refsRes.Stderr))
	}
	refs := map[string]string{}
	for _, line := range linesOf(refsRes.Stdout) {
		name, object, ok := strings.Cut(line, " ")
		if !ok || name == "" {
			return SharedState{}, fmt.Errorf("list refs in %s: unexpected line %q", w.Path, line)
		}
		refs[name] = object
	}

	// -q turns "HEAD is not a symbolic ref" into exit 1 with no output —
	// measured on git 2.43: detached HEAD exits 1 silently, not the fatal the
	// flag suppresses.
	headRes, err := w.m.run(ctx, w.Path, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return SharedState{}, err
	}
	headRef := ""
	switch {
	case headRes.Succeeded():
		headRef = strings.TrimSpace(headRes.Stdout)
	case headRes.ExitCode != nil && *headRes.ExitCode == 1:
		// Detached HEAD, recorded as the empty ref.
	default:
		return SharedState{}, fmt.Errorf("read HEAD in %s: %s", w.Path, firstLine(headRes.Stderr))
	}

	config, err := hashFile(filepath.Join(commonDir, "config"))
	if err != nil {
		return SharedState{}, err
	}
	info, err := hashFileTree(filepath.Join(commonDir, "info"), false)
	if err != nil {
		return SharedState{}, err
	}
	hooks, err := hashFileTree(hooksDir, true)
	if err != nil {
		return SharedState{}, err
	}

	return SharedState{
		CommonDir: commonDir,
		Refs:      refs,
		HeadRef:   headRef,
		Config:    config,
		Info:      info,
		Hooks:     hooks,
	}, nil
}

// BaseIsAncestor reports whether the commit this worktree was created from is
// still an ancestor of its HEAD — that the branch still contains the work it
// was branched from, so a diff against the base means something. false is a
// result (history was rewritten under it); an error means git could not answer,
// for example because the object is gone, which is a failure to check rather
// than a check that passed.
func (w *Worktree) BaseIsAncestor(ctx context.Context) (bool, error) {
	// measured: exit 0 = ancestor, 1 = not, 128 = error such as a missing
	// object (research §7i).
	res, err := w.m.run(ctx, w.Path, nil, "merge-base", "--is-ancestor", w.BaseCommit, "HEAD")
	if err != nil {
		return false, err
	}
	if res.ExitCode == nil {
		return false, fmt.Errorf("merge-base --is-ancestor in %s: %s", w.Path, firstLine(res.Stderr))
	}
	switch *res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("merge-base --is-ancestor %s in %s: %s", w.BaseCommit, w.Path, firstLine(res.Stderr))
	}
}

// RefChange is one ref's movement between two snapshots. Before is empty for a
// ref that appeared, After for one that disappeared.
type RefChange struct {
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// SharedChanges is the difference between two SharedState snapshots, computed
// by Diff. The slices are sorted, so both event payloads and tests see a stable
// order.
type SharedChanges struct {
	CommonDirMoved bool
	ConfigChanged  bool
	InfoChanged    []string
	HooksChanged   []string
	RefChanges     map[string]RefChange
	HeadMoved      bool
}

// Diff reports what changed between two snapshots. It is pure: no git, no
// filesystem, which is what lets the containment policy be tested without a
// repository.
func (b SharedState) Diff(after SharedState) SharedChanges {
	out := SharedChanges{
		CommonDirMoved: b.CommonDir != after.CommonDir,
		ConfigChanged:  b.Config != after.Config,
		HeadMoved:      b.HeadRef != after.HeadRef,
		RefChanges:     map[string]RefChange{},
	}
	out.InfoChanged = changedKeys(b.Info, after.Info)
	out.HooksChanged = changedKeys(b.Hooks, after.Hooks)

	for ref, before := range b.Refs {
		now, ok := after.Refs[ref]
		switch {
		case !ok:
			out.RefChanges[ref] = RefChange{Before: before}
		case now != before:
			out.RefChanges[ref] = RefChange{Before: before, After: now}
		}
	}
	for ref, now := range after.Refs {
		if _, ok := b.Refs[ref]; !ok {
			out.RefChanges[ref] = RefChange{After: now}
		}
	}
	return out
}

// Empty reports whether two snapshots are identical.
func (c SharedChanges) Empty() bool {
	return !c.CommonDirMoved && !c.ConfigChanged && !c.HeadMoved &&
		len(c.InfoChanged) == 0 && len(c.HooksChanged) == 0 && len(c.RefChanges) == 0
}

// changedKeys returns the names whose value differs or that exist on only one
// side, sorted.
func changedKeys(before, after map[string]string) []string {
	var out []string
	for name, was := range before {
		if now, ok := after[name]; !ok || now != was {
			out = append(out, name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// linesOf splits command stdout into lines, dropping the trailing newline that
// git always writes after the last one. Empty output is no lines, not one.
func linesOf(out string) []string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// resolvePath interprets a path git printed while running in dir: absolute
// paths pass through, relative ones (which git emits from a plain repository)
// belong to the directory the command ran in.
func resolvePath(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// hashFile returns a hex sha256 of the file's content, or "" when the file does
// not exist. "" and the hash of empty content are different values, so a file
// appearing is a change like any other.
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// hashFileTree hashes every non-directory entry of dir by name. Directories are
// skipped: git reads hooks and info as a flat directory of files. With
// withMode, the file's mode is hashed too, so an executable bit flip is a
// change; an entry that cannot be read hashes as a marker rather than failing
// the snapshot, because "we could not see it" must still compare as different
// from content we could.
func hashFileTree(dir string, withMode bool) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read directory %s: %w", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		full := filepath.Join(dir, entry.Name())
		sum := sha256.New()
		if withMode {
			info, err := os.Lstat(full)
			if err != nil {
				return nil, fmt.Errorf("stat %s: %w", full, err)
			}
			fmt.Fprintf(sum, "%v\x00", info.Mode())
		}
		data, err := os.ReadFile(full)
		if err != nil {
			data = []byte("unreadable")
		}
		sum.Write(data)
		out[entry.Name()] = hex.EncodeToString(sum.Sum(nil))
	}
	return out, nil
}
