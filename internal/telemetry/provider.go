// Package telemetry bootstraps the OTEL SDK for outpost and exposes
// the small set of helpers wire callers need (gin tracing middleware,
// reverse-proxy traceparent preservation, slog ↔ OTEL log bridge).
//
// Shape is intentionally mirrored in cloudbox/hub/internal/telemetry/
// so a reader of either codebase finds the same four files
// (provider.go / middleware.go / proxy.go / slog_bridge.go) with the
// same function signatures. Cloudbox is proprietary and outpost is
// OSS, so a shared Go module would force one repo's choices on the
// other — convention keeps them aligned without coupling.
//
// Configuration is pure-env-var, honoring the standard OTEL spec:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT     gRPC collector, e.g. 127.0.0.1:4317
//	OTEL_SERVICE_NAME               defaults to "outpost"
//	OTEL_RESOURCE_ATTRIBUTES        e.g. service.namespace=dhnt,host.name=<host>
//	OTEL_TRACES_SAMPLER             parentbased_traceidratio (default)
//	OTEL_TRACES_SAMPLER_ARG         e.g. "1.0" or "0.1"
//
// When OTEL_EXPORTER_OTLP_ENDPOINT is unset, the provider runs in
// "no-op" mode: the global TextMapPropagator is still installed so
// inbound `traceparent` headers from cloudbox are parsed and outbound
// app-proxy hops preserve them — the trace fabric still works
// end-to-end, just without persistence on this hop. This keeps the
// runtime footprint near-zero for home hosts that haven't enrolled
// in a centralized collector.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// DefaultServiceName is the service.name used when OTEL_SERVICE_NAME
// is unset.
const DefaultServiceName = "outpost"

// ChildEnv returns the OTEL environment additions to hand a supervised child
// process (loom, act_runner, …) so it self-exports into the SAME telemetry
// plane as this outpost — under its OWN service.name, not "outpost". The child
// already inherits os.Environ() (which carries OTEL_EXPORTER_OTLP_ENDPOINT +
// OTEL_RESOURCE_ATTRIBUTES when configured on the host); this only OVERRIDES
// service.name so the deploy path (loom Actions, the act_runner job) shows up as
// a distinct service an agent can filter on when supervising a deployment via
// the existing observability backend (query_traces / query_logs / search_logs).
// Returns nil when no OTLP endpoint is configured, keeping the no-op posture.
// Appended AFTER os.Environ() so it wins (exec takes the last value of a dup key).
func ChildEnv(serviceName string) []string {
	if serviceName == "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return nil
	}
	return []string{"OTEL_SERVICE_NAME=" + serviceName}
}

// Provider holds the OTEL SDK lifetime for this process.
type Provider struct {
	TracerProvider *sdktrace.TracerProvider // nil in no-op mode
	LoggerProvider *sdklog.LoggerProvider   // nil in no-op mode
	shutdown       []func(context.Context) error
}

// Shutdown flushes pending spans/logs and tears down exporters. Safe
// to call multiple times; subsequent calls are no-ops.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	for _, fn := range p.shutdown {
		if err := fn(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	p.shutdown = nil
	return errors.Join(errs...)
}

var (
	initOnce sync.Once
	initErr  error
	initProv *Provider
)

// Init bootstraps the global OTEL providers for outpost. Safe to call
// multiple times — only the first call boots; subsequent calls
// return the same Provider so any in-process supervisor restart
// doesn't double-install exporters.
//
// Even when OTEL_EXPORTER_OTLP_ENDPOINT is unset, Init installs the
// global TextMapPropagator so the cloudbox→outpost→app chain still
// preserves W3C trace context across the proxy hop. Cooperative apps
// that implement the three-rule observability contract get a
// well-formed parent span even when outpost itself isn't exporting.
func Init(ctx context.Context) (*Provider, error) {
	initOnce.Do(func() {
		initProv, initErr = build(ctx)
	})
	return initProv, initErr
}

func build(ctx context.Context) (*Provider, error) {
	// The W3C propagator is always installed — that's the contract.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		// No-op mode: global propagator stays installed so the
		// inbound traceparent from cloudbox flows through onto the
		// app-proxy outbound, but we don't pay for a TracerProvider
		// or exporter. Important on home hosts where the operator
		// hasn't stood up a collector.
		return &Provider{}, nil
	}

	svcName := os.Getenv("OTEL_SERVICE_NAME")
	if svcName == "" {
		svcName = DefaultServiceName
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(), // honors OTEL_RESOURCE_ATTRIBUTES
		resource.WithAttributes(semconv.ServiceName(svcName)),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	p := &Provider{}

	// --- Traces ---
	traceCtx, traceCancel := context.WithTimeout(ctx, 5*time.Second)
	traceExp, err := otlptracegrpc.New(traceCtx,
		otlptracegrpc.WithEndpoint(stripScheme(endpoint)),
		otlptracegrpc.WithInsecure(),
	)
	traceCancel()
	if err != nil {
		return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)
	p.TracerProvider = tp
	p.shutdown = append(p.shutdown, tp.Shutdown)
	otel.SetTracerProvider(tp)

	// --- Logs (best-effort) ---
	logCtx, logCancel := context.WithTimeout(ctx, 5*time.Second)
	logExp, err := otlploggrpc.New(logCtx,
		otlploggrpc.WithEndpoint(stripScheme(endpoint)),
		otlploggrpc.WithInsecure(),
	)
	logCancel()
	if err != nil {
		return p, nil // logs are best-effort — tracing already up
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)
	p.LoggerProvider = lp
	p.shutdown = append(p.shutdown, lp.Shutdown)
	otellog.SetLoggerProvider(lp)

	return p, nil
}

// stripScheme normalizes endpoints — operators sometimes set the
// env var as "http://host:4317" because they copied a URL; the gRPC
// exporter expects "host:4317".
func stripScheme(s string) string {
	for _, p := range []string{"http://", "https://"} {
		if len(s) > len(p) && s[:len(p)] == p {
			return s[len(p):]
		}
	}
	return s
}
