package verification

import (
	"path"
	"path/filepath"
	"strings"
)

// This file models the test runners a verification step can invoke by name: go
// test, node --test, and pytest. interception.go models a command that loads a
// file directly — a relative path, a python script operand, `python -m
// MODULE`, a shell script — but a runner invoked by name loads no such file;
// it collects the test files its arguments select and executes those. A
// changed test file it would execute is the judge the agent wrote, like a
// shadowed module, so it is reported as an interception.
//
// Selection errs wide on purpose: getting it wrong narrow is the silent
// failure (the edited test judges the work), getting it wrong wide is a loud
// refusal naming the path. Where the argument parsing is genuinely ambiguous,
// the code below says so.

// cleanOperand normalises a runner operand — a file or directory a step names
// — to a worktree-relative path. An absolute path, or one escaping the
// worktree, selects nothing in it.
func cleanOperand(a string) (string, bool) {
	if a == "" || a == "-" {
		return "", false
	}
	if filepath.IsAbs(a) || path.IsAbs(a) {
		return "", false
	}
	cleaned := filepath.ToSlash(filepath.Clean(a))
	if cleaned == "." {
		return ".", true
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

// underOrEqual reports whether p is dir or beneath it. "." names the worktree
// root, so everything is beneath it.
func underOrEqual(p, dir string) bool {
	if dir == "." {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// testSources returns the changed entries, in the form the event payload
// reports them, that the refusal guard may judge: test sources, never the
// fixtures or the Makefile beside them (isTestSource).
func testSources(changed []string) []string {
	var out []string
	for _, c := range changed {
		norm, ok := cleanOperand(c)
		if !ok {
			continue
		}
		if isTestSource(norm) {
			out = append(out, c)
		}
	}
	return out
}

// goMatches reports the changed test files a `go test` step would execute.
// The command is go whose first non-flag operand is test: it runs "the
// packages named by the import paths", compiling each package's `*_test.go`
// files into the binary it runs, and with no package arguments it compiles
// "the package sources and tests found in the current directory" (measured:
// `go help test`, go1.26.8). `go build` and `go vet` take the same package
// operands but execute no test binary, so the subcommand has to be test.
func goMatches(args []string, changed []string) []string {
	sub, rest := goSubcommand(args)
	if sub != "test" {
		return nil
	}
	patterns := goPatterns(rest)
	var out []string
	for _, c := range testSources(changed) {
		norm, _ := cleanOperand(c)
		if !strings.HasSuffix(path.Base(norm), "_test.go") {
			continue
		}
		if goSelected(patterns, path.Dir(norm)) {
			out = append(out, c)
		}
	}
	return out
}

// goSubcommand returns go's subcommand — its first non-flag operand — and the
// arguments past it. The -C flag takes the directory as its value (measured:
// `go -C` in `go help`, go1.26.8), so it is skipped with the next argument;
// anything else starting with - is a flag.
func goSubcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return "", nil
		case a == "-C":
			i++
		case a == "-" || strings.HasPrefix(a, "-"):
			continue
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

// goValueFlags names the go test flags that take a separate value (measured:
// `go help test` and `go help testflag`, go1.26.8: -run, -skip, -count,
// -timeout, -cpu, -parallel and the profiles among them). A value glued with =
// needs no skipping. Anything else starting with - is assumed boolean: an
// unknown option fails loudly at runtime (measured: `go list -run` answers
// "flag provided but not defined"), so mistaking one cannot silently pass.
var goValueFlags = map[string]bool{
	"run": true, "skip": true, "count": true, "timeout": true, "cpu": true,
	"parallel": true, "tags": true, "ldflags": true, "exec": true,
	"bench": true, "benchtime": true, "coverprofile": true, "covermode": true,
	"coverpkg": true, "outputdir": true, "cpuprofile": true, "memprofile": true,
	"memprofilerate": true, "blockprofile": true, "blockprofilerate": true,
	"mutexprofile": true, "mutexprofilefraction": true, "trace": true,
	"o": true, "list": true, "vet": true, "shuffle": true,
	"gcflags": true, "asmflags": true, "overlay": true, "toolexec": true,
}

// goPatterns returns the package patterns among the arguments past the test
// subcommand: non-flag operands, with the values of value-taking flags
// skipped. Everything past -args goes to the test binary uninterpreted, and
// the package list must precede it (measured: `go help test`), so parsing
// stops there.
func goPatterns(args []string) []string {
	var patterns []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for _, o := range args[i+1:] {
				if p, ok := cleanOperand(o); ok {
					patterns = append(patterns, p)
				}
			}
			return patterns
		case a == "-args":
			return patterns
		case a == "-" || strings.HasPrefix(a, "-"):
			name := strings.TrimLeft(a, "-")
			name, _, _ = strings.Cut(name, "=")
			if _, _, glued := strings.Cut(a, "="); !glued && goValueFlags[name] {
				i++
			}
		default:
			if p, ok := cleanOperand(a); ok {
				patterns = append(patterns, p)
			}
		}
	}
	return patterns
}

// goSelected reports whether a changed test file's directory is among the
// packages the step runs. With no patterns the step runs the package in the
// working directory (measured: `go help test`: "go test" with no package
// arguments is local directory mode).
func goSelected(patterns []string, dir string) bool {
	if len(patterns) == 0 {
		return dir == "."
	}
	for _, p := range patterns {
		if goPatternSelects(p, dir) {
			return true
		}
	}
	return false
}

// goPatternSelects reports whether one package pattern selects dir. Relative
// patterns are filesystem paths (measured: `go help packages`: an import path
// beginning with . is a file system path, and `go list ./internal/task/...`
// expands to the packages beneath); a trailing /... matches the directory
// itself too (measured: `go help packages`: "/... at the end of the pattern
// can match an empty string"). Anything unresolvable from the file list — a
// module-qualified path, `all`, a wildcard outside a trailing /... — selects
// broadly rather than silently, except the reserved names that name no
// worktree package.
func goPatternSelects(pattern, dir string) bool {
	p := strings.TrimPrefix(pattern, "./")
	switch {
	case p == "...":
		return true
	case strings.HasSuffix(p, "/..."):
		prefix := strings.TrimSuffix(p, "/...")
		return dir == prefix || strings.HasPrefix(dir, prefix+"/")
	case strings.Contains(p, "..."):
		return true
	case p == "all":
		return true
	case strings.Contains(p, "/"):
		if strings.HasPrefix(pattern, ".") {
			return dir == p
		}
		return dir == p || strings.HasSuffix(p, "/"+dir)
	case p == ".":
		return dir == "."
	case p == "std" || p == "cmd" || p == "tool":
		return false
	default:
		// A bare word reaches here only as an unknown flag's value, which
		// fails loudly at runtime, or as a package no verification step
		// names bare; only the working directory itself can be meant.
		return dir == "."
	}
}

// nodeMatches reports the changed test files a `node --test` step would run.
// The command is node with --test among its arguments (measured: `node
// --help` lists --test, v22.23.2); its operands are the files or directories
// to run, and with none the runner searches the working directory recursively
// (measured: `node --test` ran tests/x.test.mjs and left tests/plain.mjs
// alone, v22.23.2). A named file is executed whatever it holds (measured: the
// same node ran an explicitly named tests/plain.mjs and failed on its
// contents), so selection is by path, not by test-name pattern: a changed test
// source counts when it is the operand or beneath a directory operand.
func nodeMatches(args []string, changed []string) []string {
	if !hasNodeTest(args) {
		return nil
	}
	operands, hadOperands := nodeOperands(args)
	var out []string
	for _, c := range testSources(changed) {
		norm, _ := cleanOperand(c)
		if !hadOperands {
			out = append(out, c)
			continue
		}
		for _, o := range operands {
			if nodeOperandSelects(o, norm) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func hasNodeTest(args []string) bool {
	for _, a := range args {
		if a == "--test" || strings.HasPrefix(a, "--test=") {
			return true
		}
	}
	return false
}

// nodeValueFlags names the node options that take a separate value (measured:
// `node --help`, v22.23.2: --test-name-pattern, --test-reporter and the other
// --test-* options, --import, --require). Anything else starting with - is
// assumed boolean; node rejects an unknown option before running anything, so
// mistaking one cannot silently pass.
var nodeValueFlags = map[string]bool{
	"test-concurrency": true, "test-name-pattern": true, "test-reporter": true,
	"test-reporter-destination": true, "test-shard": true, "test-skip-pattern": true,
	"test-timeout": true, "test-coverage-include": true, "test-coverage-exclude": true,
	"test-coverage-lines": true, "test-coverage-functions": true,
	"test-coverage-branches": true, "import": true, "loader": true,
	"experimental-loader": true, "require": true, "r": true,
	"eval": true, "e": true, "print": true, "p": true, "title": true,
}

// nodeOperands returns the file or directory operands among the step's
// arguments. hadOperands tells whether any operand was named at all: with
// none, the runner searches the working directory, which is the widest
// selection, not the absence of one.
func nodeOperands(args []string) (operands []string, hadOperands bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for _, o := range args[i+1:] {
				hadOperands = true
				if p, ok := cleanOperand(o); ok {
					operands = append(operands, p)
				}
			}
			return operands, hadOperands
		case a == "-" || strings.HasPrefix(a, "-"):
			name := strings.TrimLeft(a, "-")
			name, _, _ = strings.Cut(name, "=")
			if _, _, glued := strings.Cut(a, "="); !glued && nodeValueFlags[name] {
				i++
			}
		default:
			hadOperands = true
			if p, ok := cleanOperand(a); ok {
				operands = append(operands, p)
			}
		}
	}
	return operands, hadOperands
}

// nodeOperandSelects reports whether the operand selects the changed path:
// the path itself, or anything beneath a directory operand. A quoted glob
// operand cannot be resolved without the file list, so its literal prefix
// selects broadly rather than silently ('tests/*.test.mjs' still starts with
// tests/).
func nodeOperandSelects(operand, changed string) bool {
	if i := strings.IndexAny(operand, "*?[{"); i >= 0 {
		head := operand[:i]
		if strings.HasSuffix(head, "/") {
			return underOrEqual(changed, path.Clean(head))
		}
		return underOrEqual(changed, path.Dir(head))
	}
	return underOrEqual(changed, operand)
}

// pytestMatches reports the changed test files a pytest step would collect.
// The usage line says it (measured: `pytest --help`, 9.1.1): `pytest
// [options] [file_or_dir] ...`. With no operands collection starts at the
// rootdir, so every test source counts; a conftest.py is loaded for the
// collected tests in its directory and below (measured: `pytest tests/`
// printed the root conftest.py's line, 9.1.1), so a changed conftest.py on
// the way to a collected test counts too.
func pytestMatches(args []string, changed []string) []string {
	operands, hadOperands := pytestOperands(args)
	noconftest := false
	for _, a := range args {
		if a == "--noconftest" {
			noconftest = true
		}
	}
	var out []string
	for _, c := range testSources(changed) {
		norm, _ := cleanOperand(c)
		if !hadOperands {
			out = append(out, c)
			continue
		}
		if path.Base(norm) == "conftest.py" {
			if !noconftest && conftestSelected(operands, path.Dir(norm)) {
				out = append(out, c)
			}
			continue
		}
		for _, o := range operands {
			if underOrEqual(norm, o) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// conftestSelected reports whether a conftest.py in dir is loaded by the
// collection the operands name. It applies to tests in dir and below, so it
// counts when an operand reaches into dir (a test beneath it is collected)
// or dir reaches an operand (the operand is collected from beneath it).
func conftestSelected(operands []string, dir string) bool {
	for _, o := range operands {
		if underOrEqual(o, dir) || underOrEqual(dir, o) {
			return true
		}
	}
	return false
}

// pytestValueFlags names the pytest options that take a separate value
// (measured: `pytest --help`, 9.1.1). --ignore and --deselect name paths but
// exclude rather than collect, so their values must not become operands; -k
// and -m take expressions, -o/-c/-p configuration. Short options cluster
// (measured: `-qs` collected everything, `-k test_x` selected), so a value
// flag ends a cluster with the rest of it or the next argument as its value.
// Anything else starting with - is assumed boolean: pytest rejects an unknown
// option before collecting anything, so mistaking one cannot silently pass.
var pytestValueFlags = map[string]bool{
	"k": true, "m": true, "o": true, "c": true, "p": true, "W": true, "r": true,
	"deselect": true, "ignore": true, "confcutdir": true, "rootdir": true,
	"junitxml": true, "junitprefix": true, "maxfail": true, "cache-show": true,
	"last-failed-no-failures": true, "lfnf": true, "capture": true, "tb": true,
	"color": true, "code-highlight": true, "pdbcls": true, "doctest-glob": true,
	"override-ini": true, "basetemp": true, "assert": true,
}

// pytestOperands returns the collection paths among the step's arguments.
// hadOperands tells whether any was named at all: with none, collection
// starts at the rootdir, which is the widest selection, not the absence of
// one.
func pytestOperands(args []string) (operands []string, hadOperands bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for _, o := range args[i+1:] {
				hadOperands = true
				if p, ok := cleanOperand(o); ok {
					operands = append(operands, p)
				}
			}
			return operands, hadOperands
		case strings.HasPrefix(a, "--"):
			name, _, _ := strings.Cut(a[2:], "=")
			if _, _, glued := strings.Cut(a, "="); !glued && pytestValueFlags[name] {
				i++
			}
		case len(a) > 1 && a[0] == '-':
			cluster := a[1:]
			for j := 0; j < len(cluster); j++ {
				if pytestValueFlags[string(cluster[j])] {
					// The value is the rest of the cluster (-kEXPR) or the
					// next argument (-k EXPR).
					if j == len(cluster)-1 {
						i++
					}
					break
				}
			}
		default:
			hadOperands = true
			if p, ok := cleanOperand(a); ok {
				operands = append(operands, p)
			}
		}
	}
	return operands, hadOperands
}
