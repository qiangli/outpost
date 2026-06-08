package telemetry

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/propagation"
)

// Tracing returns a gin middleware that:
//   - Parses incoming `traceparent` + `tracestate` from the matrix-tunnel
//     envelope (cloudbox stamps these when its own tracing middleware
//     ran) and installs them as the parent context on the gin Request
//     so every downstream span / log / outbound app-proxy hop inherits
//     the trace_id.
//   - Starts a server span named after the matched gin route with
//     standard semconv HTTP attributes.
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
	return otelgin.Middleware(serviceName,
		otelgin.WithPropagators(propagation.TraceContext{}),
	)
}
