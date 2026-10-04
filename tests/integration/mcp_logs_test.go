package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/agent"
)

// Research D1: include_logs once returned every captured byte — up to about
// 6 MiB in one call into a planner's context. Each section is now cut to
// max_bytes, reports its whole size, and pages with offset; the agent's raw
// NDJSON is replaced by default with a plain-text transcript.

// logResult decodes the parts of aidev_get_task_result the paging concerns.
type logResult struct {
	AgentTranscript           string         `json:"agent_transcript"`
	AgentStdout               string         `json:"agent_stdout"`
	AgentStderr               string         `json:"agent_stderr"`
	Diff                      string         `json:"diff"`
	AgentTranscriptTotalBytes int            `json:"agent_transcript_total_bytes"`
	AgentStdoutTotalBytes     int            `json:"agent_stdout_total_bytes"`
	AgentStderrTotalBytes     int            `json:"agent_stderr_total_bytes"`
	DiffTotalBytes            int            `json:"diff_total_bytes"`
	NextOffset                int            `json:"next_offset"`
	Result                    map[string]any `json:"result"`
}

// runNoisyTask runs a task whose agent produced a large OpenCode event stream,
// a long error output and a sizeable diff, and returns its reference and the
// stream as stored.
func runNoisyTask(t *testing.T, m *mcpHarness) (string, string) {
	t.Helper()
	var stream strings.Builder
	for i := range 400 {
		fmt.Fprintf(&stream,
			`{"type":"text","timestamp":1789233986643,"sessionID":"ses_noise","part":{"id":"prt_%04d","messageID":"msg_noise","type":"text","text":"step %d: still working on the marker"}}`+"\n",
			i, i)
	}
	m.backend.BackendName = agent.OpenCodeName
	m.backend.Stdout = stream.String()
	m.backend.Stderr = strings.Repeat("warning: something noisy\n", 400)
	m.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.WorkingDir, "big.txt"),
			[]byte(strings.Repeat("a line of the generated file\n", 500)), 0o600)
	}

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Create the marker file",
		"description":  "Create marker.txt containing the word done",
		"verification": []string{"test -f marker.txt"},
	}, &created)
	ref, _ := created.Task["ref"].(string)

	var run struct {
		Succeeded bool `json:"succeeded"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": ref, "wait_seconds": 60}, &run)
	if !run.Succeeded {
		t.Fatalf("the noisy task did not succeed")
	}
	return ref, stream.String()
}

func TestMCPLogsAreBoundedAndCondensedByDefault(t *testing.T) {
	m := newMCPHarness(t)
	ref, stream := runNoisyTask(t, m)

	var out logResult
	m.call(t, "aidev_get_task_result", map[string]any{"task": ref, "include_logs": true, "max_bytes": 4096}, &out)

	if out.AgentStdout != "" || out.AgentStdoutTotalBytes != 0 {
		t.Error("the raw event stream was returned by default; it is the section D1 keeps out")
	}
	if !strings.Contains(out.AgentTranscript, "[text] step 0: still working on the marker") {
		t.Errorf("transcript does not read as text:\n%.300s", out.AgentTranscript)
	}
	if strings.Contains(out.AgentTranscript, "sessionID") {
		t.Error("the transcript still carries the JSON envelope")
	}
	if out.AgentTranscriptTotalBytes >= len(stream) {
		t.Errorf("transcript total %d bytes, raw stream %d: condensing made nothing smaller",
			out.AgentTranscriptTotalBytes, len(stream))
	}
	for name, section := range map[string]string{
		"transcript": out.AgentTranscript, "stderr": out.AgentStderr, "diff": out.Diff,
	} {
		if len(section) > 4096 {
			t.Errorf("%s is %d bytes, over max_bytes 4096", name, len(section))
		}
		if section == "" {
			t.Errorf("%s is missing from the default sections", name)
		}
	}
	if out.AgentStderrTotalBytes != len(strings.Repeat("warning: something noisy\n", 400)) {
		t.Errorf("agent_stderr_total_bytes = %d, want the whole stored size", out.AgentStderrTotalBytes)
	}
	if out.DiffTotalBytes <= 4096 || !strings.Contains(out.Diff, "marker.txt") && !strings.Contains(out.Diff, "big.txt") {
		t.Errorf("diff = %d of %d bytes; want a cut window of a larger diff", len(out.Diff), out.DiffTotalBytes)
	}
	if out.NextOffset != 4096 {
		t.Errorf("next_offset = %d, want 4096: sections had more past this page", out.NextOffset)
	}
}

// Paging with next_offset gives the whole stored section back, page by page.
func TestMCPLogsPageThroughTheRawStream(t *testing.T) {
	m := newMCPHarness(t)
	ref, stream := runNoisyTask(t, m)

	var all strings.Builder
	offset, pages := 0, 0
	for {
		var out logResult
		m.call(t, "aidev_get_task_result", map[string]any{
			"task": ref, "sections": []string{"stdout"}, "max_bytes": 16384, "offset": offset,
		}, &out)
		if out.AgentTranscript != "" || out.Diff != "" || out.AgentStderr != "" {
			t.Fatal("sections other than stdout were returned")
		}
		if out.AgentStdoutTotalBytes != len(stream) {
			t.Fatalf("agent_stdout_total_bytes = %d, want %d", out.AgentStdoutTotalBytes, len(stream))
		}
		all.WriteString(out.AgentStdout)
		pages++
		if out.NextOffset == 0 {
			break
		}
		if out.NextOffset <= offset || pages > 100 {
			t.Fatalf("next_offset %d does not advance from %d", out.NextOffset, offset)
		}
		offset = out.NextOffset
	}
	if pages < 2 {
		t.Errorf("read the stream in %d page(s); the test needs it to span several", pages)
	}
	if all.String() != stream {
		t.Errorf("pages reassemble to %d bytes, want the stored %d", all.Len(), len(stream))
	}
}

func TestMCPLogsRejectAnUnknownSection(t *testing.T) {
	m := newMCPHarness(t)
	ref, _ := runNoisyTask(t, m)
	msg := m.callExpectingError(t, "aidev_get_task_result", map[string]any{"task": ref, "sections": []string{"everything"}})
	if !strings.Contains(msg, "everything") || !strings.Contains(msg, "transcript") {
		t.Errorf("error = %q, want it to name the bad section and the valid ones", msg)
	}
}
