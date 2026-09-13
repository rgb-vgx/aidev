package verification

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"aidev/internal/task"
)

// Interception is one verification step whose runner can be loaded from a path
// that changed during the attempt. The judge the agent wrote is not
// independent evidence, however good the work is (docs/research.md 7e).
type Interception struct {
	StepIndex int
	Step      string
	Path      string
}

// Interceptions reports each step whose runner can be loaded from a changed
// path. Order is by StepIndex, then Path, so the report is stable.
func Interceptions(steps []task.VerificationStep, changed []string) []Interception {
	var out []Interception
	for i, s := range steps {
		for _, p := range matchesForStep(s, changed) {
			out = append(out, Interception{StepIndex: i, Step: s.String(), Path: p})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].StepIndex != out[b].StepIndex {
			return out[a].StepIndex < out[b].StepIndex
		}
		return out[a].Path < out[b].Path
	})
	return out
}

func matchesForStep(step task.VerificationStep, changed []string) []string {
	base := path.Base(step.Command)
	if isPython(base) {
		return pythonMatches(step, changed)
	}
	if isShell(base) {
		runner, ok := shellScript(step.Args)
		if !ok {
			return nil
		}
		return matchRunner(runner, changed)
	}
	// A bare name is looked up in PATH only, and an absolute path is resolved
	// outside the worktree; neither can load the agent's files.
	if !strings.Contains(step.Command, "/") {
		return nil
	}
	if filepath.IsAbs(step.Command) || path.IsAbs(step.Command) {
		return nil
	}
	runner, ok := cleanRunner(step.Command)
	if !ok {
		return nil
	}
	return matchRunner(runner, changed)
}

// matchRunner returns the changed entries the runner loads: an exact entry, or
// a directory entry ending in "/" standing for everything beneath it.
func matchRunner(runner string, changed []string) []string {
	var out []string
	for _, c := range changed {
		if c == runner {
			out = append(out, c)
		} else if strings.HasSuffix(c, "/") && strings.HasPrefix(runner, c) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func cleanRunner(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if filepath.IsAbs(p) || path.IsAbs(p) {
		return "", false
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned == "." || cleaned == "" {
		return "", false
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

func isShell(base string) bool {
	switch base {
	case "sh", "bash", "dash", "zsh":
		return true
	}
	return false
}

func isPython(base string) bool {
	if base == "python" || base == "python3" {
		return true
	}
	if !strings.HasPrefix(base, "python3.") {
		return false
	}
	rest := strings.TrimPrefix(base, "python3.")
	if rest == "" {
		return false
	}
	for _, part := range strings.Split(rest, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// shellScript returns the script a shell interpreter is handed: the first
// positional argument. Flags are skipped; -o takes a value; after -c there is
// a command string, not a file.
func shellScript(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 >= len(args) {
				return "", false
			}
			return cleanRunner(args[i+1])
		}
		if a == "-c" {
			return "", false
		}
		if a == "-o" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		return cleanRunner(a)
	}
	return "", false
}

func pythonMatches(step task.VerificationStep, changed []string) []string {
	args := step.Args
	// Whether -P or -I appeared among python's own flags, before -m. Either
	// keeps the working directory off sys.path, so the worktree cannot shadow
	// the module (docs/research.md 7e).
	sawIsolated := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 >= len(args) {
				return nil
			}
			runner, ok := cleanRunner(args[i+1])
			if !ok {
				return nil
			}
			return matchRunner(runner, changed)
		}
		if a == "-c" || (len(a) > 2 && strings.HasPrefix(a, "-c")) {
			return nil
		}
		if a == "-m" {
			if i+1 >= len(args) {
				return nil
			}
			if sawIsolated {
				return nil
			}
			return matchModule(args[i+1], changed)
		}
		if len(a) > 2 && strings.HasPrefix(a, "-m") && !strings.HasPrefix(a, "--") {
			if sawIsolated {
				return nil
			}
			return matchModule(a[2:], changed)
		}
		if a == "-W" || a == "-X" {
			i++
			continue
		}
		if (strings.HasPrefix(a, "-W") || strings.HasPrefix(a, "-X")) && len(a) > 2 && !strings.HasPrefix(a, "--") {
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" && !strings.HasPrefix(a, "--") {
			if strings.Contains(a[1:], "I") || strings.Contains(a[1:], "P") {
				sawIsolated = true
			}
			continue
		}
		if strings.HasPrefix(a, "--") {
			// Long options take no module meaning here; --isolated is not a
			// spelling the measured rule covers.
			continue
		}
		runner, ok := cleanRunner(a)
		if !ok {
			return nil
		}
		return matchRunner(runner, changed)
	}
	return nil
}

// matchModule reports changed paths that can shadow MODULE loaded with
// python -m: only the top level of the working directory is searched, through
// the top-level name T (MODULE up to the first dot). A first segment equal to
// T covers a package directory; one starting with "T." covers a source,
// sourceless or extension module.
func matchModule(module string, changed []string) []string {
	if module == "" {
		return nil
	}
	top := module
	if i := strings.IndexByte(top, '.'); i >= 0 {
		top = top[:i]
	}
	if top == "" {
		return nil
	}
	var out []string
	for _, c := range changed {
		first := c
		if i := strings.IndexByte(c, '/'); i >= 0 {
			first = c[:i]
		}
		if first == top || strings.HasPrefix(first, top+".") {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}
