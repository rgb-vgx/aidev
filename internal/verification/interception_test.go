package verification

import (
	"reflect"
	"testing"

	"aidev/internal/task"
)

// The rule under test: a verification step is only evidence if the program that
// judges — the script it names, the module `python -m` loads — was not written
// by the agent being judged. Some of those programs are loaded from the
// worktree, and the worktree is the agent's. Interceptions reports each step
// whose runner can be loaded from a path that changed during the attempt.
//
// It guards the judge, not the exam. Project code, tests, a Makefile or a
// conftest.py are the work under review, and changing them is what tasks are
// for. Whether the tests still test anything is the reviewer's question.
//
// Every rule below was measured, not assumed (docs/research.md 7e):
//   - exec.Command("mytool") with Dir set does not run ./mytool: a bare name is
//     looked up in PATH only.
//   - python3 -m X loads X.py, X.pyc, X.<ext>.so and X/ from the working
//     directory ahead of site-packages; -mX behaves the same.
//   - python3 -P, -I and -IP do not put the working directory on sys.path.
//   - only the top level of the working directory is searched, not subfolders.

func interceptedPaths(ins []Interception) []string {
	var out []string
	for _, in := range ins {
		out = append(out, in.Path)
	}
	return out
}

func TestInterceptions(t *testing.T) {
	cases := []struct {
		name    string
		step    task.VerificationStep
		changed []string
		want    []string
	}{
		// Resolved outside the worktree.
		{"bare command goes through PATH, never the worktree",
			step("pytest", "-q"), []string{"pytest", "pytest.py"}, nil},
		{"a Makefile is the project's, like its tests",
			step("make", "check"), []string{"Makefile"}, nil},
		{"absolute command",
			step("/usr/bin/env", "true"), []string{"usr/bin/env"}, nil},

		// A command given as a relative path is a file in the worktree.
		{"relative script",
			step("./check.sh"), []string{"check.sh", "other.txt"}, []string{"check.sh"}},
		{"script in a subdirectory",
			step("scripts/test.sh", "--fast"), []string{"scripts/test.sh"}, []string{"scripts/test.sh"}},
		{"script inside a changed directory entry",
			step("build/out/runner"), []string{"build/"}, []string{"build/"}},
		{"path is cleaned before comparing",
			step("./a/../check.sh"), []string{"check.sh"}, []string{"check.sh"}},
		{"path leaving the worktree is not the worktree's",
			step("../outside.sh"), []string{"outside.sh"}, nil},
		{"unchanged script",
			step("./check.sh"), []string{"main.go"}, nil},

		// python -m loads the module from the working directory first.
		{"python -m shadowed by a module file",
			step("python3", "-m", "pytest", "-q"), []string{"main.go", "pytest.py"}, []string{"pytest.py"}},
		{"python -m shadowed by a sourceless pyc",
			step("python3", "-m", "pytest"), []string{"pytest.pyc"}, []string{"pytest.pyc"}},
		{"python -m shadowed by an extension module",
			step("python3", "-m", "pytest"), []string{"pytest.cpython-312-x86_64-linux-gnu.so"}, []string{"pytest.cpython-312-x86_64-linux-gnu.so"}},
		{"python -m shadowed by a package directory",
			step("python3", "-m", "pytest"), []string{"pytest/__main__.py"}, []string{"pytest/__main__.py"}},
		{"python -m shadowed by a directory entry",
			step("python3", "-m", "pytest"), []string{"pytest/"}, []string{"pytest/"}},
		{"dotted module is found through its top-level package",
			step("python3", "-m", "pkg.tool"), []string{"pkg/tool.py"}, []string{"pkg/tool.py"}},
		{"dotted module shadowed by a top-level module file",
			step("python3", "-m", "pkg.tool"), []string{"pkg.py"}, []string{"pkg.py"}},
		{"module name joined to -m",
			step("python3", "-mpytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"any python interpreter, by name or path",
			step("/usr/bin/python3.12", "-m", "pytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"plain python",
			step("python", "-m", "pytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		// A free-threaded build installs as python3.13t (PEP 703, shipped since
		// 3.13) and loads -m modules from the working directory exactly like the
		// GIL build: measured with a symlink named python3.12t, which printed the
		// module it found in the working directory. A name aidev fails to
		// recognise as python skips the -m rules, so a changed pytest.py judges
		// the agent that wrote it — interception fails open, the one direction
		// the feature exists to prevent.
		{"free-threaded interpreter",
			step("python3.13t", "-m", "pytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"free-threaded interpreter by path",
			step("/usr/local/bin/python3.14t", "-m", "pytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"a relative free-threaded interpreter is also its own runner",
			step("venv/bin/python3.13t", "-m", "pytest"), []string{"venv/"}, []string{"venv/"}},
		{"t is a suffix, not a version part",
			step("python3.t", "-m", "pytest"), []string{"pytest.py"}, nil},
		{"names that only resemble the module are not it",
			step("python3", "-m", "pytest"), []string{".pytest_cache/", "pytest_plugin.py", "pytests.py", "sub/pytest.py"}, nil},
		{"-P keeps the working directory off sys.path",
			step("python3", "-P", "-m", "pytest"), []string{"pytest.py"}, nil},
		{"-I keeps the working directory off sys.path",
			step("python3", "-I", "-m", "pytest"), []string{"pytest.py"}, nil},
		{"-IP combined",
			step("python3", "-IP", "-m", "pytest"), []string{"pytest.py"}, nil},
		{"-P after the module belongs to the module, not to python",
			step("python3", "-m", "pytest", "-P"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"-m without a module does not panic",
			step("python3", "-m"), []string{"pytest.py"}, nil},

		// An interpreter reads its script from the worktree.
		{"python script",
			step("python3", "check.py"), []string{"check.py"}, []string{"check.py"}},
		{"python script in a subdirectory",
			step("python3", "tools/check.py", "--strict"), []string{"tools/check.py"}, []string{"tools/check.py"}},
		{"shell script without ./",
			step("sh", "check.sh"), []string{"check.sh"}, []string{"check.sh"}},
		{"bash with a flag before the script",
			step("bash", "-e", "scripts/ci.sh"), []string{"scripts/ci.sh"}, []string{"scripts/ci.sh"}},
		{"bash option that takes a value",
			step("bash", "-o", "pipefail", "ci.sh"), []string{"ci.sh", "pipefail"}, []string{"ci.sh"}},

		// A relative interpreter is itself a file in the worktree: an agent that
		// created .venv/ wrote the python that would judge it. Found in review of
		// TASK-000027, where only the interpreter's arguments were checked.
		{"relative python interpreter inside a changed directory",
			step("./.venv/bin/python", "-m", "pytest"), []string{".venv/"}, []string{".venv/"}},
		{"relative interpreter and a shadowed module are both reported",
			step(".venv/bin/python3", "-m", "pytest"), []string{".venv/bin/python3", "pytest.py"}, []string{".venv/bin/python3", "pytest.py"}},
		{"relative interpreter left alone still has its module checked",
			step("./.venv/bin/python", "-m", "pytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"relative shell interpreter",
			step("tools/bash", "ci.sh"), []string{"tools/bash"}, []string{"tools/bash"}},
		{"a node_modules binary the agent installed",
			step("node_modules/.bin/jest"), []string{"node_modules/"}, []string{"node_modules/"}},

		// Python parses short flags as clusters, and an option that takes a value
		// can end one (measured: -Bm, -Bmunittest and -sm are shadowed; -IBm and
		// -BIm are not).
		{"-m at the end of a flag cluster",
			step("python3", "-Bm", "pytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"module joined to a flag cluster",
			step("python3", "-Bmpytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"another cluster ending in m",
			step("python3", "-sm", "pytest"), []string{"pytest.py"}, []string{"pytest.py"}},
		{"-I first in the cluster isolates",
			step("python3", "-IBm", "pytest"), []string{"pytest.py"}, nil},
		{"-I later in the cluster isolates",
			step("python3", "-BIm", "pytest"), []string{"pytest.py"}, nil},
		{"-W at the end of a cluster takes the next argument",
			step("python3", "-BW", "error", "-m", "pytest"), []string{"error", "pytest.py"}, []string{"pytest.py"}},
		// Found by an OpenCode reviewer reading TASK-000027 (docs/research.md 7g).
		// A top-level module T is found in the working directory only as T/, T.py,
		// T.pyc or T plus an extension suffix (importlib.machinery lists
		// .cpython-<tag>.so, .abi3.so and .so). Any other name that merely starts
		// with "T." is not a module: measured on Python 3.12.3, unittest.extra.py
		// does not shadow unittest and json.pyw does not shadow json. Intercepting
		// them blocks honest work.
		{"a dotted name is not the module it starts with",
			step("python3", "-m", "unittest"), []string{"unittest.extra.py"}, nil},
		{"only .py is a source suffix on this platform",
			step("python3", "-m", "json.tool"), []string{"json.pyw", "json.py.bak", "json.txt", "json.pyc.orig"}, nil},
		{"a stable-ABI extension module shadows",
			step("python3", "-m", "pytest"), []string{"pytest.abi3.so"}, []string{"pytest.abi3.so"}},
		{"a plain extension module shadows",
			step("python3", "-m", "pytest"), []string{"pytest.so"}, []string{"pytest.so"}},
		{"a free-threaded build's extension module shadows",
			step("python3.13t", "-m", "pytest"), []string{"pytest.cpython-313t-x86_64-linux-gnu.so"}, []string{"pytest.cpython-313t-x86_64-linux-gnu.so"}},
		{"a cpython tag without .so is not an extension module",
			step("python3", "-m", "pytest"), []string{"pytest.cpython-312-x86_64-linux-gnu.txt", "pytest.cpython.so.bak"}, nil},
		{"the real module files among unrelated ones",
			step("python3", "-m", "pkg.tool"), []string{"pkg.extra.py", "pkg.py", "pkg.pyc", "pkg.notes"}, []string{"pkg.py", "pkg.pyc"}},

		// Found by an OpenCode reviewer reading TASK-000027 (docs/research.md 7g).
		// With -s a shell reads its commands from stdin; the operand becomes a
		// positional argument, not a script to run. Intercepting it blocks honest
		// work for a file the step never executes.
		{"-s makes the operand an argument, not a script",
			step("sh", "-s", "check.sh"), []string{"check.sh"}, nil},
		{"-s inside a cluster",
			step("bash", "-es", "check.sh"), []string{"check.sh"}, nil},
		{"a shell reading stdin has no script to shadow",
			step("sh", "-"), []string{"-"}, nil},
		{"bash +o takes a value, like -o",
			step("bash", "+o", "posix", "ci.sh"), []string{"ci.sh", "posix"}, []string{"ci.sh"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := interceptedPaths(Interceptions([]task.VerificationStep{tc.step}, tc.changed))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Interceptions(%s, %q) = %q, want %q", tc.step.String(), tc.changed, got, tc.want)
			}
		})
	}
}

func TestInterceptionsNameTheStepAndKeepAStableOrder(t *testing.T) {
	steps := []task.VerificationStep{
		step("go", "test", "./..."),
		step("./check.sh"),
		step("python3", "-m", "pytest"),
	}
	changed := []string{"check.sh", "pytest.py", "pytest/"}

	got := Interceptions(steps, changed)
	want := []Interception{
		{StepIndex: 1, Step: "./check.sh", Path: "check.sh"},
		{StepIndex: 2, Step: "python3 -m pytest", Path: "pytest.py"},
		{StepIndex: 2, Step: "python3 -m pytest", Path: "pytest/"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Interceptions =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestNoChangesMeansNoInterceptions(t *testing.T) {
	steps := []task.VerificationStep{step("./check.sh"), step("python3", "-m", "pytest")}
	if got := Interceptions(steps, nil); len(got) != 0 {
		t.Errorf("Interceptions with nothing changed = %+v, want none", got)
	}
}
