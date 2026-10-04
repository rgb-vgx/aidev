package verification

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// probe stands in for a wildcard run when a changed directory entry has
// collapsed a whole subtree into a single "dir/" line and the pattern must be
// asked whether *something* under that directory would have matched. It is
// deliberately a name no repository commits.
const probe = "__aidev_protected_probe__"

// Violation is one changed path that a task's protected pattern claims. The
// pattern is reported as stored and the path as git reported it, so the audit
// log quotes both sides of the refusal.
type Violation struct {
	Pattern string
	Path    string
}

// NormalizeProtected trims each pattern to its canonical stored form: leading
// and trailing whitespace gone, and one trailing "/" dropped so that "ci/" and
// "ci" are the same protection rather than two patterns that must both be
// written. An empty pattern is a caller mistake and is rejected, not silently
// dropped: a protection list that quietly loses an entry protects less than it
// says it does.
//
// The result is what gets stored; Violations assumes it is talking to
// normalized patterns and validates only defensively.
func NormalizeProtected(patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(patterns))
	for _, raw := range patterns {
		p := normalizePattern(raw)
		if p == "" {
			return nil, fmt.Errorf("a protected path pattern must not be empty or \"/\"")
		}
		if !doublestar.ValidatePattern(p) {
			return nil, fmt.Errorf("protected path %q is not a valid pattern (for example: .env*, migrations/*, docs/**, ci)", raw)
		}
		out = append(out, p)
	}
	// Sorted and unique so the stored list, the event payload and the failure
	// message are stable however the caller collected the patterns.
	slices.Sort(out)
	return slices.Compact(out), nil
}

