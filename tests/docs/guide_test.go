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
	// dir is the subdirectory under docs/guide, empty for the English pages.
	dir string

	file string

	// lang is the value the <html lang> attribute must have.
	lang string

	// minVietnameseRunes, when set, requires that many characters carrying
	// Vietnamese diacritics. It is a crude proxy, and it exists for one specific
	// failure: an English page saved under a Vietnamese path with lang="vi",
	// which would pass every other check here while being useless.
	minVietnameseRunes int

	// mustNotContain catches exactly that, by naming sentences from the English
	// original that a translation cannot legitimately reproduce verbatim.
	mustNotContain []string

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

// englishPages and vietnamesePages are kept as one list so that every structural
// check applies to both without being written twice. A translation that is not held
// to the same standard as the original is a translation nobody can rely on.
func requiredPages() []page {
	return append(englishPages(), vietnamesePages()...)
}

func englishPages() []page {
	return []page{
		{
			lang:          "en",
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
			lang:          "en",
			file:          "getting-started.html",
			titleContains: "Getting started",
			sections: []string{
				"prerequisites", "install", "database", "configuration",
				"first-task", "reading-the-result", "where-the-work-is",
				"claude-code",
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
				// A reader who closes the terminal must be able to come back
				// tomorrow without repeating the setup, so the guide has to say
				// where configuration lives and how aidev gets on the PATH.
				"config.env",
				"make install",
				// aidev exists to be driven by Claude Code. A guide that never says
				// how to connect the two leaves the reader with a CLI and no
				// planner. --scope user, because the default local scope confines
				// the server to one directory and it silently vanishes in every
				// other repository; the success line is real output, not invented.
				"claude mcp add --scope user",
				"claude mcp get aidev",
				"Scope: User config",
			},
			minBytes:      9000,
			minCodeBlocks: 6,
		},
		{
			lang:          "en",
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
				// Every status `aidev worktree list --all` can print must be
				// explained where the list is, or the reader meets words the guide
				// never defined.
				"ACTIVE",
				"REMOVED",
				"aidev task cancel",
				"LOG_LEVEL",
				"OTEL_EXPORTER_OTLP_ENDPOINT",
			},
			minBytes:      11000,
			minCodeBlocks: 8,
		},
		{
			lang:          "en",
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
				"AIDEV_CONFIG",
			},
			minBytes:      11000,
			minCodeBlocks: 4,
		},
	}
}

// vietnamesePages mirror the English set. The section ids and the things each page
// must mention are deliberately identical: commands, status names, event types and
// environment variables are not translated, and holding both versions to the same
// list is what stops them drifting apart.
func vietnamesePages() []page {
	pages := englishPages()
	out := make([]page, 0, len(pages))

	titles := map[string]string{
		"index.html":           "aidev",
		"getting-started.html": "Bắt đầu",
		"debugging.html":       "Gỡ lỗi",
		"reference.html":       "Tra cứu",
	}
	// Sentences from the English original. Finding one in the translation means a
	// page was copied rather than written.
	untranslated := []string{
		"An agent that reports its own success is not a source of truth",
		"only verification can move a task to SUCCEEDED",
		"A reader who is new needs commands they can copy",
	}

	for _, p := range pages {
		p.dir = "vi"
		p.lang = "vi"
		p.titleContains = titles[p.file]
		p.minVietnameseRunes = 600
		p.mustNotContain = untranslated
		out = append(out, p)
	}
	return out
}

func readPage(t *testing.T, p page) string {
	t.Helper()
	path := filepath.Join(guideDir, p.dir, p.file)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s is missing: %v", filepath.Join("docs/guide", p.dir, p.file), err)
	}
	return string(body)
}

// label names a page for test output, including its locale directory.
func (p page) label() string { return filepath.Join(p.dir, p.file) }

