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
