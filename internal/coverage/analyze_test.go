package coverage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A module in a subdirectory, as Axiom-Office keeps its Go code in core-go/:
// Add is tested, Sub is new and untested.
func TestAnalyzeFindsChangedLinesNoTestRan(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	root := t.TempDir()
	files := map[string]string{
		"core-go/go.mod":            "module example.com/m/core\n\ngo 1.21\n",
		"core-go/calc/calc.go":      "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\nfunc Sub(a, b int) int {\n\treturn a - b\n}\n",
		"core-go/calc/calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n",
		"README.md":                 "not go\n",
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The change: Add's body (line 4) and all of Sub (lines 6-9), plus the
	// README and the test file, which are not measured.
	diff := `--- a/core-go/calc/calc.go
+++ b/core-go/calc/calc.go
@@ -4 +4 @@
+	return a + b
@@ -5,0 +6,4 @@
+
+func Sub(a, b int) int {
+	return a - b
+}
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
+not go
--- a/core-go/calc/calc_test.go
+++ b/core-go/calc/calc_test.go
@@ -1 +1 @@
+package calc
`
	report, err := Analyze(context.Background(), Options{Dir: root, Diff: diff, Timeout: 2 * time.Minute, MaxOutputBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(report.Files) != 1 || report.Files[0].Path != "core-go/calc/calc.go" {
		t.Fatalf("files = %+v, want only core-go/calc/calc.go", report.Files)
	}
	f := report.Files[0]
	if f.NotRun == 0 || f.Changed <= f.NotRun {
		t.Errorf("file = %+v, want Add's line run and Sub's not", f)
	}
	for _, r := range f.NotRunLines {
		if r.From <= 4 && 4 <= r.To {
			t.Errorf("line 4 (Add, tested) reported as not run: %+v", f.NotRunLines)
		}
	}
	found := false
	for _, r := range f.NotRunLines {
		if r.From <= 8 && 8 <= r.To {
			found = true
		}
	}
	if !found {
		t.Errorf("line 8 (Sub's body, untested) not reported: %+v", f.NotRunLines)
	}
}

func TestAnalyzeWithoutGoChangesIsEmpty(t *testing.T) {
	report, err := Analyze(context.Background(), Options{Dir: t.TempDir(), Diff: "--- a/x.py\n+++ b/x.py\n@@ -1 +1 @@\n+x = 1\n"})
	if err != nil || report.Changed != 0 || len(report.Files) != 0 {
		t.Errorf("Analyze = %+v, %v; want empty", report, err)
	}
}

// Failing tests are a failure to measure, not a measurement.
func TestAnalyzeReportsFailingTestsAsAnError(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	root := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":    "module example.com/m\n\ngo 1.21\n",
		"x.go":      "package m\n\nfunc X() int { return 1 }\n",
		"x_test.go": "package m\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { t.Fatal(\"no\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Analyze(context.Background(), Options{Dir: root, Diff: "--- a/x.go\n+++ b/x.go\n@@ -3 +3 @@\n+func X() int { return 1 }\n", Timeout: time.Minute, MaxOutputBytes: 1 << 20})
	if err == nil {
		t.Error("Analyze succeeded although the tests failed")
	}
}