func TestGuidePagesExistAndAreComplete(t *testing.T) {
	for _, p := range requiredPages() {
		t.Run(p.label(), func(t *testing.T) {
			body := readPage(t, p)

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
	for _, set := range [][]page{englishPages(), vietnamesePages()} {
		index := readPage(t, set[0])
		for _, p := range set {
			if p.file == "index.html" {
				continue
			}
			if !strings.Contains(index, p.file) {
				t.Errorf("%s does not link to %s", set[0].label(), p.file)
			}
			if !strings.Contains(readPage(t, p), "index.html") {
				t.Errorf("%s has no link back to its index", p.label())
			}
		}
	}
}

// A reader who lands on the wrong language must be able to switch, from any page,
// or the translation is only reachable by guessing a URL.
func TestGuideLinksBetweenLanguages(t *testing.T) {
	for _, p := range englishPages() {
		if !strings.Contains(readPage(t, p), "vi/") {
			t.Errorf("%s offers no link to the Vietnamese version", p.label())
		}
	}
	for _, p := range vietnamesePages() {
		if !strings.Contains(readPage(t, p), "../") {
			t.Errorf("%s offers no link back to the English version", p.label())
		}
	}
}

// A translation must be a translation. lang="vi" on an English page would satisfy
// every structural check here while being worthless.
func TestVietnamesePagesAreActuallyVietnamese(t *testing.T) {
	for _, p := range vietnamesePages() {
		body := readPage(t, p)

		if n := countVietnameseRunes(body); n < p.minVietnameseRunes {
			t.Errorf("%s has %d characters with Vietnamese diacritics, want at least %d: it does not look translated",
				p.label(), n, p.minVietnameseRunes)
		}
		for _, sentence := range p.mustNotContain {
			if strings.Contains(body, sentence) {
				t.Errorf("%s contains the English sentence %q verbatim, so that part was copied rather than translated",
					p.label(), sentence)
			}
		}
	}
}

// countVietnameseRunes counts characters that only appear in Vietnamese text, which
// is enough to tell a translation from a copy.
func countVietnameseRunes(body string) int {
	const marks = "ăâđêôơưáàảãạắằẳẵặấầẩẫậéèẻẽẹếềểễệíìỉĩịóòỏõọốồổỗộớờởỡợúùủũụứừửữựýỳỷỹỵ"
	count := 0
	for _, r := range strings.ToLower(body) {
		if strings.ContainsRune(marks, r) {
			count++
		}
	}
	return count
}

// A link that does not resolve is worse than no link: it teaches the reader that
// the guide cannot be trusted.
func TestGuideHasNoBrokenInternalLinks(t *testing.T) {
	hrefPattern := regexp.MustCompile(`(?i)(?:href|src)="([^"]+)"`)

	for _, p := range requiredPages() {
		body := readPage(t, p)
		ids := collectIDs(body)
		base := filepath.Join(guideDir, p.dir)

		for _, match := range hrefPattern.FindAllStringSubmatch(body, -1) {
			target := match[1]
			switch {
			case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"),
				strings.HasPrefix(target, "mailto:"), strings.HasPrefix(target, "data:"):
				continue

			case strings.HasPrefix(target, "#"):
				if anchor := strings.TrimPrefix(target, "#"); anchor != "" && !ids[anchor] {
					t.Errorf("%s links to #%s, which no element on that page defines", p.label(), anchor)
				}

			default:
				file, anchor, _ := strings.Cut(target, "#")
				if file == "" {
					continue
				}
				path := filepath.Join(base, file)
				if _, err := os.Stat(path); err != nil {
					t.Errorf("%s links to %q, which does not exist", p.label(), target)
					continue
				}
				if anchor != "" && strings.HasSuffix(file, ".html") {
					linked, err := os.ReadFile(path)
					if err == nil && !collectIDs(string(linked))[anchor] {
						t.Errorf("%s links to %q, but %s defines no id=%q", p.label(), target, file, anchor)
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
	// One stylesheet for both languages: a second copy would drift.
	for _, p := range requiredPages() {
		if !strings.Contains(readPage(t, p), "guide.css") {
			t.Errorf("%s does not reference guide.css", p.label())
		}
	}
}

// Every page must be well-formed enough that a browser and this test agree on its
// structure. Unclosed tags are how a page silently loses half its content.
func TestGuidePagesAreWellFormed(t *testing.T) {
	for _, p := range requiredPages() {
		body := readPage(t, p)

		if !strings.Contains(strings.ToLower(body), "<!doctype html>") {
			t.Errorf("%s has no doctype", p.label())
		}
		if !strings.Contains(body, `lang="`+p.lang+`"`) {
			t.Errorf("%s does not declare lang=%q on <html>", p.label(), p.lang)
		}
		if !strings.Contains(strings.ToLower(body), `charset=`) {
			t.Errorf("%s does not declare a charset", p.label())
		}

		for _, tag := range []string{"html", "head", "body", "main"} {
			open := strings.Count(strings.ToLower(body), "<"+tag)
			close := strings.Count(strings.ToLower(body), "</"+tag+">")
			if open == 0 {
				t.Errorf("%s has no <%s>", p.label(), tag)
				continue
			}
			if open != close {
				t.Errorf("%s has %d <%s> and %d </%s>: tags are unbalanced", p.label(), open, tag, close, tag)
			}
		}
	}
}

// Placeholders mean the page was not finished. They are easy to leave behind and
// embarrassing to ship.
func TestGuideHasNoPlaceholders(t *testing.T) {
	forbidden := []string{"TODO", "FIXME", "Lorem ipsum", "XXX", "<!-- fill", "TBD", "coming soon"}
	for _, p := range requiredPages() {
		body := readPage(t, p)
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains the placeholder %q", p.label(), bad)
			}
		}
	}
}

// The guide must state the one rule that makes aidev what it is. A guide that
// leaves a reader believing the agent decides the outcome has failed at its job
// however well written the rest is.
func TestGuideExplainsThatOnlyVerificationDecidesSuccess(t *testing.T) {
	for _, set := range [][]page{englishPages(), vietnamesePages()} {
		index := readPage(t, set[0])
		lowered := strings.ToLower(index)

		// "verification" and "agent" are not translated in either version: they
		// name things the reader will see in the tool's own output.
		for _, phrase := range []string{"verification", "agent", "SUCCEEDED"} {
			if !strings.Contains(lowered, strings.ToLower(phrase)) {
				t.Errorf("%s does not contain %q", set[0].label(), phrase)
			}
		}
		if !hasID(index, "why") {
			t.Errorf("%s has no #why section, which is where this belongs", set[0].label())
		}
	}
}

// A glossary is what lets a reader who does not know the words follow the rest.
func TestGuideDefinesItsTerms(t *testing.T) {
	for _, set := range [][]page{englishPages(), vietnamesePages()} {
		index := readPage(t, set[0])
		if !hasID(index, "vocabulary") {
			t.Errorf("%s has no #vocabulary section", set[0].label())
			continue
		}
		for _, term := range []string{
			"worktree", "attempt", "verification", "task", "agent", "event",
		} {
			if !strings.Contains(strings.ToLower(index), term) {
				t.Errorf("%s: the vocabulary does not cover %q", set[0].label(), term)
			}
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
