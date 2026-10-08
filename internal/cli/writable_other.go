//go:build !unix

package cli

// userCanWrite cannot tell without POSIX access(2); it assumes the worst, so
// the warning is shown rather than skipped.
func userCanWrite(string) bool { return true }
