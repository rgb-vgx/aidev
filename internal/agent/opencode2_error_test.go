package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"aidev/internal/task"
)

// OpenCode 2 reports a failed run with {"error":{"type":...,"message":...}},
// not 1.x's {"error":{"name":...,"data":{"message":...}}}. Read with the 1.x
// shape, TASK-000149's "provider.no-route: Model unavailable:
// opencode-go/deepseek-v4.1-flash" was recorded as "opencode reported: error".
// That error comes from the environment — a model the run cannot reach — and
// was retried at once, one second and zero tool calls each time; a provider
// that cannot be reached is a startup failure, which aidev does not retry.

// fixtureError2NoRoute is TASK-000149's stdout (attempt 1), verbatim.
const fixtureError2NoRoute = `{"type":"error","timestamp":1793460884991,"sessionID":"ses_e6d66ababffeLtnLETLj1swzEl","error":{"type":"provider.no-route","message":"Model unavailable: opencode-go/deepseek-v4.1-flash"}}`

// fixtureError2Agent is the same shape for an error that is not about
// reaching the provider.
const fixtureError2Agent = `{"type":"error","timestamp":1793460884991,"sessionID":"ses_e6d66ababffeLtnLETLj1swzEl","error":{"type":"session.aborted","message":"The model stopped responding"}}`

func TestOpenCode2ErrorKeepsItsTypeAndMessage(t *testing.T) {
	command, _ := fakeOpenCode(t, emit(fixtureError2Agent)+"exit 1")
	res, err := NewOpenCode(command, "").Run(context.Background(), openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Err == nil {
		t.Fatal("Err = nil, want the reported error")
	}
	for _, want := range []string{"session.aborted", "The model stopped responding"} {
		if !strings.Contains(res.Err.Error(), want) {
			t.Errorf("Err = %q, want it to contain %q", res.Err, want)
		}
	}
	if res.FailureKind != task.FailureAgentError {
		t.Errorf("failure kind = %s, want AGENT_ERROR for an error that is not the provider's", res.FailureKind)
	}
}

// A provider the run cannot reach fails the same way on every attempt, so it
// is a startup failure (never retried) and says which model it could not reach.
func TestOpenCode2ProviderErrorIsAStartupFailure(t *testing.T) {
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprintf("exit %d", exit), func(t *testing.T) {
			command, _ := fakeOpenCode(t, emit(fixtureError2NoRoute)+fmt.Sprintf("exit %d", exit))
			res, err := NewOpenCode(command, "").Run(context.Background(), openCodeRequest(t))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != task.WorkerFailed {
				t.Errorf("status = %s, want FAILED", res.Status)
			}
			if res.FailureKind != task.FailureStartup {
				t.Errorf("failure kind = %s, want STARTUP: a provider that cannot be reached is not fixed by another attempt", res.FailureKind)
			}
			if res.Err == nil || !strings.Contains(res.Err.Error(), "provider.no-route") ||
				!strings.Contains(res.Err.Error(), "Model unavailable: opencode-go/deepseek-v4.1-flash") {
				t.Errorf("Err = %v, want the error type and the model it could not reach", res.Err)
			}
		})
	}
}
