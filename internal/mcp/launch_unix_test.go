//go:build unix

package mcp

import (
	"os"
	"testing"
)

// setsid is what lets the run outlive this server: a new session means no
// controlling terminal, its own process group, and none of this process's
// signals reaching the child (research C2).
func TestNewRunChildOwnsItsSession(t *testing.T) {
	logFile, err := os.CreateTemp(t.TempDir(), "run-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	cmd, closeStdout, err := newRunChild("/usr/bin/aidev", "TASK-000001", logFile)
	if err != nil {
		t.Fatalf("newRunChild: %v", err)
	}
	defer closeStdout()

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("the child would share this server's session and die with it")
	}
}
