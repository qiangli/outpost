package sandbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestService_CapacityHandler(t *testing.T) {
	svc := NewService(Policy{MaxContainers: 8})
	rr := httptest.NewRecorder()
	svc.CapacityHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_pool/capacity", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q", ct)
	}
	var rep CapacityReport
	if err := json.NewDecoder(rr.Body).Decode(&rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.MaxContainers != 8 {
		t.Errorf("MaxContainers=%d, want 8", rep.MaxContainers)
	}
	if rep.Isolation != "runc" {
		t.Errorf("Isolation=%q, want runc (default)", rep.Isolation)
	}
}

func TestService_WrapProxy_DeniedCreateNotCounted(t *testing.T) {
	svc := NewService(Policy{})
	upstreamHit := false
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHit = true
		w.WriteHeader(http.StatusCreated)
	})
	h := svc.WrapProxy(upstream)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1.41/containers/create",
		strings.NewReader(`{"Image":"alpine","HostConfig":{"Privileged":true}}`))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rr.Code)
	}
	if upstreamHit {
		t.Error("filter must deny before the proxy/counter runs")
	}
	// A denied create must leave InFlight at zero.
	if got := svc.Snapshot().InFlight; got != 0 {
		t.Errorf("InFlight=%d after denied create, want 0", got)
	}
}

func TestService_WrapProxy_InFlightTracked(t *testing.T) {
	svc := NewService(Policy{})
	probe := make(chan struct{})
	hold := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probe <- struct{}{}
		<-hold
		w.WriteHeader(http.StatusCreated)
	})
	h := svc.WrapProxy(upstream)
	go h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1.41/containers/create", strings.NewReader(`{"Image":"alpine"}`)))
	<-probe
	if got := svc.Snapshot().InFlight; got != 1 {
		t.Errorf("InFlight=%d during create, want 1", got)
	}
	close(hold)
}

func TestService_SetIsolation(t *testing.T) {
	svc := NewService(Policy{})
	svc.SetIsolation("gvisor")
	if got := svc.Snapshot().Isolation; got != "gvisor" {
		t.Errorf("Isolation=%q, want gvisor", got)
	}
}
