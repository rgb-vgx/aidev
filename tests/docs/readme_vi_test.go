package docs

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"aidev/internal/config"
)

// README.vi.md is the Vietnamese README. A translation is only useful while it
// says what the original says, so these checks compare it with README.md section
// by section: the prose is translated, but every command, inline code span,
// configuration key and link is kept exactly, and no English sentence is left
// behind. Sections are compared by position, from the text before the first
// "## " heading (section 00) to the last one.

const readmeVietnamese = "../../README.vi.md"

// readmeSections splits a markdown file at its "## " headings. Headings inside
// code blocks do not count.
func readmeSections(body string) []string {
	var sections []string
	var current strings.Builder
	inCode := false
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
		}
		if !inCode && strings.HasPrefix(line, "## ") {
			sections = append(sections, current.String())
			current.Reset()
		}
		current.WriteString(line)
	}
	return append(sections, current.String())
}

// codeLines returns the command part of every line in the section's code blocks:
// comment lines are skipped and a trailing "  # comment" is cut, because
// comments are prose and may be translated.
func codeLines(section string) []string {
	var out []string
	inCode := false
	for line := range strings.SplitSeq(section, "\n") {
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			continue
		}
		if !inCode {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// proseOnly drops code blocks, so inline code and links are looked for in the text.
func proseOnly(section string) string {
	var b strings.Builder
	inCode := false
	for line := range strings.SplitSeq(section, "\n") {
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			continue
		}
		if !inCode {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

var (
	inlineCode = regexp.MustCompile("`([^`]+)`")
	linkTarget = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	whitespace = regexp.MustCompile(`\s+`)
)

// inlineSpans returns the inline code spans of the prose, paragraph by paragraph,
// with whitespace collapsed: a span may wrap onto the next line, and pairing
// backticks across a whole section would read the text between two spans as
// code.
func inlineSpans(prose string) []string {
	var spans []string
	for paragraph := range strings.SplitSeq(prose, "\n\n") {
		for _, m := range inlineCode.FindAllStringSubmatch(whitespace.ReplaceAllString(paragraph, " "), -1) {
			spans = append(spans, m[1])
		}
	}
	return spans
}

// linkedFrom accepts the original target or its Vietnamese counterpart. The
// English README's link to this translation becomes a link back to the original.
func linkedFrom(vi, target string) bool {
	if target == "README.vi.md" {
		return strings.Contains(vi, "](README.md)")
	}
	for _, t := range []string{
		target,
		strings.Replace(target, ".md", ".vi.md", 1),
		strings.Replace(target, "docs/guide/", "docs/guide/vi/", 1),
	} {
		if strings.Contains(vi, "]("+t) {
			return true
		}
	}
	return false
}

func TestReadmeVietnameseSections(t *testing.T) {
	english := readmeSections(readDoc(t, "../../README.md"))
	body, err := os.ReadFile(readmeVietnamese)
	if err != nil {
		t.Fatalf("README.vi.md is missing: %v", err)
	}
	vi := readmeSections(string(body))

	for i, en := range english {
		t.Run(fmt.Sprintf("%02d", i), func(t *testing.T) {
			heading, _, _ := strings.Cut(en, "\n")
			if i >= len(vi) {
				t.Fatalf("README.vi.md has no section %02d (English: %q)", i, heading)
			}
			v := vi[i]

			for _, line := range codeLines(en) {
				if !strings.Contains(v, line) {
					t.Errorf("section %02d (%s) drops or changes the code line %q", i, heading, line)
				}
			}
			flat := whitespace.ReplaceAllString(v, " ")
			for _, span := range inlineSpans(proseOnly(en)) {
				if !strings.Contains(flat, "`"+span+"`") {
					t.Errorf("section %02d (%s) drops or changes the inline code `%s`", i, heading, span)
				}
			}
			for _, m := range linkTarget.FindAllStringSubmatch(proseOnly(en), -1) {
				if !linkedFrom(v, m[1]) {
					t.Errorf("section %02d (%s) drops the link to %s", i, heading, m[1])
				}
			}

			// A sentence copied from the original is a sentence not translated.
			// Table rows and short lines are exempt: they are mostly names.
			for line := range strings.SplitSeq(proseOnly(en), "\n") {
				line = strings.TrimSpace(line)
				if len(line) < 60 || strings.HasPrefix(line, "|") || strings.HasPrefix(line, "#") {
					continue
				}
				if strings.Contains(v, line) {
					t.Errorf("section %02d (%s) still has the English line %q", i, heading, line)
				}
			}
			// Every section with prose to translate must read as Vietnamese.
			if letters := len(strings.Fields(proseOnly(en))); letters > 40 && countVietnameseRunes(v) < letters/4 {
				t.Errorf("section %02d (%s): %d Vietnamese characters for %d English words; it does not read as a translation",
					i, heading, countVietnameseRunes(v), letters)
			}
		})
	}
}

// The file as a whole: the same number of sections, every setting named, both
// READMEs pointing at each other at the top, balanced backticks, and no
// environment variable aidev no longer reads.
func TestReadmeVietnameseIsComplete(t *testing.T) {
	englishText := readDoc(t, "../../README.md")
	body, err := os.ReadFile(readmeVietnamese)
	if err != nil {
		t.Fatalf("README.vi.md is missing: %v", err)
	}
	vi := string(body)

	if en, got := len(readmeSections(englishText)), len(readmeSections(vi)); en != got {
		t.Errorf("README.vi.md has %d sections, README.md has %d", got, en)
	}
	for _, key := range config.SettingKeys() {
		if !strings.Contains(vi, key) {
			t.Errorf("README.vi.md does not document the setting %s", key)
		}
	}
	top := func(text string) string {
		lines := strings.SplitN(text, "\n", 6)
		return strings.Join(lines[:min(5, len(lines))], "\n")
	}
	if !strings.Contains(top(vi), "](README.md)") {
		t.Error("README.vi.md does not link to README.md in its first five lines")
	}
	if !strings.Contains(top(englishText), "](README.vi.md)") {
		t.Error("README.md does not link to README.vi.md in its first five lines")
	}
	// An inline code span may wrap onto the next line, so count per paragraph.
	for paragraph := range strings.SplitSeq(proseOnly(vi), "\n\n") {
		if strings.Count(paragraph, "`")%2 == 1 {
			t.Errorf("unbalanced backtick, which breaks the markdown after it, in:\n%s", paragraph)
		}
	}
	for _, removed := range removedFromTheEnvironment {
		if strings.Contains(strings.ReplaceAll(vi, "TEST_DATABASE_URL", ""), removed) {
			t.Errorf("README.vi.md names %s, which aidev no longer reads", removed)
		}
	}
}