// Violations reports every (pattern, changed path) pair where the attempt
// touched something the pattern protects. An attempt that trips any of them
// fails verification before a single check runs.
//
// Matching prefers a false refusal to a missed touch, because a refusal is
// reviewable and a missed touch is not:
//
//   - A pattern with no "/" names a path at any depth (gitignore's rule), so
//     "Makefile" protects internal/Makefile and ".env*" protects config/.env.
//   - A pattern with "/" is anchored to the repository root and protects the
//     whole subtree of anything it matches: "migrations" is unanchored yet
//     still protects migrations/0001_init.sql through the ancestor rule, and
//     "docs/*" protects everything under each direct child of docs because the
//     child's contents went with it.
//   - A changed entry ending in "/" is git collapsing an untracked or ignored
//     directory it never listed inside. The directory itself is matched, the
//     probe subjects stand for its contents, and a concrete path resolved from
//     the pattern is accepted when it lands under the directory — so
//     "migrations/*.sql" catches a fresh migrations/ that git reported only as
//     "migrations/". A collapsed entry names no file inside it, so a pattern
//     resolving elsewhere (a bare "*.log" against a collapsed logs/) is not
//     claimed by these rules; every path git listed individually is still
//     covered by the direct and ancestor rules.
//
// An error means a stored pattern could not be parsed at all (the database
// holds something creation did not write); the caller fails closed rather than
// running checks it can no longer be sure were guarded.
func Violations(patterns []string, changed []string) ([]Violation, error) {
	var out []Violation
	for _, stored := range patterns {
		p := normalizePattern(stored)
		if p == "" || !doublestar.ValidatePattern(p) {
			return nil, fmt.Errorf("protected path %q is not a valid pattern; the guard cannot be evaluated", stored)
		}
		anchored := strings.Contains(p, "/")
		// A pattern resolved to one concrete path answers the collapsed
		// directory case: if that path lies under the reported directory, the
		// pattern could have matched inside it.
		concrete := resolveMagic(p)
		for _, c := range changed {
			if matchesProtected(p, anchored, concrete, c) {
				out = append(out, Violation{Pattern: stored, Path: c})
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	// The stored list is usually already sorted by NormalizeProtected, but the
	// database may hold an older order; sort by (pattern, path) so the report
	// never depends on it, then drop duplicates.
	slices.SortFunc(out, func(a, b Violation) int {
		if c := strings.Compare(a.Pattern, b.Pattern); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return slices.CompactFunc(out, func(a, b Violation) bool {
		return a.Pattern == b.Pattern && a.Path == b.Path
	}), nil
}

func normalizePattern(raw string) string {
	return strings.TrimSuffix(strings.TrimSpace(raw), "/")
}

// matchesProtected applies the rules to one (pattern, changed path) pair. The
// pattern is already normalized.
func matchesProtected(pattern string, anchored bool, concrete, changed string) bool {
	base := strings.TrimSuffix(changed, "/")
	if base == "" {
		return false
	}

	// The entry itself: a file path, and for a directory entry both "docs/"
	// and "docs" so a pattern of either shape claims it. A collapsed directory
	// also gets stand-ins for its contents, one and two levels deep, so
	// "docs/*" and "docs/**" claim the whole tree git never listed. The pattern
	// resolved to a concrete path is the precise version of the same question:
	// it is only accepted when it lands under the reported directory, which is
	// what lets "migrations/*.sql" catch a fresh migrations/ reported as
	// "migrations/" while a collapsed node_modules/ does not trip ".env*" —
	// a collapsed entry names no file, so a pattern resolving outside it is
	// speculation, and speculation about a new directory would refuse every
	// attempt that merely created one.
	subjects := []string{changed}
	if base != changed {
		subjects = append(subjects, base, base+"/"+probe, base+"/"+probe+"/"+probe)
		if concrete != pattern && (concrete == base || strings.HasPrefix(concrete, base+"/")) {
			subjects = append(subjects, concrete)
		}
	}
	for _, s := range subjects {
		if matchOne(pattern, anchored, s) {
			return true
		}
	}

	// Everything above the entry: the pattern may protect a directory the
	// entry lives in, and the contents of a protected directory are protected
	// with it.
	for _, a := range ancestors(base) {
		if matchOne(pattern, anchored, a) {
			return true
		}
	}
	return false
}

// matchOne tests one subject against the pattern. An unanchored pattern (no
// "/") matches the subject's final segment, so ".env*" claims config/.env and
// "node_modules" claims packages twice/node_modules as well as the root one.
// A match error means the pattern failed to parse here, which ValidatePattern
// should already have caught: refuse rather than let an unparseable guard wave
// the run through.
func matchOne(pattern string, anchored bool, subject string) bool {
	if anchored {
		ok, err := doublestar.Match(pattern, subject)
		return err != nil || ok
	}
	ok, err := doublestar.Match(pattern, path.Base(subject))
	return err != nil || ok
}

// ancestors lists every directory above the entry, shallowest first:
// "docs/sub/a.md" yields "docs/sub", "docs".
func ancestors(p string) []string {
	var out []string
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		out = append(out, d)
	}
	return out
}

// resolveMagic turns a pattern into one concrete path by rewriting every
// wildcard run inside a segment, so the result still matches the pattern while
// containing no wildcards: "migrations/*.sql" becomes
// "migrations/__aidev_protected_probe__.sql" and "a?c" becomes "axc".
// Literal segments stay as they are.
func resolveMagic(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, seg := range segs {
		if strings.ContainsAny(seg, "*?[") {
			segs[i] = magicPlaceholder(seg)
		}
	}
	return strings.Join(segs, "/")
}

// magicPlaceholder rewrites one wildcard segment into a literal that the
// segment still matches: the literal prefix and suffix around the wildcards
// are kept, "*" runs become the probe, "?" runs become one "x" each (a "?" is
// exactly one character, so the multi-character probe would not fit), and a
// bracket class becomes one character it accepts.
func magicPlaceholder(seg string) string {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		switch seg[i] {
		case '*':
			// Consecutive stars are one run: "**" inside a segment behaves
			// like a single wildcard for matching, and one probe is enough.
			for i+1 < len(seg) && seg[i+1] == '*' {
				i++
			}
			b.WriteString(probe)
		case '?':
			b.WriteByte('x')
		case '[':
			end := classEnd(seg, i)
			b.WriteString(classRepresentative(seg[i:end]))
			i = end - 1
		default:
			b.WriteByte(seg[i])
		}
	}
	return b.String()
}

// classEnd returns the index just past the "]" that closes the class starting
// at i. ValidatePattern has already accepted the pattern, so an unterminated
// class cannot occur; the guard just avoids running off the end regardless.
func classEnd(seg string, i int) int {
	j := i + 1
	if j < len(seg) && (seg[j] == '^' || seg[j] == '!') {
		j++
	}
	if j < len(seg) && seg[j] == ']' {
		j++
	}
	for j < len(seg) && seg[j] != ']' {
		if seg[j] == '\\' {
			j++
		}
		j++
	}
	if j >= len(seg) {
		return len(seg)
	}
	return j + 1
}

// classRepresentative returns one character the bracket class accepts: the
// first literal of a positive class, or a letter outside every excluded
// literal of a negated one. Classes that exclude the obvious candidates fall
// back to the first excluded literal — a wrong probe risks missing a
// collapsed-directory match, not a false one, and the direct rules still cover
// every path git actually listed.
func classRepresentative(class string) string {
	inner := class
	inner = strings.TrimPrefix(inner, "[")
	inner = strings.TrimSuffix(inner, "]")
	negated := false
	if inner != "" && (inner[0] == '^' || inner[0] == '!') {
		negated = true
		inner = inner[1:]
	}
	if inner == "" {
		return "x"
	}

	if !negated {
		if inner[0] == '\\' && len(inner) > 1 {
			return string(inner[1])
		}
		return string(inner[0])
	}
	for _, c := range "abcdefghijklmnopqrstuvwxyz0123456789" {
		if !strings.ContainsRune(inner, c) {
			return string(c)
		}
	}
	return string(inner[0])
}
