package docs

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The OpenSandbox assessment is a judgement about another project, and a
// judgement is only as good as the facts under it. While planning this work, a
// confident claim that OpenSandbox had no Go SDK was repeated here — and
// sdks/sandbox/go/go.mod had been in its repository all along.
//
// These checks cannot tell whether the recommendation is wise. They insist that
// every file the assessment cites exists, at the commit it names, so that a
// reader can check each claim; and that the facts which decide the question for
// aidev are addressed rather than skipped.

const (
	assessmentPath = "../../docs/opensandbox.md"
	repoRoot       = "../.."

	// openSandboxCommit is what the assessment is written against. A report
	// about a moving project is unverifiable without it.
	openSandboxCommit = "d8cfce39dc1d846e580510ca44f44c495cbe95c4"
)

// A citation is written in backticks as `opensandbox:path`, `aidev:path`, or
// either with :line or :start-end, so that it can be checked mechanically.
var citationPattern = regexp.MustCompile("`(opensandbox|aidev):([^`\\s:]+)(?::(\\d+)(?:-(\\d+))?)?`")

type citation struct {
	repo       string
	path       string
	start, end int // zero when the citation names no lines
	raw        string
}

func readAssessment(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(assessmentPath)
	if err != nil {
		t.Fatalf("docs/opensandbox.md is missing: %v", err)
	}
	return string(body)
}

func citations(body string) []citation {
	var out []citation
	for _, m := range citationPattern.FindAllStringSubmatch(body, -1) {
		c := citation{repo: m[1], path: m[2], raw: m[0]}
		if m[3] != "" {
			c.start, _ = strconv.Atoi(m[3])
			c.end = c.start
		}
		if m[4] != "" {
			c.end, _ = strconv.Atoi(m[4])
		}
		out = append(out, c)
	}
	return out
}

func countLines(content string) int {
	n := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		n++
	}
	return n
}

func checkLines(t *testing.T, c citation, content string) {
	t.Helper()
	if c.start == 0 {
		return
	}
	if c.start > c.end {
		t.Errorf("%s: range starts after it ends", c.raw)
	}
	if lines := countLines(content); c.end > lines {
		t.Errorf("%s: the file has only %d lines", c.raw, lines)
	}
}

func TestOpenSandboxAssessmentIsStructured(t *testing.T) {
	body := readAssessment(t)

	if len(body) < 9000 {
		t.Errorf("assessment is %d bytes, want at least 9000: it has to show its evidence, not only its conclusion", len(body))
	}

	lower := strings.ToLower(body)
	for _, heading := range []string{
		"## what opensandbox is",
		"## how aidev isolates work today",
		"## where it could fit",
		"## what it would cost",
		"## what was run and what was not",
		"## recommendation",
	} {
		if !strings.Contains(lower, "\n"+heading) {
			t.Errorf("no %q section", heading)
		}
	}

	if !strings.Contains(body, openSandboxCommit) {
		t.Errorf("the assessment does not name the OpenSandbox commit %s it was written against", openSandboxCommit)
	}

	// The facts that decide whether aidev can run its agent and its verification
	// inside OpenSandbox: whether a worktree can be mounted at all, what the
	// server needs from the host, whether a failed command's exit code survives
	// the trip back, and what network a sandbox gets by default.
	for _, fact := range []string{"allowed_host_paths", "docker.sock", "ExitCode", "network_mode"} {
		if !strings.Contains(body, fact) {
			t.Errorf("the assessment never addresses %q", fact)
		}
	}

	for _, wrong := range []string{"no go sdk", "does not have a go sdk", "doesn't have a go sdk", "without a go sdk"} {
		if strings.Contains(lower, wrong) {
			t.Errorf("the assessment says %q; sdks/sandbox/go exists at the pinned commit", wrong)
		}
	}

	cited := map[string]bool{}
	counts := map[string]int{}
	for _, c := range citations(body) {
		cited[c.repo+":"+c.path] = true
		counts[c.repo]++
	}
	if counts["opensandbox"] < 20 {
		t.Errorf("%d OpenSandbox citations, want at least 20", counts["opensandbox"])
	}
	if counts["aidev"] < 6 {
		t.Errorf("%d aidev citations, want at least 6: a fit cannot be judged without the side it fits into", counts["aidev"])
	}
	for _, must := range []string{
		"opensandbox:sdks/sandbox/go/go.mod",
		"opensandbox:sdks/sandbox/go/execution.go",
		"opensandbox:specs/sandbox-lifecycle.yml",
		"opensandbox:specs/execd-api.yaml",
		"opensandbox:server/configuration.md",
		"opensandbox:examples/opencode/main.py",
		"opensandbox:examples/codex-cli/main.py",
		"aidev:internal/procexec/procexec.go",
		"aidev:internal/verification/verification.go",
		"aidev:internal/agent/agent.go",
		"aidev:internal/git/git.go",
		"aidev:internal/worker/worker.go",
	} {
		if !cited[must] {
			t.Errorf("the assessment never cites `%s`", must)
		}
	}
}

