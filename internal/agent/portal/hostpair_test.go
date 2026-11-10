package portal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

const pairOK = `{"host":{"host":"studio","owner_email":"owner@example.com","paired_at":"2026-11-09T18:10:00Z"},"token":"htok","scopes":["host"],"tunnel":false}`

func TestHostPairNotMaster(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"404":          func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"spa fallback": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html></html>")) },
	} {
		_, err := HostPair(context.Background(), HostPairRequest{ServerURL: serve(t, h), Code: "c", Host: "studio"})
		if !errors.Is(err, ErrNoHostPair) {
			t.Errorf("%s: err = %v, want ErrNoHostPair", name, err)
		}
	}
}

func TestHostPairRefusesTunnelTrue(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(strings.Replace(pairOK, `"tunnel":false`, `"tunnel":true`, 1)))
	})
	if _, err := HostPair(context.Background(), HostPairRequest{ServerURL: url, Code: "c", Host: "studio"}); err == nil || errors.Is(err, ErrNoHostPair) {
		t.Fatalf("err = %v, want refusal", err)
	}
}

// 503 is retried; the replay is safe because the contract makes a same
// code+host retry idempotent.
func TestHostPairRetriesTransient(t *testing.T) {
	var n atomic.Int32
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(pairOK))
	})
	id, err := HostPair(context.Background(), HostPairRequest{ServerURL: url, Code: "c", Host: "studio"})
	if err != nil || id.Token != "htok" || id.OwnerEmail != "owner@example.com" || id.PortalURL != url {
		t.Fatalf("id=%+v err=%v", id, err)
	}
}

func TestHostPairEnvelopeError(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"code":"host_exists","error":"host \"studio\" is already paired"}`))
	})
	_, err := HostPair(context.Background(), HostPairRequest{ServerURL: url, Code: "c", Host: "studio"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "host_exists" || ae.Status != 409 {
		t.Fatalf("err = %v, want host_exists envelope", err)
	}
}
