package telemetry

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

var (
	localMu sync.Mutex
	localTP *sdktrace.TracerProvider
)

// CollectorAnswers reports whether anything speaks HTTP at endpoint. Any
// response counts: an OTLP proxy answers a bare GET with 404/405, and only a
// transport error means nothing is listening.
func CollectorAnswers(ctx context.Context, endpoint string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// LocalExporter returns an OTLP/HTTP span exporter for endpoint when it
// answers, else nil (collector-free).
func LocalExporter(ctx context.Context, endpoint string) sdktrace.SpanExporter {
	if !CollectorAnswers(ctx, endpoint) {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(u.Host)}
	if u.Scheme != "https" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil
	}
	return exp
}

// EnsureLocalCollector exports spans to the local bashy otel stack when it
// answers at endpoint. Safe to call repeatedly (daemon start, capability
// Health): the first call that finds a collector installs the tracer provider;
// later calls are no-ops. An explicit OTEL_EXPORTER_OTLP_ENDPOINT wins, and
// Init has already honoured it. Reports whether export is active.
func EnsureLocalCollector(ctx context.Context, endpoint string) bool {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		return false
	}
	localMu.Lock()
	defer localMu.Unlock()
	if localTP != nil {
		return true
	}
	exp := LocalExporter(ctx, endpoint)
	if exp == nil {
		return false
	}
	svc := os.Getenv("OTEL_SERVICE_NAME")
	if svc == "" {
		svc = DefaultServiceName
	}
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithAttributes(semconv.ServiceName(svc)))
	if err != nil {
		return false
	}
	localTP = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(localTP)
	return true
}
