package integration

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"aidev/internal/task"
)

// recordSpans installs a global tracer provider that keeps every finished span in
// memory, and restores the previous one afterwards.
//
// In memory rather than against a collector on purpose: this test asserts the shape
// of what aidev emits, which is a property of aidev. Whether a backend displays it
// is a separate question, answered by pointing at Jaeger by hand.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return recorder
}

// spansByName indexes recorded spans, so assertions read by name rather than by
// position and do not break when an unrelated span is added.
func spansByName(recorder *tracetest.SpanRecorder) map[string][]sdktrace.ReadOnlySpan {
	out := map[string][]sdktrace.ReadOnlySpan{}
	for _, span := range recorder.Ended() {
		out[span.Name()] = append(out[span.Name()], span)
	}
	return out
}

func attr(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.Emit(), true
		}
	}
	return "", false
}

func requireAttr(t *testing.T, span sdktrace.ReadOnlySpan, key string) string {
	t.Helper()
	value, ok := attr(span, key)
	if !ok {
		var have []string
		for _, kv := range span.Attributes() {
			have = append(have, string(kv.Key))
		}
		t.Fatalf("span %q has no attribute %q; it has: %s", span.Name(), key, strings.Join(have, ", "))
	}
	return value
}

// A successful task run must produce one trace describing the whole pipeline, with
// the agent invocation carrying the usage a backend needs to show cost.
func TestTaskRunIsTraced(t *testing.T) {
	recorder := recordSpans(t)

	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	h.backend.Summary = "Created marker.txt"
	h.backend.SessionID = "ses_traced"
	h.backend.FinishReason = "stop"
	cost := 0.0025
	h.backend.Cost = &cost
	h.backend.Tokens = []byte(`{"steps":2,"input_tokens":8511,"output_tokens":86}`)

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: %s", outcome.Task.Status, outcome.Message)
	}

	byName := spansByName(recorder)
	for _, want := range []string{
		"aidev.task.run",
		"aidev.worktree.create",
		"aidev.agent.run",
		"aidev.verification",
		"aidev.verification.step",
	} {
		if len(byName[want]) == 0 {
			var have []string
			for name := range byName {
				have = append(have, name)
			}
			t.Fatalf("no span named %q was recorded; recorded: %s", want, strings.Join(have, ", "))
		}
	}

	// The root span identifies the task, so a trace can be found from a task
	// reference and the other way round.
	root := byName["aidev.task.run"][0]
	if got := requireAttr(t, root, "aidev.task.ref"); got != created.Ref {
		t.Errorf("aidev.task.ref = %q, want %q", got, created.Ref)
	}
	requireAttr(t, root, "aidev.task.id")
	requireAttr(t, root, "aidev.attempt.number")
	if got := requireAttr(t, root, "aidev.task.status"); got != "SUCCEEDED" {
		t.Errorf("aidev.task.status = %q, want SUCCEEDED", got)
	}

	// Everything belongs to one trace, or a reader sees four unrelated fragments.
	for name, spans := range byName {
		for _, span := range spans {
			if span.SpanContext().TraceID() != root.SpanContext().TraceID() {
				t.Errorf("span %q is in a different trace from the run", name)
			}
		}
	}
	if root.Parent().IsValid() {
		t.Error("aidev.task.run has a parent; it should be the root of its trace")
	}

	// The agent span is what a backend renders as an LLM call, so it carries the
	// gen_ai attributes as well as aidev's own.
	agent := byName["aidev.agent.run"][0]
	if !agent.Parent().SpanID().IsValid() {
		t.Error("aidev.agent.run has no parent")
	}
	if got := requireAttr(t, agent, "aidev.agent.backend"); got != "fake" {
		t.Errorf("aidev.agent.backend = %q, want fake", got)
	}
	if got := requireAttr(t, agent, "gen_ai.usage.input_tokens"); got != "8511" {
		t.Errorf("gen_ai.usage.input_tokens = %q, want 8511", got)
	}
	if got := requireAttr(t, agent, "gen_ai.usage.output_tokens"); got != "86" {
		t.Errorf("gen_ai.usage.output_tokens = %q, want 86", got)
	}
	if got := requireAttr(t, agent, "gen_ai.usage.cost"); !strings.HasPrefix(got, "0.0025") {
		t.Errorf("gen_ai.usage.cost = %q, want 0.0025", got)
	}
	if got := requireAttr(t, agent, "langfuse.observation.type"); got != "generation" {
		t.Errorf("langfuse.observation.type = %q, want generation so a backend renders it as an LLM call", got)
	}
	requireAttr(t, agent, "aidev.agent.session_id")

	// Verification is the step that decides the outcome, so its result must be
	// visible in the trace without reading the database.
	verification := byName["aidev.verification"][0]
	if got := requireAttr(t, verification, "aidev.verification.passed"); got != "true" {
		t.Errorf("aidev.verification.passed = %q, want true", got)
	}
	step := byName["aidev.verification.step"][0]
	requireAttr(t, step, "aidev.verification.command")
	if got := requireAttr(t, step, "aidev.verification.status"); got != "PASSED" {
		t.Errorf("step status = %q, want PASSED", got)
	}
	if got := requireAttr(t, step, "aidev.verification.exit_code"); got != "0" {
		t.Errorf("step exit code = %q, want 0", got)
	}
}

// A failed task must be as legible in a trace as a successful one, and the span
// status must mark the failure so a backend's error view finds it.
func TestFailedTaskRunIsTracedAsAnError(t *testing.T) {
	recorder := recordSpans(t)

	h := newHarness(t, nil)
	h.backend.Work = nil // reports success, changes nothing
	h.backend.Summary = "All done! Tests pass."

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED", outcome.Task.Status)
	}

	byName := spansByName(recorder)
	root := byName["aidev.task.run"]
	if len(root) == 0 {
		t.Fatal("a failed run produced no aidev.task.run span")
	}
	if got := requireAttr(t, root[0], "aidev.task.status"); got != "FAILED" {
		t.Errorf("aidev.task.status = %q, want FAILED", got)
	}
	if got := requireAttr(t, root[0], "aidev.failure_kind"); got != "VERIFICATION" {
		t.Errorf("aidev.failure_kind = %q, want VERIFICATION", got)
	}
	if root[0].Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error so a backend's error view finds it", root[0].Status().Code)
	}

	step := byName["aidev.verification.step"]
	if len(step) == 0 {
		t.Fatal("no verification step span")
	}
	if got := requireAttr(t, step[0], "aidev.verification.status"); got != "FAILED" {
		t.Errorf("step status = %q, want FAILED", got)
	}
}

// Tracing must be entirely optional: with no provider configured the run behaves
// identically, which is what the no-op tracer is for.
func TestRunWorksWithoutTracing(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask without tracing: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Errorf("status = %s, want SUCCEEDED", outcome.Task.Status)
	}
}
