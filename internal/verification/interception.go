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
	var out []string

	// A command given as a relative path is a file in the worktree whatever it
	// is — a script, or an interpreter such as ./.venv/bin/python. A fresh
	// worktree has no virtualenv, so one that exists was made during the attempt.
	if runner, ok := relativeCommand(step.Command); ok {
		out = append(out, matchRunner(runner, changed)...)
	}

	base := path.Base(filepath.ToSlash(step.Command))
	switch {
	case isPython(base):
		out = append(out, pythonMatches(step.Args, changed)...)
	case isShell(base):
		if script, ok := shellScript(step.Args); ok {
			out = append(out, matchRunner(script, changed)...)
		}
	}
	return uniqueSorted(out)
}

// relativeCommand returns the worktree path of a command given as a relative
// path. A bare name is looked up in PATH only, and an absolute path is resolved
// outside the worktree; neither can load the agent's files.
func relativeCommand(command string) (string, bool) {
	if !strings.Contains(command, "/") {
		return "", false
	}
	return cleanRunner(command)
}

// matchRunner returns the changed entries the runner loads: an exact entry, or
// a directory entry ending in "/" standing for everything beneath it.
func matchRunner(runner string, changed []string) []string {
	var out []string
	for _, c := range changed {
		if c == runner || (strings.HasSuffix(c, "/") && strings.HasPrefix(runner, c)) {
			out = append(out, c)
		}
	}
	return out
}

func cleanRunner(p string) (string, bool) {
	if p == "" || p == "-" {
		return "", false
	}
	if filepath.IsAbs(p) || path.IsAbs(p) {
		return "", false
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
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
	rest, ok := strings.CutPrefix(base, "python3.")
	if !ok || rest == "" {
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

// shellScript returns the script a shell is handed: its first operand. Options
// come in clusters (-ex); -o and +o take the next argument; a cluster containing
// c means the operand is a command string, not a file.
func shellScript(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return cleanRunner(args[i+1])
			}
			return "", false
		case strings.HasPrefix(a, "--"):
			continue
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'c':
					return "", false
				case 'o':
					if j == len(a)-1 {
						i++
					}
				}
			}
		default:
			return cleanRunner(a)
		}
	}
	return "", false
}

// pythonMatches follows python's own option parsing far enough to find what it
// loads from the working directory: the module of -m, or the script operand.
// Short options come in clusters, and m, c, W and X take a value — the rest of
// the cluster, or the next argument (measured: -Bm and -BW error are parsed
// that way; docs/research.md 7e).
func pythonMatches(args []string, changed []string) []string {
	isolated := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				if script, ok := cleanRunner(args[i+1]); ok {
					return matchRunner(script, changed)
				}
			}
			return nil
		case strings.HasPrefix(a, "--"):
			// No long option names a module or a script.
			continue
		case len(a) > 1 && a[0] == '-':
			cluster := a[1:]
			for j := 0; j < len(cluster); j++ {
				switch cluster[j] {
				case 'I', 'P':
					// Either keeps the working directory off sys.path.
					isolated = true
				case 'c':
					// Code from the command line: there is no file to load, and
					// its imports cannot be seen from here.
					return nil
				case 'm':
					module := cluster[j+1:]
					if module == "" {
						if i+1 >= len(args) {
							return nil
						}
						module = args[i+1]
					}
					if isolated {
						return nil
					}
					return matchModule(module, changed)
				case 'W', 'X':
					if j == len(cluster)-1 {
						i++
					}
					j = len(cluster)
				}
			}
		default:
			if script, ok := cleanRunner(a); ok {
				return matchRunner(script, changed)
			}
			return nil
		}
	}
	return nil
}

// matchModule reports changed paths that can shadow MODULE loaded with
// python -m: only the top level of the working directory is searched, through
// the top-level name T (MODULE up to the first dot). A first segment equal to
// T covers a package directory; one starting with "T." covers a source,
// sourceless or extension module.
func matchModule(module string, changed []string) []string {
	top, _, _ := strings.Cut(module, ".")
	if top == "" {
		return nil
	}
	var out []string
	for _, c := range changed {
		first, _, _ := strings.Cut(c, "/")
		if first == top || strings.HasPrefix(first, top+".") {
			out = append(out, c)
		}
	}
	return out
}
