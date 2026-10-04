package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"aidev/internal/task"
	"aidev/internal/verification"
)

// finishRootSpan records the final status on the run span. It runs deferred,
// so every path through execute — success, recorded failure, early error,
// cancellation — ends the span with the outcome visible in the trace.
func (r *run) finishRootSpan(root trace.Span, retErr error, outcome Outcome) {
	if r.attempt.AttemptNumber != 0 {
		root.SetAttributes(attribute.Int("aidev.attempt.number", r.attempt.AttemptNumber))
	}
	status := r.task.Status
	if outcome.Task.Status.Valid() && outcome.Task.Status.String() != "" {
		// A recorded outcome carries the authoritative final status.
		status = outcome.Task.Status
	}
	root.SetAttributes(attribute.String("aidev.task.status", string(status)))
	if r.failureKind != task.FailureNone && r.failureKind != "" {
		root.SetAttributes(attribute.String("aidev.failure_kind", string(r.failureKind)))
	}
	if status != task.StatusSucceeded {
		root.SetStatus(codes.Error, rootErrorDescription(status, r.failureKind, retErr))
	}
	root.End()
}

// rootErrorDescription keeps the span status readable: a backend error view
// shows this string, not a full log, so a recorded failure reports only its
// classification while an early error reports the error itself.
func rootErrorDescription(status task.Status, kind task.FailureKind, retErr error) string {
	if retErr != nil {
		return truncateForSpan(retErr.Error())
	}
	if kind != task.FailureNone && kind != "" {
		return fmt.Sprintf("task %s (%s)", status, kind)
	}
	return fmt.Sprintf("task %s", status)
}

// truncateForSpan bounds the error description so a verbose failure cannot
// bloat the span.
func truncateForSpan(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// tokenUsage mirrors the integer fields a backend reports in WorkerRun.Tokens.
// Pointers distinguish "absent" from zero, so only present values become
// attributes.
type tokenUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

// usageAttributes parses the raw token JSON without ever failing the task:
// tracing is observability, not control flow.
func usageAttributes(tokens []byte) []attribute.KeyValue {
	if len(tokens) == 0 {
		return nil
	}
	var usage tokenUsage
	if err := json.Unmarshal(tokens, &usage); err != nil {
		return nil
	}
	var attrs []attribute.KeyValue
	if usage.InputTokens != nil {
		attrs = append(attrs, attribute.Int("gen_ai.usage.input_tokens", *usage.InputTokens))
	}
	if usage.OutputTokens != nil {
		attrs = append(attrs, attribute.Int("gen_ai.usage.output_tokens", *usage.OutputTokens))
	}
	return attrs
}

// setAgentUsageSpan records what the backend reported about the invocation.
// Usage is observability, so a missing or unparseable value only means the
// attribute is omitted, never a task failure.
func (r *run) setAgentUsageSpan(span trace.Span, record *task.WorkerRun) {
	backend := r.o.Backend.Name()
	sessionID := ""
	var tokens []byte
	var cost *float64
	if record != nil {
		if record.Backend != "" {
			backend = record.Backend
		}
		sessionID = record.SessionID
		tokens = record.Tokens
		cost = record.Cost
	}
	attrs := []attribute.KeyValue{
		attribute.String("aidev.agent.backend", backend),
	}
	if sessionID != "" {
		attrs = append(attrs, attribute.String("aidev.agent.session_id", sessionID))
	}
	attrs = append(attrs, usageAttributes(tokens)...)
	if cost != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.usage.cost", *cost))
	}
	span.SetAttributes(attrs...)
}

// traceVerificationSteps emits one span per declared step, including steps that
// were skipped, so a reader can account for every step from the trace alone.
func (r *run) traceVerificationSteps(ctx context.Context, report verification.Report) {
	tracer := otel.Tracer("aidev")
	for _, vr := range report.Runs {
		_, stepSpan := tracer.Start(ctx, "aidev.verification.step")
		attrs := []attribute.KeyValue{
			attribute.String("aidev.verification.command", vr.Command),
			attribute.String("aidev.verification.status", string(vr.Status)),
		}
		if vr.ExitCode != nil {
			attrs = append(attrs, attribute.Int("aidev.verification.exit_code", *vr.ExitCode))
		}
		stepSpan.SetAttributes(attrs...)
		stepSpan.End()
	}
}
