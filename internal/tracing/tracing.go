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
	"os"
	"strconv"
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

// DefaultServiceName is used when OTEL_SERVICE_NAME is unset so that spans
// from every aidev process share one service identity in the backend.
const DefaultServiceName = "aidev"

// exporterTimeout bounds each batch export, so a slow or unreachable backend
// cannot block shutdown indefinitely; Shutdown still honours its own context
// deadline on top of this.
const exporterTimeout = 10 * time.Second

// Config is read from the environment by FromEnv.
type Config struct {
	Enabled        bool
	Endpoint       string            // base OTLP HTTP URL from OTEL_EXPORTER_OTLP_ENDPOINT
	TracesEndpoint string            // full URL the traces exporter posts to (<base>/v1/traces, or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT as given)
	Headers        map[string]string // extra OTLP headers, e.g. Authorization
	ServiceName    string            // defaults to "aidev"
	SampleRatio    float64           // 0..1, defaults to 1
}

// FromEnv reads OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_TRACES_ENDPOINT,
// OTEL_EXPORTER_OTLP_HEADERS, OTEL_SERVICE_NAME and OTEL_TRACES_SAMPLER_ARG.
// Enabled is true when either endpoint variable sets a non-empty value.
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is the full signal-specific URL and is used
// as given; otherwise TracesEndpoint appends /v1/traces to the base endpoint, as
// the OpenTelemetry specification defines the base variable without a signal
// path. OTEL_EXPORTER_OTLP_HEADERS is a comma-separated list of key=value pairs.
// Returns an error for a malformed header list or a sample ratio outside 0..1.
func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(key string) string {
		v, _ := lookup(key)
		return v
	}

	cfg := Config{
		ServiceName: DefaultServiceName,
		SampleRatio: 1,
	}

	cfg.Endpoint = strings.TrimSpace(get("OTEL_EXPORTER_OTLP_ENDPOINT"))
	tracesEndpoint := strings.TrimSpace(get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	switch {
	case tracesEndpoint != "":
		// Signal-specific URL takes precedence and is used verbatim.
		cfg.TracesEndpoint = tracesEndpoint
	case cfg.Endpoint != "":
		cfg.TracesEndpoint = resolveTracesEndpoint(cfg.Endpoint)
	}
	cfg.Enabled = cfg.Endpoint != "" || cfg.TracesEndpoint != ""

	if v := strings.TrimSpace(get("OTEL_SERVICE_NAME")); v != "" {
		cfg.ServiceName = v
	}

	rawHeaders := strings.TrimSpace(get("OTEL_EXPORTER_OTLP_HEADERS"))
	if rawHeaders != "" {
		headers, err := parseHeaders(rawHeaders)
		if err != nil {
			return Config{}, fmt.Errorf("parse OTEL_EXPORTER_OTLP_HEADERS: %w", err)
		}
		cfg.Headers = headers
	}

	rawRatio := strings.TrimSpace(get("OTEL_TRACES_SAMPLER_ARG"))
	if rawRatio != "" {
		ratio, err := strconv.ParseFloat(rawRatio, 64)
		if err != nil {
			return Config{}, fmt.Errorf("parse OTEL_TRACES_SAMPLER_ARG %q as a number: %w", rawRatio, err)
		}
		if ratio < 0 || ratio > 1 {
			return Config{}, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG %q is outside 0..1", rawRatio)
		}
		cfg.SampleRatio = ratio
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

// parseHeaders splits a comma-separated key=value list. Entries without a
// separator or with an empty key are rejected rather than silently dropped, so
// a typo in authentication headers fails at startup instead of producing
// unauthenticated exports.
func parseHeaders(raw string) (map[string]string, error) {
	headers := make(map[string]string)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("malformed header entry %q: want key=value", part)
		}
		headers[key] = strings.TrimSpace(value)
	}
	return headers, nil
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
