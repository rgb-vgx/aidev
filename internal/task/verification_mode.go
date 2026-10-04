package task

import (
	"fmt"
	"strings"
)

// VerificationMode says where a task's verification commands run.
//
// It is decided when the task is created and frozen into the task row, so
// verification stays a pure function of the task: changing the project's
// default afterwards cannot move the goalposts under a task that is already
// running, or rewrite what a recorded result was judged against.
type VerificationMode string

const (
	// VerificationInPlace runs the checks in the worktree the agent worked
	// in. The default, and what every task did before the mode existed.
	// Its blind spot is what clean exists for: a file git ignores, or a
	// file the agent created but never added, is present for the checks
	// while being absent from what a commit would carry.
	VerificationInPlace VerificationMode = "in_place"

	// VerificationClean snapshots the post-agent tree, checks it out into
	// a temporary worktree, and runs the checks there. The checks then see
	// exactly the content a commit would carry — no more (untracked-but-
	// ignored files the agent left behind) and no less (files that exist on
	// disk but would never be committed).
	VerificationClean VerificationMode = "clean"
)

// AllVerificationModes lists every mode. Used by validation and by the
// migration's CHECK constraints, which must stay in agreement with this list.
func AllVerificationModes() []VerificationMode {
	return []VerificationMode{
		VerificationInPlace,
		VerificationClean,
	}
}

// Valid reports whether m is a mode aidev knows.
func (m VerificationMode) Valid() bool {
	switch m {
	case VerificationInPlace, VerificationClean:
		return true
	}
	return false
}

// String makes VerificationMode printable.
func (m VerificationMode) String() string { return string(m) }

// ParseVerificationMode converts a user-supplied string into a
// VerificationMode. It is accepted the way a person writes it: trimmed,
// case-insensitive, with a dash reading the same as an underscore. Blank
// means VerificationInPlace, so an unset value is the behaviour that
// existed before the mode was introduced.
func ParseVerificationMode(raw string) (VerificationMode, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return VerificationInPlace, nil
	}
	// The stored spellings are lower-case; the normalisation only decides
	// which one a person's spelling means.
	switch strings.ReplaceAll(strings.ToLower(trimmed), "-", "_") {
	case string(VerificationInPlace):
		return VerificationInPlace, nil
	case string(VerificationClean):
		return VerificationClean, nil
	}
	return "", fmt.Errorf("%q is not a valid verification mode (want one of in_place, clean)", raw)
}

// VerificationPhase says which half of a verification pass a run belongs to.
//
// Setup and verify steps share one global step_index — the results table is
// keyed on it — so the phase is a separate column rather than an encoding
// inside the index: readers that page on step_index see no difference.
type VerificationPhase string

const (
	// PhaseSetup is a command that prepares the checkout (npm ci and
	// friends) before any check runs.
	PhaseSetup VerificationPhase = "setup"

	// PhaseVerify is one of the task's own verification commands.
	PhaseVerify VerificationPhase = "verify"
)

// AllVerificationPhases lists every phase. Used by validation and by the
// migration's CHECK constraint, which must stay in agreement with this list.
func AllVerificationPhases() []VerificationPhase {
	return []VerificationPhase{PhaseSetup, PhaseVerify}
}

// Valid reports whether p is a phase aidev knows. An unset phase reads as
// verify, because that is what every row written before the column existed
// means.
func (p VerificationPhase) Valid() bool {
	switch p {
	case "", PhaseSetup, PhaseVerify:
		return true
	}
	return false
}

// String makes VerificationPhase printable.
func (p VerificationPhase) String() string { return string(p) }
