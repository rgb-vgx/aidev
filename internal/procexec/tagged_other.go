//go:build !linux

package procexec

// taggedProcesses finds nothing where there is no /proc to read: a descendant
// that left the child's process group is then out of reach, as it always was.
func taggedProcesses(string) []int { return nil }
