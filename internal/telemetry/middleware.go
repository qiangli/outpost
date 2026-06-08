package telemetry

import (
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Tracing returns a gin middleware that:
//   - Parses incoming `traceparent` + `tracestate` from the matrix-tunnel
//     envelope (cloudbox stamps these when its own tracing middleware
//     ran) and installs them as the parent context on the gin Request
//     so every downstream span / log / outbound app-proxy hop inherits
//     the trace_id.
//   - Starts a server span named after the matched gin route with
//     standard semconv HTTP attributes.
//   - Emits one slog line per /app/<name>/* request with the parsed
//     trace_id + span_id so the outpost journal can be grep'd for a
//     given trace_id without a running OTLP collector — load-bearing
//     for end-to-end validation when neither cloudbox nor outpost
//     has an exporter wired up.
//
// Mount this generically on the main matrix-tunnel ingress engine so
// every /app/<name>/* proxy hop, every built-in route (/shell, /ssh,
// /apps, etc.), and every health check is observable.
//
// No-op when the global TracerProvider hasn't been swapped from the
// default (i.e. telemetry.Init ran in no-op mode) — otelgin reads
// from the global TracerProvider, so the noop provider yields noop
// spans with near-zero overhead.
func Tracing(serviceName string) gin.HandlerFunc {
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	otelMW := otelgin.Middleware(serviceName,
		otelgin.WithPropagators(propagation.TraceContext{}),
	)
	return func(c *gin.Context) {
		otelMW(c)
		// Log only the app-proxy hot path. Health checks, /apps polls,
		// and SSE/WS frames would drown the signal otherwise.
		if !strings.HasPrefix(c.FullPath(), "/app/") {
			return
		}
		sc := trace.SpanContextFromContext(c.Request.Context())
		if sc.IsValid() {
			slog.Info("telemetry: incoming traced request",
				"trace_id", sc.TraceID().String(),
				"span_id", sc.SpanID().String(),
				"service", serviceName,
				"path", c.FullPath(),
			)
		}
	}
}
