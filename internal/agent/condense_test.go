package agent

import (
	"strings"
	"testing"
)

// The condensed transcript keeps what a reader follows — messages, tool calls
// with where they were aimed, the stop reason, errors — and drops ids,
// timestamps, snapshots and token counts. It runs against the fixtures real
// OpenCode emitted, not an idealised shape.
func TestCondenseOpenCodeTranscript(t *testing.T) {
	stream := strings.Join([]string{
		fixtureStepStart, fixtureToolUse, fixtureStepFinishTools,
		"a line the agent printed that is not JSON",
		fixtureText, fixtureStepFinishStop, fixtureError, fixtureRefusedRead,
	}, "\n") + "\n"

	got, ok := CondenseTranscript(OpenCodeName, stream)
	if !ok {
		t.Fatal("an OpenCode stream was not condensed")
	}
	want := []string{
		"[tool] write completed /tmp/wt/greet.go",
		"[step finished] tool-calls",
		"a line the agent printed that is not JSON",
		"[text] Done. Created `greet.go` with package main and Greet function.",
		"[step finished] stop",
		"[error] UnknownError: Unexpected server error. Check server logs for details.",
		"[tool] read error /home/thuyetmt/work/pingpong/ocr-service/Makefile: The user rejected permission to use this specific tool call.",
	}
	if got != strings.Join(want, "\n")+"\n" {
		t.Errorf("condensed transcript =\n%s\nwant\n%s", got, strings.Join(want, "\n"))
	}
	for _, noise := range []string{"sessionID", "timestamp", "snapshot", "prt_", "tokens"} {
		if strings.Contains(got, noise) {
			t.Errorf("condensed transcript still carries %q:\n%s", noise, got)
		}
	}
	if len(got) >= len(stream)/3 {
		t.Errorf("condensed %d bytes from %d; the point is to be much smaller", len(got), len(stream))
	}
}

func TestCondenseCodexTranscript(t *testing.T) {
	stream := strings.Join([]string{
		codexTurnStarted, codexWarningItem, codexAgentMessage, codexCommandItem,
		codexFinalMessage, codexTurnCompleted, codexTurnFailed,
	}, "\n")

	got, ok := CondenseTranscript(CodexName, stream)
	if !ok {
		t.Fatal("a Codex stream was not condensed")
	}
	for _, want := range []string{
		"[error] Model metadata for",
		"[text] Got it — creating `greet.go` now.",
		`[command] /bin/bash -lc "cat > greet.go"`,
		"[text] Created `greet.go` in the current directory.",
		"[error] the model refused the request",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("condensed transcript lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "turn.started") || strings.Contains(got, "input_tokens") {
		t.Errorf("lifecycle noise was kept:\n%s", got)
	}
}

// A backend whose stream aidev does not know is returned as it is: guessing
// at a format would hide output instead of summarising it.
func TestCondenseUnknownBackendPassesThrough(t *testing.T) {
	got, ok := CondenseTranscript("fake", "whatever it printed\n")
	if ok || got != "whatever it printed\n" {
		t.Errorf("CondenseTranscript(fake) = %q, %v; want the input unchanged and false", got, ok)
	}
}
