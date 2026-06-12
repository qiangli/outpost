package sandbox

import (
	"encoding/json"
	"net/http"
)

// Service glues the security Filter and the in-flight Counter to the two
// outpost-side surfaces the sandbox mount feeds: the proxy-wrap middleware
// (filter + live load tracking) and the /_pool/capacity intercept
// (cloudbox-side scheduling queries). One Service per sandbox mount.
//
// It mirrors ollama.Service so the main.go boot wiring is symmetric:
//
//	apps.SetCapabilities("sandbox", &agent.AppCapabilities{Type: sandbox.CapabilityType})
//	svc := sandbox.NewService(policy)
//	apps.SetProxyWrap("sandbox", svc.WrapProxy)
//	apps.AddIntercept("sandbox", "/_pool/capacity", svc.CapacityHandler())
type Service struct {
	filter    *Filter
	counter   *Counter
	isolation string
}

// NewService builds a Service from policy. The OCI isolation tier defaults
// to "runc" (shared kernel) — the Phase-A posture; a Phase-B gVisor/Kata
// build sets it via SetIsolation so cloudbox can route untrusted work to
// suitably-isolated hosts.
func NewService(policy Policy) *Service {
	return &Service{
		filter:    NewFilter(policy),
		counter:   NewCounter(policy.MaxContainers),
		isolation: "runc",
	}
}

// SetIsolation records the OCI runtime tier this host enforces ("runc",
// "gvisor", "kata"). Surfaced in CapacityReport.Isolation.
func (s *Service) SetIsolation(tier string) {
	if tier != "" {
		s.isolation = tier
	}
}

// WrapProxy is the middleware factory passed to AppRegistry.SetProxyWrap.
// The filter runs outermost so a denied create never increments the
// in-flight counter, then the counter wraps the reverse proxy.
func (s *Service) WrapProxy(next http.Handler) http.Handler {
	return s.filter.Wrap(s.counter.Wrap(next))
}

// Snapshot composes the capacity report from the counter plus the
// service-level isolation tier. Pool fields stay zero until the Phase-A
// warm pool lands.
func (s *Service) Snapshot() CapacityReport {
	rep := s.counter.Snapshot()
	rep.Isolation = s.isolation
	return rep
}

// CapacityHandler returns the http.Handler bound at
// /app/sandbox/_pool/capacity. It answers quickly (an atomic load + a
// struct encode) because cloudbox's scheduler may probe it on every
// routed request.
func (s *Service) CapacityHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Snapshot())
	})
}
