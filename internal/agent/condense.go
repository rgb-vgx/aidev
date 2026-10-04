package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CondenseTranscript turns a backend's captured NDJSON stdout into plain text
// a reader can follow: one line per message, tool call or error, with the
// envelope, ids and timestamps dropped (research D1). The raw stream is still
// stored and still available; this is the form a planner reads first, because
// the raw form spends most of its bytes on structure nobody needs to see.
//
// It returns the input unchanged, and false, for a backend whose stream it
// does not know: guessing at an unknown format would hide output rather than
// summarise it.
func CondenseTranscript(backend, stdout string) (string, bool) {
	var line func([]byte) (string, bool)
	switch backend {
	case OpenCodeName:
		line = condenseOpenCodeLine
	case CodexName:
		line = condenseCodexLine
	default:
		return stdout, false
	}

	var b strings.Builder
	for raw := range strings.SplitSeq(stdout, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		text, keep := line([]byte(trimmed))
		if !keep {
			continue
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return b.String(), true
}

// condenseOpenCodeLine renders one OpenCode event. A line that is not JSON is
// kept as it is: it is something the agent printed, and dropping it would
// hide exactly the unexpected output a reader is looking for.
func condenseOpenCodeLine(line []byte) (string, bool) {
	var ev openCodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return string(line), true
	}
	switch ev.Type {
	case "text":
		var part openCodeTextPart
		if json.Unmarshal(ev.Part, &part) != nil || strings.TrimSpace(part.Text) == "" {
			return "", false
		}
		return "[text] " + strings.TrimSpace(part.Text), true
	case "tool_use":
		var part openCodeToolPart
		if json.Unmarshal(ev.Part, &part) != nil {
			return "[tool]", true
		}
		out := fmt.Sprintf("[tool] %s %s", part.Tool, part.State.Status)
		if loc := part.location(); loc != "" {
			out += " " + loc
		}
		if part.State.Error != "" {
			out += ": " + firstLine(part.State.Error)
		}
		return out, true
	case "step_finish":
		var part openCodeStepFinishPart
		if json.Unmarshal(ev.Part, &part) != nil || part.Reason == "" {
			return "[step finished]", true
		}
		return "[step finished] " + part.Reason, true
	case "step_start":
		return "", false
	case "error":
		if ev.Error == nil {
			return "[error]", true
		}
		out := "[error] " + ev.Error.Name
		if ev.Error.Data.Message != "" {
			out += ": " + ev.Error.Data.Message
		}
		return out, true
	default:
		return "[" + ev.Type + "]", true
	}
}

// condenseCodexLine renders one `codex exec --json` event.
func condenseCodexLine(line []byte) (string, bool) {
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return string(line), true
	}
	switch ev.Type {
	case "item.completed":
		if ev.Item == nil {
			return "", false
		}
		switch ev.Item.Type {
		case "agent_message":
			return "[text] " + strings.TrimSpace(ev.Item.Text), true
		case "command_execution":
			return "[command] " + firstLine(ev.Item.Command), true
		case "error":
			return "[error] " + ev.Item.Message, true
		default:
			return "[" + ev.Item.Type + "]", true
		}
	case "turn.failed":
		if ev.Error != nil && ev.Error.Message != "" {
			return "[error] " + ev.Error.Message, true
		}
		return "[error]", true
	case "thread.started", "turn.started", "turn.completed":
		// Lifecycle noise: the completed item carries what happened.
		return "", false
	default:
		return "[" + ev.Type + "]", true
	}
}
