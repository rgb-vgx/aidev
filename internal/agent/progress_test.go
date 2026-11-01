package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// While an agent runs, nothing reaches aidev's record: TASK-000143 has
// task.worker_started at 10:24:29 and task.worker_completed at 10:44:53 with
// nothing in between, and finding out what it was doing meant opening
// OpenCode's own SQLite database. The event stream is already parsed as it
// arrives (eventScanner sits on the process's stdout), so the backend can say
// how far the agent has got while it works: Request.OnProgress.
//
// What a report carries is what the final record already carries — counts,
// the tool's name, the path it was aimed at — and never a bash command line,
// for the reason opencode_events.go gives.

type progressLog struct {
	mu      sync.Mutex
	reports []Progress
	at      []time.Time
}

func (l *progressLog) record(p Progress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reports = append(l.reports, p)
	l.at = append(l.at, time.Now())
}

func (l *progressLog) snapshot() ([]Progress, []time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Progress(nil), l.reports...), append([]time.Time(nil), l.at...)
}

func progressRequest(t *testing.T, log *progressLog, every time.Duration) Request {
	t.Helper()
	req := openCodeRequest(t)
	req.OnProgress = log.record
	req.ProgressInterval = every
	return req
}

// With no interval every tool call is reported, in order, with the running
// counts and where the call was aimed.
func TestOpenCodeReportsEachToolCallAsItHappens(t *testing.T) {
	command, _ := fakeOpenCode(t, emit(
		fixtureStepStart, fixtureToolUse, fixtureToolUse, fixtureRefusedBash,
		fixtureStepFinishTools, fixtureText, fixtureStepFinishStop,
	)+"exit 0")
	log := &progressLog{}
	if _, err := NewOpenCode(command, "").Run(context.Background(), progressRequest(t, log, 0)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reports, _ := log.snapshot()
	if len(reports) != 3 {
		t.Fatalf("got %d progress reports, want 3 (one per tool call): %+v", len(reports), reports)
	}
	for i, r := range reports {
		if r.ToolCalls != i+1 {
			t.Errorf("report %d: ToolCalls = %d, want %d", i, r.ToolCalls, i+1)
		}
	}
	if reports[0].LastTool != "write" || reports[0].LastPath != "/tmp/wt/greet.go" {
		t.Errorf("first report = %+v, want the write to /tmp/wt/greet.go", reports[0])
	}
	last := reports[2]
	if last.LastTool != "bash" || last.LastPath != "/home/thuyetmt/work/pingpong/ocr-service" {
		t.Errorf("last report = %+v, want the bash call and its workdir", last)
	}
	// The command line of a bash call is not part of a report.
	for _, r := range reports {
		if strings.Contains(r.LastPath, "OCR_VLM_API_URL") || strings.Contains(r.LastTool, "grep") {
			t.Errorf("report %+v carries the bash command line", r)
		}
	}
}

// Reports arrive while the agent runs, not when it has finished: that is the
// point of them.
func TestOpenCodeReportsProgressBeforeTheRunEnds(t *testing.T) {
	command, _ := fakeOpenCode(t, emit(fixtureStepStart, fixtureToolUse)+
		"sleep 2\n"+emit(fixtureStepFinishTools, fixtureText, fixtureStepFinishStop)+"exit 0")
	log := &progressLog{}
	if _, err := NewOpenCode(command, "").Run(context.Background(), progressRequest(t, log, 0)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ended := time.Now()
	reports, at := log.snapshot()
	if len(reports) == 0 {
		t.Fatal("no progress was reported")
	}
	if gap := ended.Sub(at[0]); gap < time.Second {
		t.Errorf("first report came %s before the run ended, want it while the agent was still working (about 2s before)", gap)
	}
}

// An interval bounds how often a busy agent is reported: the first tool call
// always, then at most one report per interval.
func TestOpenCodeProgressIntervalBoundsReports(t *testing.T) {
	command, _ := fakeOpenCode(t, emit(
		fixtureStepStart, fixtureToolUse, fixtureToolUse, fixtureToolUse, fixtureToolUse,
		fixtureStepFinishTools, fixtureText, fixtureStepFinishStop,
	)+"exit 0")
	log := &progressLog{}
	if _, err := NewOpenCode(command, "").Run(context.Background(), progressRequest(t, log, time.Hour)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reports, _ := log.snapshot()
	if len(reports) != 1 || reports[0].ToolCalls != 1 {
		t.Errorf("reports = %+v, want exactly the first tool call within one interval", reports)
	}
}

// No callback, no reports, and nothing else changes.
func TestOpenCodeRunsWithoutAProgressCallback(t *testing.T) {
	command, _ := fakeOpenCode(t, emit(fixtureStepStart, fixtureToolUse, fixtureStepFinishTools, fixtureText, fixtureStepFinishStop)+"exit 0")
	res, err := NewOpenCode(command, "").Run(context.Background(), openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", res.ToolCalls)
	}
}
