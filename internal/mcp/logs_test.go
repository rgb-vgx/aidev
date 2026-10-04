package mcp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// No logs asked for means no logs: the plain result stays small.
func TestParseLogRequestDefaults(t *testing.T) {
	if _, ok, err := parseLogRequest(false, nil, 0, 0); ok || err != nil {
		t.Fatalf("no include_logs and no sections = ok %v, err %v; want nothing requested", ok, err)
	}

	req, ok, err := parseLogRequest(true, nil, 0, 0)
	if err != nil || !ok {
		t.Fatalf("include_logs = ok %v, err %v", ok, err)
	}
	if req.maxBytes != defaultLogBytes {
		t.Errorf("max_bytes default = %d, want %d", req.maxBytes, defaultLogBytes)
	}
	if req.wants(sectionStdout) {
		t.Error("the raw stdout is returned by default; it is the multi-megabyte section D1 exists to avoid")
	}
	for _, s := range []string{sectionTranscript, sectionStderr, sectionDiff, sectionVerification} {
		if !req.wants(s) {
			t.Errorf("default sections lack %s", s)
		}
	}
}

// Naming a section implies include_logs, and is normalised and deduplicated.
func TestParseLogRequestSections(t *testing.T) {
	req, ok, err := parseLogRequest(false, []string{" Diff ", "diff", "stdout"}, 10, 100)
	if err != nil || !ok {
		t.Fatalf("sections without include_logs = ok %v, err %v; want logs", ok, err)
	}
	if len(req.sections) != 2 || !req.wants(sectionDiff) || !req.wants(sectionStdout) {
		t.Errorf("sections = %v, want [diff stdout]", req.sections)
	}
	if req.offset != 10 || req.maxBytes != 100 {
		t.Errorf("offset/max_bytes = %d/%d, want 10/100", req.offset, req.maxBytes)
	}
}

func TestParseLogRequestRejectsBadInput(t *testing.T) {
	cases := []struct {
		name     string
		sections []string
		offset   int
		max      int
		mention  string
	}{
		{"unknown section", []string{"everything"}, 0, 0, "transcript"},
		{"negative offset", nil, -1, 0, "offset"},
		{"negative max", nil, 0, -5, "max_bytes"},
		{"max over the cap", nil, 0, maxLogBytes + 1, "max_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseLogRequest(true, tc.sections, tc.offset, tc.max)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error = %v, want it to mention %q", err, tc.mention)
			}
		})
	}
}

func TestWindow(t *testing.T) {
	cases := []struct {
		s           string
		offset, max int
		want        string
	}{
		{"abcdef", 0, 3, "abc"},
		{"abcdef", 3, 3, "def"},
		{"abcdef", 4, 10, "ef"},
		{"abcdef", 6, 3, ""},
		{"abcdef", 99, 3, ""},
		{"", 0, 3, ""},
	}
	for _, tc := range cases {
		if got := window(tc.s, tc.offset, tc.max); got != tc.want {
			t.Errorf("window(%q, %d, %d) = %q, want %q", tc.s, tc.offset, tc.max, got, tc.want)
		}
	}
}

// Pages never split a character, and consecutive pages put together give
// back the whole text — the property a caller paging with next_offset needs.
func TestWindowPagesReassembleMultibyteText(t *testing.T) {
	text := strings.Repeat("aé日🙂", 50)
	for _, size := range []int{1, 2, 3, 5, 7, 64} {
		var b strings.Builder
		for offset := 0; offset < len(text); offset += size {
			page := window(text, offset, size)
			if !utf8.ValidString(page) {
				t.Fatalf("size %d offset %d: page %q splits a character", size, offset, page)
			}
			b.WriteString(page)
		}
		if size >= 4 && b.String() != text {
			t.Errorf("size %d: pages do not reassemble the text", size)
		}
	}
}

func TestPagerNextOffset(t *testing.T) {
	p := &pager{req: logRequest{offset: 0, maxBytes: 4}}
	if got, total := p.page("abc"); got != "abc" || total != 3 {
		t.Errorf("short section = %q/%d", got, total)
	}
	if p.nextOffset() != 0 {
		t.Errorf("next_offset = %d after a section that fit, want 0", p.nextOffset())
	}
	if got, total := p.page("abcdefgh"); got != "abcd" || total != 8 {
		t.Errorf("long section = %q/%d", got, total)
	}
	if p.nextOffset() != 4 {
		t.Errorf("next_offset = %d, want 4", p.nextOffset())
	}
}
