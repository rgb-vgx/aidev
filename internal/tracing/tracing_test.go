package tracing

import (
	"context"
	"testing"
)

// Tracing is configured in conf.json, not through OTEL_* variables (decided with the
// user on 2026-09-15). The rules the environment reader enforced carry over.

func ratio(v float64) *float64 { return &v }

// No endpoint leaves tracing disabled, so plain local runs never attempt an export.
func TestFromSettingsDisabledWithoutEndpoint(t *testing.T) {
	cfg, err := FromSettings(Settings{})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled = true without an endpoint, want false")
	}
	if cfg.ServiceName != "aidev" {
		t.Errorf("ServiceName = %q, want default %q", cfg.ServiceName, "aidev")
	}
	if cfg.SampleRatio != 1 {
		t.Errorf("SampleRatio = %v, want default 1", cfg.SampleRatio)
	}
}

// Headers carry authentication such as a bearer token through to the exporter.
func TestFromSettingsCarriesHeaders(t *testing.T) {
	cfg, err := FromSettings(Settings{
		Endpoint: "https://example.com/otel",
		Headers:  map[string]string{"Authorization": "Bearer secret", "team-id": "team_123"},
	})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("Enabled = false with an endpoint set, want true")
	}
	if cfg.Headers["Authorization"] != "Bearer secret" || cfg.Headers["team-id"] != "team_123" {
		t.Errorf("headers = %v", cfg.Headers)
	}
}

// A header with no name cannot be sent; failing at startup beats exporting without
// the intended credentials.
func TestFromSettingsRejectsAnEmptyHeaderName(t *testing.T) {
	if _, err := FromSettings(Settings{Endpoint: "https://example.com/otel", Headers: map[string]string{"": "x"}}); err == nil {
		t.Fatal("an empty header name was accepted")
	}
}

func TestFromSettingsRejectsOutOfRangeSampleRatio(t *testing.T) {
	if _, err := FromSettings(Settings{Endpoint: "https://example.com/otel", SampleRatio: ratio(2)}); err == nil {
		t.Fatal("sample ratio 2 was accepted")
	}
}

// An explicit 0 means sample nothing; only an absent ratio takes the default of 1.
func TestFromSettingsKeepsAnExplicitZeroSampleRatio(t *testing.T) {
	cfg, err := FromSettings(Settings{Endpoint: "https://example.com/otel", SampleRatio: ratio(0)})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	if cfg.SampleRatio != 0 {
		t.Errorf("SampleRatio = %v, want the explicit 0", cfg.SampleRatio)
	}
}

// The base endpoint carries no signal path per the OpenTelemetry specification, so
// /v1/traces is appended; a traces endpoint is already complete and wins verbatim.
func TestFromSettingsResolvesTracesEndpoint(t *testing.T) {
	tests := []struct {
		name          string
		settings      Settings
		wantEndpoint  string
		wantTracesURL string
	}{
		{"base with path gains signal path",
			Settings{Endpoint: "http://host:3000/api/public/otel"},
			"http://host:3000/api/public/otel", "http://host:3000/api/public/otel/v1/traces"},
		{"base with trailing slash avoids double slash",
			Settings{Endpoint: "http://host:3000/api/public/otel/"},
			"http://host:3000/api/public/otel/", "http://host:3000/api/public/otel/v1/traces"},
		{"base without path gains signal path",
			Settings{Endpoint: "http://host:4318"},
			"http://host:4318", "http://host:4318/v1/traces"},
		{"traces endpoint wins verbatim",
			Settings{Endpoint: "http://host:3000/api/public/otel", TracesEndpoint: "http://other:4318/custom/path"},
			"http://host:3000/api/public/otel", "http://other:4318/custom/path"},
		{"traces endpoint alone enables tracing",
			Settings{TracesEndpoint: "http://other:4318/v1/traces"},
			"", "http://other:4318/v1/traces"},
		{"base query and fragment are preserved",
			Settings{Endpoint: "http://host:3000/api/public/otel?key=1#frag"},
			"http://host:3000/api/public/otel?key=1#frag", "http://host:3000/api/public/otel/v1/traces?key=1#frag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := FromSettings(tt.settings)
			if err != nil {
				t.Fatalf("FromSettings: %v", err)
			}
			if !cfg.Enabled {
				t.Fatal("Enabled = false, want true")
			}
			if cfg.Endpoint != tt.wantEndpoint || cfg.TracesEndpoint != tt.wantTracesURL {
				t.Errorf("endpoint, traces = %q, %q; want %q, %q", cfg.Endpoint, cfg.TracesEndpoint, tt.wantEndpoint, tt.wantTracesURL)
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
