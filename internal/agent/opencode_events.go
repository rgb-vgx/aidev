package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// OpenCode's `--format json` output is newline-delimited JSON: one event per
// line, not a single document (docs/research.md §2.4). The envelope is
// {"type":..., "timestamp":..., "sessionID":..., "part":{...}} for most events,
// and carries an "error" object instead of a part when a run fails.
type openCodeEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
	Error     *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
			Ref     string `json:"ref"`
		} `json:"data"`
	} `json:"error"`
}

type openCodeTextPart struct {
	Text string `json:"text"`
}

type openCodeToolPart struct {
	Tool  string `json:"tool"`
	State struct {
		Status string `json:"status"`
	} `json:"state"`
}

type openCodeTokens struct {
	Total     int `json:"total"`
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

type openCodeStepFinishPart struct {
	Reason string         `json:"reason"`
	Tokens openCodeTokens `json:"tokens"`
	Cost   float64        `json:"cost"`
}

// usage is aidev's own summation across the run's steps. OpenCode reports usage
// per step, so neither the first nor the last step is the total; the field names
// say plainly that these are sums rather than a passed-through payload.
type usage struct {
	Steps            int `json:"steps"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// transcript is what aidev extracts from the event stream.
type transcript struct {
	SessionID string

	// Summary is the agent's last text message: its own account of what it did.
	Summary string

	// FinishReason is the reason from the final step, passed through unchanged
	// so that a value aidev does not recognise is still recorded.
	FinishReason string

	ToolCalls     int
	FailedTools   int
	Usage         usage
	Cost          float64
	Errors        []string
	UnknownEvents map[string]int

	// OversizedLines counts lines longer than maxEventLine, which were dropped.
	OversizedLines int

	// Lines counts parsed lines, which distinguishes "the agent said nothing"
	// from "the agent was never reached".
	Lines int
}

// maxEventLine bounds how much of one line the scanner holds. Real OpenCode
// events are a few kilobytes; a line longer than this is not an event aidev can
// use, and holding it would break the promise that output is bounded.
const maxEventLine = 1 << 20

// eventScanner parses the NDJSON stream as it arrives.
//
// It is wired as a Tee on stdout rather than parsing the captured buffer
// afterwards, so that events are still extracted when output exceeds the capture
// cap and the tail is discarded. The Tee sits in front of that cap, so the
// scanner bounds itself: see maxEventLine.
type eventScanner struct {
	buf    bytes.Buffer
	result transcript

	// skipping is set while the rest of an oversized line is being discarded.
	skipping bool
}

func newEventScanner() *eventScanner {
	return &eventScanner{result: transcript{UnknownEvents: map[string]int{}}}
}

// Write implements io.Writer. Partial lines are held until their newline
// arrives, because a process write can split a JSON object anywhere. A line
// that grows past maxEventLine is dropped whole, up to and including its
// newline, and counted in OversizedLines.
func (s *eventScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if s.skipping {
			if end < 0 {
				return n, nil
			}
			s.skipping = false
			p = p[end+1:]
			continue
		}
		if end < 0 {
			if s.buf.Len()+len(p) > maxEventLine {
				s.dropLine()
				s.skipping = true
				return n, nil
			}
			s.buf.Write(p)
			return n, nil
		}
		if s.buf.Len()+end > maxEventLine {
			s.dropLine()
		} else {
			s.buf.Write(p[:end])
			s.consume(s.buf.Bytes())
			s.buf.Reset()
		}
		p = p[end+1:]
	}
	return n, nil
}

func (s *eventScanner) dropLine() {
	s.buf.Reset()
	s.result.OversizedLines++
}

// Close processes a trailing line with no newline, which happens when a process
// is killed mid-write. The tail of a dropped line is not a line.
func (s *eventScanner) Close() {
	if s.buf.Len() > 0 && !s.skipping {
		s.consume(s.buf.Bytes())
	}
	s.buf.Reset()
	s.skipping = false
}

// Transcript returns what has been parsed so far.
func (s *eventScanner) Transcript() transcript { return s.result }

func (s *eventScanner) consume(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}

	var ev openCodeEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		// A line that is not JSON is noise, not a reason to fail the task. It is
		// counted so that a malformed stream is visible in the record.
		s.result.UnknownEvents["unparseable"]++
		return
	}
	s.result.Lines++

	if ev.SessionID != "" {
		s.result.SessionID = ev.SessionID
	}

	switch ev.Type {
	case "text":
		var part openCodeTextPart
		if json.Unmarshal(ev.Part, &part) == nil && strings.TrimSpace(part.Text) != "" {
			// Later text supersedes earlier: the final message is the agent's
			// conclusion.
			s.result.Summary = strings.TrimSpace(part.Text)
		}

	case "tool_use":
		s.result.ToolCalls++
		var part openCodeToolPart
		if json.Unmarshal(ev.Part, &part) == nil {
			if part.State.Status != "" && part.State.Status != "completed" {
				s.result.FailedTools++
			}
		}

	case "step_finish":
		var part openCodeStepFinishPart
		if json.Unmarshal(ev.Part, &part) == nil {
			s.result.Usage.Steps++
			s.result.Usage.InputTokens += part.Tokens.Input
			s.result.Usage.OutputTokens += part.Tokens.Output
			s.result.Usage.ReasoningTokens += part.Tokens.Reasoning
			s.result.Usage.CacheReadTokens += part.Tokens.Cache.Read
			s.result.Usage.CacheWriteTokens += part.Tokens.Cache.Write
			s.result.Cost += part.Cost
			if part.Reason != "" {
				s.result.FinishReason = part.Reason
			}
		}

	case "error":
		message := "the agent reported an error"
		if ev.Error != nil {
			name := ev.Error.Name
			if name == "" {
				name = "error"
			}
			message = name
			if ev.Error.Data.Message != "" {
				message += ": " + ev.Error.Data.Message
			}
			if ev.Error.Data.Ref != "" {
				message += fmt.Sprintf(" (ref %s)", ev.Error.Data.Ref)
			}
		}
		s.result.Errors = append(s.result.Errors, message)

	case "step_start":
		// Nothing to extract; named so it is not counted as unknown.

	default:
		// Unknown event types are counted and ignored. OpenCode may add events,
		// and failing a task because its agent learned a new trick would be the
		// wrong response.
		s.result.UnknownEvents[ev.Type]++
	}
}
