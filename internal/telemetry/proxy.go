package telemetry

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// PreserveTraceContext stamps the W3C `traceparent` (+ `tracestate`)
// from the inbound request's context onto the outbound proxy request
// so a cooperative app (or any downstream hop) can attach its spans
// to the same trace.
//
// Call this from inside an httputil.ReverseProxy.Rewrite callback —
// outpost's apps.go uses Rewrite (not Director), so the inbound and
// outbound requests are two distinct *http.Request values via
// pr.In and pr.Out. The function takes them in that order:
//
//	telemetry.PreserveTraceContext(pr.Out, pr.In)
//
// Safe to call unconditionally: when no span is active in the inbound
// context (no traceparent from cloudbox and no local span), the
// propagator injects nothing and pr.Out is left unchanged.
func PreserveTraceContext(out *http.Request, in *http.Request) {
	if out == nil || in == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(
		in.Context(),
		propagation.HeaderCarrier(out.Header),
	)
}
