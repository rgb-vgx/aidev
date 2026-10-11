// Package coverage tells which of the lines a task changed its tests actually
// ran. Verification says the checks pass; it does not say they exercised the
// new code, and every dogfood round of aidev found defects in code no
// specification ran. This package compares a task's changed lines with a Go
// coverage profile. It only reports: nothing here decides an outcome.
package coverage

import (
	"bufio"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Block is one entry of a Go coverage profile: a range of lines in a file
// and how many times the statements in it ran (0 = never).
type Block struct {
	File      string // import path of the package + "/" + file name
	StartLine int
	EndLine   int
	Count     int
}

// ParseProfile reads the text format `go test -coverprofile` writes:
// a "mode:" line, then "file:startLine.col,endLine.col numStmt count".
func ParseProfile(profile string) ([]Block, error) {
	sc := bufio.NewScanner(strings.NewReader(profile))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var blocks []Block
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if !strings.HasPrefix(line, "mode:") {
				return nil, fmt.Errorf("coverage profile: first line %q is not a mode line", line)
			}
			continue
		}
		colon := strings.LastIndexByte(line, ':')
		if colon < 0 {
			return nil, fmt.Errorf("coverage profile: malformed line %q", line)
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) != 3 {
			return nil, fmt.Errorf("coverage profile: malformed line %q", line)
		}
		start, end, ok := strings.Cut(fields[0], ",")
		if !ok {
			return nil, fmt.Errorf("coverage profile: malformed range in %q", line)
		}
		startLine, err1 := lineOf(start)
		endLine, err2 := lineOf(end)
		count, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("coverage profile: malformed numbers in %q", line)
		}
		blocks = append(blocks, Block{File: line[:colon], StartLine: startLine, EndLine: endLine, Count: count})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if first {
		return nil, fmt.Errorf("coverage profile: empty")
	}
	return blocks, nil
}

// lineOf reads the line of "line.col".
func lineOf(pos string) (int, error) {
	line, _, _ := strings.Cut(pos, ".")
	return strconv.Atoi(line)
}

// AddedLines reads `git diff -U0` output and returns, per file in its new
// version, the line numbers of the lines the diff adds. Deleted files and
// pure deletions contribute nothing: there is no new line to run.
func AddedLines(diff string) map[string][]int {
	out := map[string][]int{}
	file := ""
	next := 0
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			name := strings.TrimPrefix(line, "+++ ")
			if name == "/dev/null" {
				file = ""
			} else {
				file = strings.TrimPrefix(name, "b/")
			}
		case strings.HasPrefix(line, "@@ "):
			// @@ -a,b +c,d @@
			plus := strings.Fields(line)
			if len(plus) < 3 {
				continue
			}
			start, _, _ := strings.Cut(strings.TrimPrefix(plus[2], "+"), ",")
			next, _ = strconv.Atoi(start)
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if file != "" && next > 0 {
				out[file] = append(out[file], next)
				next++
			}
		}
	}
	return out
}

// LineRange is an inclusive range of lines.
type LineRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// File is the comparison for one changed file.
type File struct {
	Path        string      `json:"path"`
	Changed     int         `json:"changed"`
	NotRun      int         `json:"not_run"`
	NotRunLines []LineRange `json:"not_run_lines,omitempty"`
}

// Report is the comparison for a whole change: how many changed executable
// lines there were, how many no test ran, and where.
type Report struct {
	Files   []File `json:"files"`
	Changed int    `json:"changed"`
	NotRun  int    `json:"not_run"`
}

// Compare matches changed lines against coverage blocks. The keys of added
// and the File of each block must name files the same way (Analyze maps the
// profile's import paths to repository paths before calling it). A changed
// line is executable when some block contains it; it ran when any containing
// block ran — a block appears once per test binary, with its own count.
// Lines no block contains (comments, declarations, blank lines) are not
// counted at all.
func Compare(added map[string][]int, blocks []Block) Report {
	byFile := map[string][]Block{}
	for _, b := range blocks {
		byFile[b.File] = append(byFile[b.File], b)
	}
	paths := make([]string, 0, len(added))
	for p := range added {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var report Report
	for _, path := range paths {
		fileBlocks := byFile[path]
		if len(fileBlocks) == 0 {
			continue
		}
		f := File{Path: path}
		var notRun []int
		for _, line := range added[path] {
			inBlock, ran := false, false
			for _, b := range fileBlocks {
				if line >= b.StartLine && line <= b.EndLine {
					inBlock = true
					if b.Count > 0 {
						ran = true
					}
				}
			}
			if !inBlock {
				continue
			}
			f.Changed++
			if !ran {
				f.NotRun++
				notRun = append(notRun, line)
			}
		}
		if f.Changed == 0 {
			continue
		}
		f.NotRunLines = ranges(notRun)
		report.Files = append(report.Files, f)
		report.Changed += f.Changed
		report.NotRun += f.NotRun
	}
	return report
}

// ranges collapses sorted line numbers into inclusive ranges.
func ranges(lines []int) []LineRange {
	sort.Ints(lines)
	var out []LineRange
	for _, l := range lines {
		if n := len(out); n > 0 && out[n-1].To == l-1 {
			out[n-1].To = l
			continue
		}
		out = append(out, LineRange{From: l, To: l})
	}
	return out
}

// ModulePath reads the module path from a go.mod file's content.
func ModulePath(gomod string) string {
	for line := range strings.SplitSeq(gomod, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// Result is what a task's record holds about one attempt's measurement: the
// report, or why it could not be made.
type Result struct {
	Report
	Error string `json:"error,omitempty"`
}
