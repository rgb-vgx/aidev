//go:build !linux && !darwin

package cli

import "errors"

// freeSpace is not measured here: aidev ships for Linux and macOS, and the
// doctor skips the disk check rather than guess.
func freeSpace(string) (uint64, error) { return 0, errors.ErrUnsupported }
