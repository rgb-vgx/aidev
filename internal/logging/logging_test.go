package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	log := NewTo(&buf, slog.LevelInfo)
	log.Info("task started", FieldTaskRef, "TASK-000001", FieldExitCode, 0)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log line is not JSON: %v\nline: %s", err, buf.String())
	}
	if record["msg"] != "task started" {
		t.Errorf("msg = %v", record["msg"])
	}
	if record[FieldTaskRef] != "TASK-000001" {
		t.Errorf("%s = %v, want TASK-000001", FieldTaskRef, record[FieldTaskRef])
	}
	if record["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", record["level"])
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := NewTo(&buf, slog.LevelWarn)
	log.Info("suppressed")
	log.Warn("kept")

	out := buf.String()
	if strings.Contains(out, "suppressed") {
		t.Error("an info record was emitted by a warn-level logger")
	}
	if !strings.Contains(out, "kept") {
		t.Error("a warn record was dropped by a warn-level logger")
	}
}

func TestDiscardWritesNothing(t *testing.T) {
	log := Discard()
	// Nothing to assert beyond "does not panic and has no sink"; the value of
	// Discard is that tests can pass a non-nil logger.
	log.Error("ignored")
}

func TestContextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	log := NewTo(&buf, slog.LevelInfo).With(FieldTaskRef, "TASK-000042")

	ctx := Into(context.Background(), log)
	From(ctx).Info("inside")

	if !strings.Contains(buf.String(), "TASK-000042") {
		t.Errorf("correlation field lost through the context: %s", buf.String())
	}
}

func TestFromNeverReturnsNil(t *testing.T) {
	if From(context.Background()) == nil {
		t.Fatal("From returned nil for a context with no logger")
	}
	// A nil context must also be safe: From is called from paths that cannot
	// guarantee one was threaded through. Using a nil variable rather than a
	// literal keeps the intent clear without tripping the linter.
	var missing context.Context
	if From(missing) == nil {
		t.Fatal("From(nil context) returned nil")
	}
}

func TestIntoIgnoresNilLogger(t *testing.T) {
	ctx := Into(context.Background(), nil)
	if From(ctx) == nil {
		t.Fatal("storing a nil logger broke retrieval")
	}
}
