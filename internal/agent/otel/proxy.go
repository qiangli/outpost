package otel

import "net/http"

// Surface names for the ycode observability stack. Each becomes a
// built-in app: /app/<name> on the matrix tunnel proxies to
// <ycode-proxy>/<sub-path>/. Cloudbox discovers these by scanning the
// /apps response for capability Type="otel".
const (
	SurfacePrometheus = "otel-prometheus" // PromQL + remote_read + /federate
	SurfaceAlerts     = "otel-alerts"     // Alertmanager API v2 + UI
	SurfaceLogs       = "otel-logs"       // VictoriaLogs select/insert
	SurfaceTraces     = "otel-traces"     // Jaeger query
	SurfaceDashboard  = "otel-dashboard"  // Perses dashboards
)

// Surfaces returns the ordered list of ycode observability surfaces
// outpost exposes when OtelOn() is true. Order is significant only for
// log output (prometheus first because it's the most-queried) — every
// caller iterates the slice and treats each independently.
func Surfaces() []string {
	return []string{
		SurfacePrometheus,
		SurfaceLogs,
		SurfaceAlerts,
		SurfaceTraces,
		SurfaceDashboard,
	}
}

// SubPath returns the ycode-proxy sub-path each surface lives at.
// Empty for unknown surfaces.
func SubPath(surface string) string {
	switch surface {
	case SurfacePrometheus:
		return "/prometheus/"
	case SurfaceAlerts:
		return "/alerts/"
	case SurfaceLogs:
		return "/logs/"
	case SurfaceTraces:
		return "/traces/"
	case SurfaceDashboard:
		return "/dashboard/"
	}
	return ""
}

// BearerInjector returns proxy-wrap middleware that stamps
// Authorization: Bearer <token> on every forwarded request. The ycode
// proxy enforces bearer auth on every sub-path including /prometheus/
// — without this header upstream returns 401 and the federated query
// path looks broken from cloudbox's side.
//
// We set unconditionally (overwriting any inbound Authorization)
// because cloudbox's matrix-tunnel hop carries its own bearer in the
// reverse direction; the inbound value is irrelevant to ycode and
// would make this surface inadvertently accept cloudbox-tier tokens.
// Empty token returns a pass-through wrapper — handy for tests and
// for ycode builds with auth disabled.
func BearerInjector(token string) func(http.Handler) http.Handler {
	if token == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	hdr := "Bearer " + token
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("Authorization", hdr)
			next.ServeHTTP(w, r)
		})
	}
}
