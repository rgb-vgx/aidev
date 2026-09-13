package tracing

import (
	"context"
	"testing"
)

// lookupFor builds a lookup func over a fixed map so tests need no global state.
func lookupFor(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// FromEnv without an endpoint must leave tracing disabled, so plain local runs
// never attempt an export.
func TestFromEnvDisabledWithoutEndpoint(t *testing.T) {
	cfg, err := FromEnv(lookupFor(nil))
	if err != nil {
		t.Fatalf("FromEnv with empty environment: %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("Enabled = true without an endpoint, want false")
	}
	if cfg.ServiceName != "aidev" {
		t.Fatalf("ServiceName = %q, want default %q", cfg.ServiceName, "aidev")
	}
	if cfg.SampleRatio != 1 {
		t.Fatalf("SampleRatio = %v, want default 1", cfg.SampleRatio)
	}
}

// Header parsing must carry authentication material such as an Authorization
// bearer token through to the exporter.
func TestFromEnvParsesHeaders(t *testing.T) {
	cfg, err := FromEnv(lookupFor(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://example.com/otel",
		"OTEL_EXPORTER_OTLP_HEADERS":  "Authorization=Bearer secret,team-id=team_123",
	}))
	if err != nil {
		t.Fatalf("FromEnv with headers: %v", err)
	}
	if !cfg.Enabled {
		t.Fatalf("Enabled = false with an endpoint set, want true")
	}
	if cfg.Headers["Authorization"] != "Bearer secret" {
		t.Fatalf("Authorization header = %q, want %q", cfg.Headers["Authorization"], "Bearer secret")
	}
	if cfg.Headers["team-id"] != "team_123" {
		t.Fatalf("team-id header = %q, want %q", cfg.Headers["team-id"], "team_123")
	}
}

// A header entry that is not key=value must fail fast rather than exporting
// without the intended credentials.
func TestFromEnvRejectsMalformedHeader(t *testing.T) {
	_, err := FromEnv(lookupFor(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://example.com/otel",
		"OTEL_EXPORTER_OTLP_HEADERS":  "not-a-header",
	}))
	if err == nil {
		t.Fatalf("FromEnv with malformed header entry succeeded, want an error")
	}
}

// A sampler argument above 1 is meaningless and must be rejected at startup.
func TestFromEnvRejectsOutOfRangeSampleRatio(t *testing.T) {
	_, err := FromEnv(lookupFor(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://example.com/otel",
		"OTEL_TRACES_SAMPLER_ARG":     "2",
	}))
	if err == nil {
		t.Fatalf("FromEnv with sample ratio 2 succeeded, want an error")
	}
}

// The base endpoint carries no signal path per the OpenTelemetry specification,
// so FromEnv must append /v1/traces; the signal-specific variable is already
// complete and wins verbatim.
func TestFromEnvResolvesTracesEndpoint(t *testing.T) {
	tests := []struct {
		name          string
		env           map[string]string
		wantEnabled   bool
		wantEndpoint  string
		wantTracesURL string
	}{
		{
			name:          "base with path gains signal path",
			env:           map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://host:3000/api/public/otel"},
			wantEnabled:   true,
			wantEndpoint:  "http://host:3000/api/public/otel",
			wantTracesURL: "http://host:3000/api/public/otel/v1/traces",
		},
		{
			name:          "base with trailing slash avoids double slash",
			env:           map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://host:3000/api/public/otel/"},
			wantEnabled:   true,
			wantEndpoint:  "http://host:3000/api/public/otel/",
			wantTracesURL: "http://host:3000/api/public/otel/v1/traces",
		},
		{
			name:          "base without path gains signal path",
			env:           map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://host:4318"},
			wantEnabled:   true,
			wantEndpoint:  "http://host:4318",
			wantTracesURL: "http://host:4318/v1/traces",
		},
		{
			name: "signal-specific endpoint wins verbatim",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":        "http://host:3000/api/public/otel",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://other:4318/custom/path",
			},
			wantEnabled:   true,
			wantEndpoint:  "http://host:3000/api/public/otel",
			wantTracesURL: "http://other:4318/custom/path",
		},
		{
			name:          "signal-specific endpoint alone enables tracing",
			env:           map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://other:4318/v1/traces"},
			wantEnabled:   true,
			wantEndpoint:  "",
			wantTracesURL: "http://other:4318/v1/traces",
		},
		{
			name:          "base query and fragment are preserved",
			env:           map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://host:3000/api/public/otel?key=1#frag"},
			wantEnabled:   true,
			wantEndpoint:  "http://host:3000/api/public/otel?key=1#frag",
			wantTracesURL: "http://host:3000/api/public/otel/v1/traces?key=1#frag",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := FromEnv(lookupFor(tt.env))
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.Enabled != tt.wantEnabled {
				t.Fatalf("Enabled = %v, want %v", cfg.Enabled, tt.wantEnabled)
			}
			if cfg.Endpoint != tt.wantEndpoint {
				t.Fatalf("Endpoint = %q, want %q", cfg.Endpoint, tt.wantEndpoint)
			}
			if cfg.TracesEndpoint != tt.wantTracesURL {
				t.Fatalf("TracesEndpoint = %q, want %q", cfg.TracesEndpoint, tt.wantTracesURL)
			}
		})
	}
}

// A disabled tracer must still produce usable spans so call sites need no
// enabled-check before instrumenting.
func TestStartDisabledReturnsNoopTracer(t *testing.T) {
	tracer, shutdown, err := Start(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Start disabled: %v", err)
	}
	if tracer == nil {
		t.Fatalf("Start disabled returned a nil tracer")
	}
	ctx, span := tracer.Start(context.Background(), "test-operation")
	span.End()
	_ = ctx
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown of disabled tracer: %v", err)
	}
}

// Shutdown on a disabled tracer must be idempotent, since deferred cleanup can
// run after an explicit shutdown.
func TestStartDisabledShutdownTwice(t *testing.T) {
	_, shutdown, err := Start(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Start disabled: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}
