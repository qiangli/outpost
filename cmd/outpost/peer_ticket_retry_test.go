package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A peer ticket is 60s and SINGLE-USE (the peer replay-protects the jti), and
// that is deliberate: minting one per dial keeps authorization CURRENT, and
// nothing durable travels with a mobile peer. The cost is that a momentary
// cloudbox 5xx denies a direct link that is up and idle, observed live. These
// tests pin the mitigation — survive a blip, never weaken the check.
func TestExchangePeerTicketRetriesTransientCloudbox(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&calls, 1) == 1 {
					w.WriteHeader(code)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ticket":"tkt-abc"}`))
			}))
			defer srv.Close()

			got, err := exchangePeerTicket(context.Background(), srv.URL, "bearer", "cookie", "host-b", "ssh")
			if err != nil {
				t.Fatalf("expected the retry to recover, got %v", err)
			}
			if got != "tkt-abc" {
				t.Fatalf("ticket = %q, want tkt-abc", got)
			}
			if n := atomic.LoadInt32(&calls); n != 2 {
				t.Fatalf("calls = %d, want 2 (one failure, one success)", n)
			}
		})
	}
}

// A 4xx is cloudbox's real answer — the caller is not authorized, or the host
// is unknown. Retrying it would delay an honest failure and hammer the control
// plane for nothing.
func TestExchangePeerTicketDoesNotRetryClientErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()

	if _, err := exchangePeerTicket(context.Background(), srv.URL, "b", "c", "host-b", "ssh"); err == nil {
		t.Fatal("expected an error for 403")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls = %d, want 1 — a 4xx must not be retried", n)
	}
}

// Sustained unavailability still fails, and must not retry forever: the caller
// falls back to the tunnel, so a bounded attempt is the whole contract.
func TestExchangePeerTicketGivesUpAndIsBounded(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := exchangePeerTicket(context.Background(), srv.URL, "b", "c", "host-b", "ssh")
	if err == nil {
		t.Fatal("expected failure when cloudbox stays down")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error should name the status, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls = %d, want 3 (initial + 2 retries)", n)
	}
}

// A cancelled context must abandon the retry loop rather than sleep through it.
func TestExchangePeerTicketHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := exchangePeerTicket(ctx, srv.URL, "b", "c", "host-b", "ssh"); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
}
