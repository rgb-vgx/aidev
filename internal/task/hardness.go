package task

import (
	"fmt"
	"strings"
)

// Hardness is a task's own estimate of how much model it needs, stated once
// when the task is created and then used to pick a model without a person
// naming one every time. It is deliberately a closed, short vocabulary: a
// number would invite false precision, and a free-text label could not be
// routed. Empty means the task did not state one, which is allowed.
type Hardness string

const (
	// HardnessTrivial is work a cheap model can do.
	HardnessTrivial Hardness = "TRIVIAL"

	// HardnessStandard is ordinary work with no routing entry by default.
	HardnessStandard Hardness = "STANDARD"

	// HardnessHard is work that deserves the strong model.
	HardnessHard Hardness = "HARD"
)

// AllHardnesses lists every hardness level. Used by validation and by the
// migration's CHECK constraint, which must stay in agreement with this list.
func AllHardnesses() []Hardness {
	return []Hardness{
		HardnessTrivial,
		HardnessStandard,
		HardnessHard,
	}
}

// Valid reports whether h is a stated hardness level. Empty is not valid: it
// means the task did not state a hardness.
func (h Hardness) Valid() bool {
	switch h {
	case HardnessTrivial, HardnessStandard, HardnessHard:
		return true
	}
	return false
}

// String makes Hardness printable.
func (h Hardness) String() string { return string(h) }

// ParseHardness converts a user-supplied string into a Hardness. It is
// accepted the way a person writes it: trimmed and case-insensitive. Blank
// returns ("", nil) because a task need not state a hardness.
func ParseHardness(raw string) (Hardness, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	h := Hardness(strings.ToUpper(strings.TrimSpace(raw)))
	if !h.Valid() {
		return "", fmt.Errorf("%q is not a valid hardness (want one of TRIVIAL, STANDARD, HARD)", raw)
	}
	return h, nil
}
