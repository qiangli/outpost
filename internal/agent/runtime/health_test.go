package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The load-bearing classification: ANY well-formed HTTP response means the
// apiserver is SERVING — including a 401 from an apiserver that denies
// anonymous requests, because it still had to complete a TLS handshake and
// route the request to say so. Only a transport error means dead. Treating a
// 401 as unhealthy would have the supervisor restart a perfectly good,
// correctly locked-down control plane in a loop.
func TestProbeAPIServer_ClassifiesServingIncluding401(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	h := ProbeAPIServer(context.Background(), srv.URL+serverReadyPath)
	if !h.Serving {
		t.Fatalf("a 401 from a live apiserver must read as serving: %+v", h)
	}
	if h.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", h.Status)
	}
	if h.Err != "" {
		t.Errorf("serving result carried a transport error: %q", h.Err)
	}
}

func TestProbeAPIServer_HealthyEndpointServes(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	h := ProbeAPIServer(context.Background(), srv.URL+serverReadyPath)
	if !h.Serving || h.Status != http.StatusOK {
		t.Fatalf("healthy endpoint misread: %+v", h)
	}
}

// A closed listener is dead: a transport error, no HTTP response, Serving
// false. This is the container-up-but-apiserver-dead signal the supervisor
// acts on.
func TestProbeAPIServer_ClassifiesTransportErrorAsDead(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL + serverReadyPath
	srv.Close() // now nothing is listening

	h := ProbeAPIServer(context.Background(), url)
	if h.Serving {
		t.Fatalf("a closed server must read as not serving: %+v", h)
	}
	if h.Err == "" {
		t.Error("a dead endpoint must record the transport error")
	}
}

// A wildcard bind is an accept address, not a dial address — the probe must hit
// loopback so it reaches the port the container publishes there.
func TestAPIReadyURL_NormalizesWildcardBindToLoopback(t *testing.T) {
	for _, bind := range []string{"", "0.0.0.0", "::", "[::]"} {
		o := ServerOptions{TunnelBindAddr: bind, APIPort: 16443}
		got := o.APIReadyURL()
		if want := "https://127.0.0.1:16443/readyz"; got != want {
			t.Errorf("bind %q: APIReadyURL() = %q, want %q", bind, got, want)
		}
	}
}
