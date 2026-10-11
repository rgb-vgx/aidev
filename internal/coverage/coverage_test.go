package coverage

import (
	"reflect"
	"testing"
)

func TestParseProfile(t *testing.T) {
	profile := `mode: set
example.com/m/calc/calc.go:3.24,5.2 1 1
example.com/m/calc/calc.go:7.24,9.2 1 0
example.com/m/calc/calc.go:7.24,9.2 1 1
`
	blocks, err := ParseProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	want := Block{File: "example.com/m/calc/calc.go", StartLine: 7, EndLine: 9, Count: 0}
	if blocks[1] != want {
		t.Errorf("blocks[1] = %+v, want %+v", blocks[1], want)
	}
	if _, err := ParseProfile("not a profile"); err == nil {
		t.Error("a malformed profile parsed without error")
	}
}

func TestAddedLines(t *testing.T) {
	// `git diff -U0` output: a new file, a modified hunk, a deletion.
	diff := `diff --git a/calc/calc.go b/calc/calc.go
index 1..2 100644
--- a/calc/calc.go
+++ b/calc/calc.go
@@ -4,0 +5,3 @@ func Add(a, b int) int {
+
+func Sub(a, b int) int {
+	return a - b
@@ -10 +13 @@ func Mul(a, b int) int {
-	return 0
+	return a * b
@@ -20,2 +22,0 @@
-gone
-gone
diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package m
+var X = 1
`
	got := AddedLines(diff)
	want := map[string][]int{
		"calc/calc.go": {5, 6, 7, 13},
		"new.go":       {1, 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AddedLines = %v, want %v", got, want)
	}
}

// A changed line counts as run when a block containing it ran; as not run
// when the only blocks containing it never did; and not at all when no block
// contains it (a comment, a declaration, a blank line).
func TestCompare(t *testing.T) {
	blocks := []Block{
		{File: "calc.go", StartLine: 3, EndLine: 5, Count: 1},
		{File: "calc.go", StartLine: 7, EndLine: 9, Count: 0},
		{File: "calc.go", StartLine: 11, EndLine: 12, Count: 0},
		{File: "calc.go", StartLine: 11, EndLine: 12, Count: 2}, // the same block from another test binary
	}
	added := map[string][]int{"calc.go": {1, 4, 7, 8, 11, 20}}
	report := Compare(added, blocks)
	if len(report.Files) != 1 {
		t.Fatalf("files = %+v, want one", report.Files)
	}
	f := report.Files[0]
	if f.Changed != 4 || f.NotRun != 2 {
		t.Errorf("file = %+v, want 4 changed executable lines, 2 not run", f)
	}
	if !reflect.DeepEqual(f.NotRunLines, []LineRange{{7, 8}}) {
		t.Errorf("not run = %v, want 7-8", f.NotRunLines)
	}
	if report.Changed != 4 || report.NotRun != 2 {
		t.Errorf("totals = %d/%d, want 4/2", report.Changed, report.NotRun)
	}
}

func TestModulePath(t *testing.T) {
	gomod := "// comment\nmodule   example.com/m/core\n\ngo 1.25\n"
	if got := ModulePath(gomod); got != "example.com/m/core" {
		t.Errorf("ModulePath = %q", got)
	}
	if got := ModulePath(`module "quoted/path"`); got != "quoted/path" {
		t.Errorf("ModulePath(quoted) = %q", got)
	}
}
