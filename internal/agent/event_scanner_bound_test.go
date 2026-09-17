package agent

import (
	"bytes"
	"testing"
)

// The scanner holds a partial line until its newline arrives. procexec bounds what
// it captures, but the scanner sits on the Tee in front of that bound, so a process
// that writes one enormous line with no newline — a base64 blob, a runaway print —
// would grow the buffer without limit. The promise that output is bounded has to
// hold here too, and the lines after the monster must still be read.
func TestEventScannerDoesNotHoldAnUnboundedLine(t *testing.T) {
	s := newEventScanner()

	const limit = 8 << 20
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 32; i++ {
		if _, err := s.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if s.buf.Len() > limit {
			t.Fatalf("after %d MiB without a newline the scanner holds %d bytes; it must stay under %d", i+1, s.buf.Len(), limit)
		}
	}

	// The oversized line ends, and ordinary events follow.
	if _, err := s.Write([]byte("\n" + fixtureStepStart + "\n" + fixtureText + "\n" + fixtureStepFinishStop + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Close()

	got := s.Transcript()
	if got.SessionID == "" {
		t.Error("the events after the oversized line were not parsed: no session id")
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want the one from the event after the oversized line", got.FinishReason)
	}
	if got.Summary == "" {
		t.Error("the text event after the oversized line was lost")
	}
}