// aidev's own citations are checked on every `make check`: when the code moves,
// the assessment is told.
func TestOpenSandboxAssessmentCitesAidevFilesThatExist(t *testing.T) {
	for _, c := range citations(readAssessment(t)) {
		if c.repo != "aidev" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(c.path)))
		if err != nil {
			t.Errorf("%s: %v", c.raw, err)
			continue
		}
		checkLines(t, c, string(content))
	}
}

// OpenSandbox citations need a clone, which the repository does not carry. The
// check skips without one, except when AIDEV_REQUIRE_GROUNDING is set — which is
// how the task that writes the assessment is verified, so that a missing clone
// cannot pass as a clean result.
func TestOpenSandboxCitationsExistAtThePinnedCommit(t *testing.T) {
	clone := os.Getenv("OPENSANDBOX_CLONE")
	if clone == "" {
		if os.Getenv("AIDEV_REQUIRE_GROUNDING") != "" {
			t.Fatal("AIDEV_REQUIRE_GROUNDING is set but OPENSANDBOX_CLONE is not: the citations cannot be checked")
		}
		t.Skip("set OPENSANDBOX_CLONE to a clone of https://github.com/opensandbox-group/OpenSandbox to check citations")
	}
	if out, err := exec.Command("git", "-C", clone, "cat-file", "-e", openSandboxCommit+"^{commit}").CombinedOutput(); err != nil {
		t.Fatalf("%s does not contain commit %s (git fetch it): %v %s", clone, openSandboxCommit, err, out)
	}

	for _, c := range citations(readAssessment(t)) {
		if c.repo != "opensandbox" {
			continue
		}
		object := openSandboxCommit + ":" + c.path
		if err := exec.Command("git", "-C", clone, "cat-file", "-e", object).Run(); err != nil {
			t.Errorf("%s: no such path at %s", c.raw, openSandboxCommit[:12])
			continue
		}
		if c.start == 0 {
			continue
		}
		content, err := exec.Command("git", "-C", clone, "show", object).Output()
		if err != nil {
			t.Errorf("%s: %v", c.raw, err)
			continue
		}
		checkLines(t, c, string(content))
	}
}

const assessmentPathVietnamese = "../../docs/opensandbox.vi.md"

// The Vietnamese version carries the same evidence as the original. A
// translation that drops a citation, or points one at different lines, is a
// different argument wearing the same title, so the citations must match the
// English file exactly. Prose and headings are translated; commands, paths,
// configuration keys and citations are not.
func TestOpenSandboxVietnameseCarriesTheSameEvidence(t *testing.T) {
	english := readAssessment(t)
	body, err := os.ReadFile(assessmentPathVietnamese)
	if err != nil {
		t.Fatalf("docs/opensandbox.vi.md is missing: %v", err)
	}
	vi := string(body)

	if !strings.Contains(vi, openSandboxCommit) {
		t.Errorf("the Vietnamese version does not name the OpenSandbox commit %s", openSandboxCommit)
	}
	for _, heading := range []string{
		"## OpenSandbox là gì",
		"## aidev hiện cô lập công việc như thế nào",
		"## OpenSandbox có thể lắp vào đâu",
		"## Cái giá phải trả",
		"## Đã chạy gì và chưa chạy gì",
		"## Khuyến nghị",
	} {
		if !strings.Contains(vi, "\n"+heading) {
			t.Errorf("no %q section", heading)
		}
	}
	for _, fact := range []string{"allowed_host_paths", "docker.sock", "ExitCode", "network_mode"} {
		if !strings.Contains(vi, fact) {
			t.Errorf("the Vietnamese version never addresses %q", fact)
		}
	}

	// An English file saved under a Vietnamese name would pass every check above.
	if n := countVietnameseRunes(vi); n < 1500 {
		t.Errorf("%d Vietnamese characters, want at least 1500: this does not read as a translation", n)
	}

	tally := func(body string) map[string]int {
		m := map[string]int{}
		for _, c := range citations(body) {
			m[c.raw]++
		}
		return m
	}
	en, got := tally(english), tally(vi)
	for raw, n := range en {
		if got[raw] != n {
			t.Errorf("%s appears %d time(s) in English and %d in Vietnamese", raw, n, got[raw])
		}
	}
	for raw, n := range got {
		if en[raw] == 0 {
			t.Errorf("%s appears %d time(s) in Vietnamese but not in English", raw, n)
		}
	}

	for i, line := range strings.Split(vi, "\n") {
		if strings.Count(line, "`")%2 == 1 {
			t.Errorf("line %d has an unbalanced backtick, which breaks the markdown after it", i+1)
		}
	}
}
