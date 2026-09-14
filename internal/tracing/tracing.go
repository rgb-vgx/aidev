// Package tracing wires OpenTelemetry tracing for aidev.
//
// Tracing is optional and off unless an OTLP endpoint is configured, so local
// runs and tests never attempt a network connection. When enabled, spans are
// exported over OTLP HTTP, which is what hosted backends such as Langfuse
// accept, avoiding a vendor-specific exporter dependency.
package tracing

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// DefaultServiceName is used when no service name is configured so that spans
// from every aidev process share one service identity in the backend.
const DefaultServiceName = "aidev"

// exporterTimeout bounds each batch export, so a slow or unreachable backend
// cannot block shutdown indefinitely; Shutdown still honours its own context
// deadline on top of this.
const exporterTimeout = 10 * time.Second

// Settings carries the tracing section of conf.json. It is a plain data
// transfer type so that configuration stays in internal/config; the defaults
// and validation live here with the code that uses them.
type Settings struct {
	Endpoint       string
	TracesEndpoint string
	Headers        map[string]string
	ServiceName    string
	SampleRatio    *float64
}

// Config is built from Settings by FromSettings.
type Config struct {
	Enabled        bool
	Endpoint       string            // base OTLP HTTP URL
	TracesEndpoint string            // full URL the traces exporter posts to (<base>/v1/traces, or TracesEndpoint as given)
	Headers        map[string]string // extra OTLP headers, e.g. Authorization
	ServiceName    string            // defaults to "aidev"
	SampleRatio    float64           // 0..1, defaults to 1
}

// FromSettings resolves Settings into a Config. Enabled is true when either
// endpoint sets a non-empty value. TracesEndpoint is the full
// signal-specific URL and is used as given; otherwise TracesEndpoint appends
// /v1/traces to the base endpoint, as the OpenTelemetry specification defines
// the base URL without a signal path. A nil SampleRatio means 1; an explicit
// 0 stays 0. Returns an error for an empty header name or a sample ratio
// outside 0..1.
func FromSettings(s Settings) (Config, error) {
	cfg := Config{
		ServiceName: DefaultServiceName,
		SampleRatio: 1,
	}

	cfg.Endpoint = strings.TrimSpace(s.Endpoint)
	tracesEndpoint := strings.TrimSpace(s.TracesEndpoint)
	switch {
	case tracesEndpoint != "":
		// Signal-specific URL takes precedence and is used verbatim.
		cfg.TracesEndpoint = tracesEndpoint
	case cfg.Endpoint != "":
		cfg.TracesEndpoint = resolveTracesEndpoint(cfg.Endpoint)
	}
	cfg.Enabled = cfg.Endpoint != "" || cfg.TracesEndpoint != ""

	if v := strings.TrimSpace(s.ServiceName); v != "" {
		cfg.ServiceName = v
	}

	if s.Headers != nil {
		for name := range s.Headers {
			if strings.TrimSpace(name) == "" {
				return Config{}, fmt.Errorf("tracing header name must not be empty")
			}
		}
		cfg.Headers = s.Headers
	}

	if s.SampleRatio != nil {
		if *s.SampleRatio < 0 || *s.SampleRatio > 1 {
			return Config{}, fmt.Errorf("tracing sample ratio %v is outside 0..1", *s.SampleRatio)
		}
		cfg.SampleRatio = *s.SampleRatio
	}

	return cfg, nil
}

// resolveTracesEndpoint appends the /v1/traces signal path to a base OTLP URL.
// A trailing slash on the base must not produce a double slash, and any query
// string or fragment is preserved because only the path is joined.
func resolveTracesEndpoint(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return strings.TrimSuffix(strings.TrimSpace(base), "/") + "/v1/traces"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/traces"
	return u.String()
}

// Start returns a tracer and a shutdown function. When cfg.Enabled is false it
// returns a no-op tracer and a shutdown that does nothing and returns nil, and
// it must not open any network connection. Shutdown must be safe to call more
// than once and must respect its context deadline.
func Start(ctx context.Context, cfg Config) (trace.Tracer, func(context.Context) error, error) {
	if !cfg.Enabled {
		// A no-op provider keeps instrumented code paths identical whether or
		// not tracing is configured, so callers never branch on cfg.Enabled.
		tracer := noop.NewTracerProvider().Tracer(DefaultServiceName)
		shutdown := func(ctx context.Context) error {
			// Honour cancellation so a caller waiting on shutdown with a
			// deadline does not hang even though there is nothing to flush.
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		}
		return tracer, shutdown, nil
	}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, nil, fmt.Errorf("create tracer for service %q: sample ratio %v is outside 0..1", serviceName, cfg.SampleRatio)
	}
	if strings.TrimSpace(cfg.Endpoint) == "" && strings.TrimSpace(cfg.TracesEndpoint) == "" {
		return nil, nil, fmt.Errorf("create tracer for service %q: tracing is enabled without an endpoint", serviceName)
	}
	endpoint := strings.TrimSpace(cfg.TracesEndpoint)
	if endpoint == "" {
		// Configs built without TracesEndpoint still resolve from the base.
		endpoint = resolveTracesEndpoint(strings.TrimSpace(cfg.Endpoint))
	}
	// WithEndpointURL silently keeps its default on an invalid URL, so the
	// endpoint is validated here to surface a typo instead of exporting to
	// localhost unexpectedly.
	if err := checkEndpointURL(endpoint); err != nil {
		return nil, nil, fmt.Errorf("create tracer for service %q: %w", serviceName, err)
	}

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
		otlptracehttp.WithHeaders(cfg.Headers),
		otlptracehttp.WithTimeout(exporterTimeout),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP exporter for endpoint %q: %w", endpoint, err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create resource for service %q: %w", serviceName, err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		// ParentBased keeps trace continuity for sampled parents while the
		// ratio sampler bounds the volume of new traces.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithBatcher(exporter),
	)
	// Publishing the provider globally lets libraries that only know the
	// global otel handle join aidev's traces.
	otel.SetTracerProvider(provider)

	var mu sync.Mutex
	var stopped bool
	var firstErr error
	shutdown := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return firstErr
		}
		stopped = true
		firstErr = provider.Shutdown(ctx)
		return firstErr
	}

	return provider.Tracer(serviceName), shutdown, nil
}

// checkEndpointURL requires a scheme and host because the OTLP exporter would
// otherwise fall back to localhost and send spans somewhere unintended.
func checkEndpointURL(endpoint string) error {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return fmt.Errorf("invalid OTLP endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid OTLP endpoint %q: want an http or https URL", endpoint)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid OTLP endpoint %q: missing host", endpoint)
	}
	return nil
}
