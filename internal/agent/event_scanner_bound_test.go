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

// An event split across many small writes is still one event, however it is cut.
func TestEventScannerJoinsAnEventSplitAcrossWrites(t *testing.T) {
	s := newEventScanner()
	stream := fixtureStepStart + "\n" + fixtureText + "\n" + fixtureStepFinishStop + "\n"
	for i := 0; i < len(stream); i += 7 {
		end := min(i+7, len(stream))
		if _, err := s.Write([]byte(stream[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	got := s.Transcript()
	if got.Lines != 3 || got.FinishReason != "stop" || got.OversizedLines != 0 {
		t.Errorf("lines = %d, finish = %q, oversized = %d; want 3, stop, 0", got.Lines, got.FinishReason, got.OversizedLines)
	}
}

// An oversized line that ends inside the same write is dropped, and what follows
// its newline in that write is read; the drop is counted.
func TestEventScannerDropsAnOversizedLineInOneWrite(t *testing.T) {
	s := newEventScanner()
	payload := append(bytes.Repeat([]byte("y"), 2*maxEventLine), '\n')
	payload = append(payload, []byte(fixtureStepStart+"\n")...)
	if _, err := s.Write(payload); err != nil {
		t.Fatal(err)
	}
	// A tail with no newline after a dropped line is still dropped at Close.
	if _, err := s.Write(bytes.Repeat([]byte("z"), maxEventLine+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	got := s.Transcript()
	if got.SessionID == "" {
		t.Error("the event after the oversized line was not read")
	}
	if got.OversizedLines != 2 {
		t.Errorf("oversized lines = %d, want 2", got.OversizedLines)
	}
	if got.Lines != 1 {
		t.Errorf("lines = %d, want only the real event counted", got.Lines)
	}
}
