// Package docs checks the HTML guide in docs/guide.
//
// Documentation rots silently, and a guide written for someone new is worth
// nothing if it is wrong, incomplete, or links to pages that do not exist. These
// checks are mechanical on purpose: they cannot judge whether an explanation is
// clear, but they can insist that every page parses, that every promised section
// is present, that no internal link is broken, and that the facts a reader must
// not be misled about are actually stated.
package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// guideDir is relative to this test file's package directory.
const guideDir = "../../docs/guide"

// page is one required page and what it must contain.
type page struct {
	file string

	// title must appear in the <title> element.
	titleContains string

	// sections are HTML id attributes that must exist, so that the guide's
	// structure is fixed and every page can be linked to by section.
	sections []string

	// mustMention are strings the page must contain. They are the facts a reader
	// must not leave without, stated in the page that is responsible for them.
	mustMention []string

	// minBytes forces actual detail rather than a stub. A page that merely lists
	// headings will not reach it.
	minBytes int

	// minCodeBlocks forces worked examples rather than prose alone. A reader who
	// is new needs commands they can copy.
	minCodeBlocks int
}

func requiredPages() []page {
	return []page{
		{
			file:          "index.html",
			titleContains: "aidev",
			sections: []string{
				"what-is-aidev", "why", "architecture", "lifecycle",
				"vocabulary", "where-next",
			},
			mustMention: []string{
				// The product's central rule. A reader who misses this
				// misunderstands everything else.
				"verification",
				"worktree",
				"SUCCEEDED",
				"PostgreSQL",
				"OpenCode",
				"Claude Code",
			},
			minBytes:      9000,
			minCodeBlocks: 3,
		},
		{
			file:          "getting-started.html",
			titleContains: "Getting started",
			sections: []string{
				"prerequisites", "install", "database", "configuration",
				"first-task", "reading-the-result", "where-the-work-is",
			},
			mustMention: []string{
				"make db-up",
				"aidev migrate",
				"aidev task create",
				"aidev task run",
				"DATABASE_URL",
				"WORKSPACE_ROOT",
				"--verify",
				"aidev/TASK-",
			},
			minBytes:      9000,
			minCodeBlocks: 6,
		},
		{
			file:          "debugging.html",
			titleContains: "Debugging",
			sections: []string{
				"where-to-look-first", "logs", "events", "task-result",
				"worktrees", "tracing", "recovery", "common-problems",
			},
			mustMention: []string{
				"stderr",
				"aidev task events",
				"aidev task result",
				"--logs",
				"aidev worktree list",
				"RETAINED",
				"aidev task cancel",
				"LOG_LEVEL",
				"OTEL_EXPORTER_OTLP_ENDPOINT",
			},
			minBytes:      11000,
			minCodeBlocks: 8,
		},
		{
			file:          "reference.html",
			titleContains: "Reference",
			sections: []string{
				"cli", "task-statuses", "failure-kinds", "events-reference",
				"environment", "mcp-tools",
			},
			mustMention: []string{
				"WAITING_APPROVAL", "VERIFYING", "CANCELLED",
				"VERIFICATION", "AGENT_ERROR", "TIMEOUT",
				"task.verification_completed",
				"aidev_create_task", "aidev_run_task", "aidev_get_task_result",
				"DEFAULT_TASK_TIMEOUT", "WORKTREE_CLEANUP", "OPENCODE_MODEL",
			},
			minBytes:      11000,
			minCodeBlocks: 4,
		},
	}
}

func readPage(t *testing.T, file string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(guideDir, file))
	if err != nil {
		t.Fatalf("docs/guide/%s is missing: %v", file, err)
	}
	return string(body)
}

func TestGuidePagesExistAndAreComplete(t *testing.T) {
	for _, p := range requiredPages() {
		t.Run(p.file, func(t *testing.T) {
			body := readPage(t, p.file)

			if len(body) < p.minBytes {
				t.Errorf("page is %d bytes, want at least %d: it is meant to explain, not to list headings",
					len(body), p.minBytes)
			}

			if !strings.Contains(strings.ToLower(title(body)), strings.ToLower(p.titleContains)) {
				t.Errorf("<title> is %q, want it to contain %q", title(body), p.titleContains)
			}
			if !regexp.MustCompile(`(?is)<h1[^>]*>`).MatchString(body) {
				t.Error("no <h1>: every page needs one heading that says what it is")
			}

			for _, id := range p.sections {
				if !hasID(body, id) {
					t.Errorf("no element with id=%q; the guide's sections are fixed so that they can be linked to", id)
				}
			}

			for _, want := range p.mustMention {
				if !strings.Contains(body, want) {
					t.Errorf("page does not mention %q, which it is the page responsible for explaining", want)
				}
			}

			if blocks := countCodeBlocks(body); blocks < p.minCodeBlocks {
				t.Errorf("%d code block(s), want at least %d: a reader who is new needs commands to copy",
					blocks, p.minCodeBlocks)
			}
		})
	}
}

// A reader arriving on any page must be able to get back, and the index must lead
// everywhere, or the guide is a pile of pages rather than a guide.
func TestGuideIsNavigable(t *testing.T) {
	index := readPage(t, "index.html")
	for _, p := range requiredPages() {
		if p.file == "index.html" {
			continue
		}
		if !strings.Contains(index, p.file) {
			t.Errorf("index.html does not link to %s", p.file)
		}

		body := readPage(t, p.file)
		if !strings.Contains(body, "index.html") {
			t.Errorf("%s has no link back to index.html", p.file)
		}
	}
}

