package task

import (
	"fmt"
	"strings"
)

// SubmoduleMode says what a project's task worktrees do about git submodules.
//
// It is per project rather than per task because it is a property of the
// repository, not of the work: a repository either keeps its sources in
// submodules or it does not, and a task author should not have to know.
//
// The default is deliberately "do nothing". Populating submodules writes into
// the parent repository's administrative area (it registers a worktree in the
// submodule's own repository), and no single-repository project should pay for
// a feature it cannot use.
type SubmoduleMode string

const (
	// SubmodulesNone leaves submodule directories as git leaves them: empty.
	SubmodulesNone SubmoduleMode = "NONE"

	// SubmodulesReadOnly checks each submodule out in the task's worktree at
	// the commit the parent repository pins, so that verification commands can
	// read the sources. Nothing inside a submodule is ever committed: a task
	// leaves one commit on one branch of one repository (docs/architecture.md).
	SubmodulesReadOnly SubmoduleMode = "READ_ONLY"
)

// AllSubmoduleModes lists every mode. Used by validation and by the migration's
// CHECK constraint, which must stay in agreement with this list.
func AllSubmoduleModes() []SubmoduleMode {
	return []SubmoduleMode{
		SubmodulesNone,
		SubmodulesReadOnly,
	}
}

// Valid reports whether s is a mode aidev knows.
func (s SubmoduleMode) Valid() bool {
	switch s {
	case SubmodulesNone, SubmodulesReadOnly:
		return true
	}
	return false
}

// String makes SubmoduleMode printable.
func (s SubmoduleMode) String() string { return string(s) }

// ParseSubmoduleMode converts a user-supplied string into a SubmoduleMode. It is
// accepted the way a person writes it: trimmed, case-insensitive, and with a
// dash reading the same as an underscore. Blank means SubmodulesNone, so an
// unset value is the safe one.
func ParseSubmoduleMode(raw string) (SubmoduleMode, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return SubmodulesNone, nil
	}
	m := SubmoduleMode(strings.ReplaceAll(strings.ToUpper(trimmed), "-", "_"))
	if !m.Valid() {
		return "", fmt.Errorf("%q is not a valid submodule mode (want one of none, read_only)", raw)
	}
	return m, nil
}
