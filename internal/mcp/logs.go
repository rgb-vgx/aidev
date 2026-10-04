package mcp

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// The sections include_logs can return (research D1). Each is paged on its
// own, with the same offset and max_bytes, so a caller reads a long diff a
// page at a time without re-fetching everything else.
const (
	sectionTranscript   = "transcript"
	sectionStdout       = "stdout"
	sectionStderr       = "stderr"
	sectionDiff         = "diff"
	sectionVerification = "verification"
)

// allSections is every valid section, in the order they are documented.
var allSections = []string{sectionTranscript, sectionStdout, sectionStderr, sectionDiff, sectionVerification}

// defaultSections is what include_logs returns when no sections are named.
// The raw stdout is left out: the transcript carries the same events as
// plain text, and the raw stream is mostly JSON structure — the reason one
// call could put megabytes into a planner's context.
var defaultSections = []string{sectionTranscript, sectionStderr, sectionDiff, sectionVerification}

const (
	// defaultLogBytes bounds each section when the caller does not say. It
	// is enough to diagnose most failures and small enough that a result
	// with every section still fits comfortably in a planner's context.
	defaultLogBytes = 64 * 1024

	// maxLogBytes caps what a caller may ask for in one section. Stored
	// output is already bounded by output.max_bytes, so this only stops a
	// single call from asking for all of it at once.
	maxLogBytes = 1 << 20
)

// logRequest is a validated include_logs request.
type logRequest struct {
	sections []string
	offset   int
	maxBytes int
}

func (r logRequest) wants(section string) bool { return slices.Contains(r.sections, section) }

// parseLogRequest validates the paging inputs. Naming sections implies
// include_logs: asking for the diff and getting nothing back would be a trap.
// It returns ok=false when no logs were asked for at all.
func parseLogRequest(include bool, sections []string, offset, maxBytes int) (logRequest, bool, error) {
	if !include && len(sections) == 0 {
		return logRequest{}, false, nil
	}
	if offset < 0 {
		return logRequest{}, false, fmt.Errorf("offset must not be negative, got %d", offset)
	}
	if maxBytes < 0 {
		return logRequest{}, false, fmt.Errorf("max_bytes must not be negative, got %d", maxBytes)
	}
	if maxBytes == 0 {
		maxBytes = defaultLogBytes
	}
	if maxBytes > maxLogBytes {
		return logRequest{}, false, fmt.Errorf("max_bytes is at most %d, got %d", maxLogBytes, maxBytes)
	}

	wanted := defaultSections
	if len(sections) > 0 {
		wanted = make([]string, 0, len(sections))
		for _, s := range sections {
			name := strings.ToLower(strings.TrimSpace(s))
			if !slices.Contains(allSections, name) {
				return logRequest{}, false, fmt.Errorf("%q is not a log section; valid sections: %s",
					s, strings.Join(allSections, ", "))
			}
			if !slices.Contains(wanted, name) {
				wanted = append(wanted, name)
			}
		}
	}
	return logRequest{sections: wanted, offset: offset, maxBytes: maxBytes}, true, nil
}

// window returns the part of s from offset up to offset+max bytes, with both
// ends moved back to the start of a UTF-8 character so no character is split.
// Moving both the same way means consecutive pages fit together: a character
// straddling a page end is left out of that page and starts the next one.
func window(s string, offset, max int) string {
	if offset >= len(s) {
		return ""
	}
	start := runeStart(s, offset)
	end := offset + max
	if end >= len(s) {
		return s[start:]
	}
	end = runeStart(s, end)
	if end <= start {
		// A window narrower than one character: return that character
		// rather than nothing, so paging always makes progress.
		_, size := utf8.DecodeRuneInString(s[start:])
		end = start + size
	}
	return s[start:end]
}

// runeStart moves i back to the first byte of the character containing it.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// pager windows each section and remembers whether any had more to give, so
// the result can say where the next page starts.
type pager struct {
	req  logRequest
	more bool
}

// page windows s and returns the window with the full size.
func (p *pager) page(s string) (string, int) {
	if len(s) > p.req.offset+p.req.maxBytes {
		p.more = true
	}
	return window(s, p.req.offset, p.req.maxBytes), len(s)
}

// nextOffset is where the following page starts, or 0 when every section has
// been returned to its end.
func (p *pager) nextOffset() int {
	if !p.more {
		return 0
	}
	return p.req.offset + p.req.maxBytes
}
