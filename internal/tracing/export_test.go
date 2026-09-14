package tracing

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// The unit tests cover the disabled path only, which proves nothing about whether
// a span ever leaves the process. This test does.
//
// What it establishes, precisely: the exporter posted to a URL the backend accepts
// and the backend did not reject the batch. That is enough to catch a wrong signal
// path, a wrong host, and bad credentials, which is what it is for.
//
// What it does NOT establish: that the trace becomes visible in whatever UI sits on
// top. Ingestion beyond the HTTP response is the backend's business and can fail
// afterwards — with self-hosted Langfuse it was observed accepting a batch with 200,
// storing it in object storage, and never surfacing it, because its own ingestion
// queue was never fed. Conflating "accepted" with "visible" cost real time, so the
// distinction is written down here rather than left to be rediscovered.
//
// It must not judge success by whether shutdown returns an error. That was tried,
// and it is wrong: a batch span processor reports an export failure to the global
// OpenTelemetry error handler and Shutdown still returns nil, so the first version
// of this test passed while every span was being refused with HTTP 404. Collecting
// what the error handler receives is what actually detects a failed export.
//
// It is opt-in so that `make check` never needs a collector. Run it with:
//
//	make langfuse-up
//	eval "$(make langfuse-env)"
//	AIDEV_TEST_OTLP=http://localhost:4318 go test ./internal/tracing/ -run TestExportReachesTheBackend -v
//
// It is deliberately vendor-neutral: any OTLP endpoint will do, and nothing here
// knows about Langfuse.
func TestExportReachesTheBackend(t *testing.T) {
	if os.Getenv("AIDEV_TEST_OTLP") == "" {
		t.Skip("set AIDEV_TEST_OTLP to an OTLP base endpoint to exercise a real export")
	}

	cfg, err := FromSettings(Settings{Endpoint: os.Getenv("AIDEV_TEST_OTLP")})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	if !cfg.Enabled {
		t.Skip("OTEL_EXPORTER_OTLP_ENDPOINT is not set")
	}
	t.Logf("exporting to %s as service %q", cfg.Endpoint, cfg.ServiceName)

	// Collect what the SDK reports asynchronously. Installed before Start so that
	// nothing is missed, and read after the flush.
	var (
		mu             sync.Mutex
		exportFailures []error
	)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		exportFailures = append(exportFailures, err)
	}))

	ctx := context.Background()
	tracer, shutdown, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// One span shaped the way aidev will shape a task run, so the attributes a
	// backend needs for an LLM view are exercised too, not only the transport.
	_, span := tracer.Start(ctx, "aidev.test.export")
	span.SetAttributes(
		attribute.String("langfuse.observation.type", "span"),
		attribute.String("aidev.task.ref", "TASK-TEST"),
		attribute.String("gen_ai.request.model", "opencode/muse-spark-1.3-contributor-free"),
		attribute.Int("gen_ai.usage.input_tokens", 123),
		attribute.Int("gen_ai.usage.output_tokens", 45),
	)
	span.End()

	// Shutdown flushes the batch synchronously.
	flushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(exportFailures) > 0 {
		t.Fatalf("the backend refused the export to %s (%d error(s)); first: %v",
			cfg.Endpoint, len(exportFailures), exportFailures[0])
	}
}