// A link that does not resolve is worse than no link: it teaches the reader that
// the guide cannot be trusted.
func TestGuideHasNoBrokenInternalLinks(t *testing.T) {
	hrefPattern := regexp.MustCompile(`(?i)(?:href|src)="([^"]+)"`)

	for _, p := range requiredPages() {
		body := readPage(t, p.file)
		ids := collectIDs(body)

		for _, match := range hrefPattern.FindAllStringSubmatch(body, -1) {
			target := match[1]
			switch {
			case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"),
				strings.HasPrefix(target, "mailto:"), strings.HasPrefix(target, "data:"):
				continue

			case strings.HasPrefix(target, "#"):
				if anchor := strings.TrimPrefix(target, "#"); anchor != "" && !ids[anchor] {
					t.Errorf("%s links to #%s, which no element on that page defines", p.file, anchor)
				}

			default:
				file, anchor, _ := strings.Cut(target, "#")
				if file == "" {
					continue
				}
				path := filepath.Join(guideDir, file)
				if _, err := os.Stat(path); err != nil {
					t.Errorf("%s links to %q, which does not exist", p.file, target)
					continue
				}
				if anchor != "" && strings.HasSuffix(file, ".html") {
					linked, err := os.ReadFile(path)
					if err == nil && !collectIDs(string(linked))[anchor] {
						t.Errorf("%s links to %q, but %s defines no id=%q", p.file, target, file, anchor)
					}
				}
			}
		}
	}
}

// A stylesheet referenced by every page must exist, and the pages must actually
// reference it: an unstyled wall of text is not a guide someone will read.
func TestGuideHasAStylesheet(t *testing.T) {
	if _, err := os.Stat(filepath.Join(guideDir, "guide.css")); err != nil {
		t.Fatalf("docs/guide/guide.css is missing: %v", err)
	}
	for _, p := range requiredPages() {
		if !strings.Contains(readPage(t, p.file), "guide.css") {
			t.Errorf("%s does not reference guide.css", p.file)
		}
	}
}

// Every page must be well-formed enough that a browser and this test agree on its
// structure. Unclosed tags are how a page silently loses half its content.
func TestGuidePagesAreWellFormed(t *testing.T) {
	for _, p := range requiredPages() {
		body := readPage(t, p.file)

		if !strings.Contains(strings.ToLower(body), "<!doctype html>") {
			t.Errorf("%s has no doctype", p.file)
		}
		if !strings.Contains(body, `lang=`) {
			t.Errorf("%s does not declare a language on <html>", p.file)
		}
		if !strings.Contains(strings.ToLower(body), `charset=`) {
			t.Errorf("%s does not declare a charset", p.file)
		}

		for _, tag := range []string{"html", "head", "body", "main"} {
			open := strings.Count(strings.ToLower(body), "<"+tag)
			close := strings.Count(strings.ToLower(body), "</"+tag+">")
			if open == 0 {
				t.Errorf("%s has no <%s>", p.file, tag)
				continue
			}
			if open != close {
				t.Errorf("%s has %d <%s> and %d </%s>: tags are unbalanced", p.file, open, tag, close, tag)
			}
		}
	}
}

// Placeholders mean the page was not finished. They are easy to leave behind and
// embarrassing to ship.
func TestGuideHasNoPlaceholders(t *testing.T) {
	forbidden := []string{"TODO", "FIXME", "Lorem ipsum", "XXX", "<!-- fill", "TBD", "coming soon"}
	for _, p := range requiredPages() {
		body := readPage(t, p.file)
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains the placeholder %q", p.file, bad)
			}
		}
	}
}

// The guide must state the one rule that makes aidev what it is. A guide that
// leaves a reader believing the agent decides the outcome has failed at its job
// however well written the rest is.
func TestGuideExplainsThatOnlyVerificationDecidesSuccess(t *testing.T) {
	index := readPage(t, "index.html")
	lowered := strings.ToLower(index)

	for _, phrase := range []string{"only", "verification"} {
		if !strings.Contains(lowered, phrase) {
			t.Fatalf("index.html does not contain %q", phrase)
		}
	}
	// The agent's claim must be named as something that does not decide anything.
	if !strings.Contains(lowered, "agent") {
		t.Error("index.html never mentions the agent")
	}
	if !hasID(index, "why") {
		t.Error("index.html has no #why section, which is where this belongs")
	}
}

// A glossary is what lets a reader who does not know the words follow the rest.
func TestGuideDefinesItsTerms(t *testing.T) {
	index := readPage(t, "index.html")
	if !hasID(index, "vocabulary") {
		t.Fatal("index.html has no #vocabulary section")
	}
	for _, term := range []string{
		"worktree", "attempt", "verification", "task", "agent", "event",
	} {
		if !strings.Contains(strings.ToLower(index), term) {
			t.Errorf("the vocabulary does not cover %q", term)
		}
	}
}

func title(body string) string {
	m := regexp.MustCompile(`(?is)<title>(.*?)</title>`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

var idPattern = regexp.MustCompile(`(?i)\sid="([^"]+)"`)

func collectIDs(body string) map[string]bool {
	ids := map[string]bool{}
	for _, m := range idPattern.FindAllStringSubmatch(body, -1) {
		ids[m[1]] = true
	}
	return ids
}

func hasID(body, id string) bool { return collectIDs(body)[id] }

func countCodeBlocks(body string) int {
	return strings.Count(strings.ToLower(body), "<pre")
}
