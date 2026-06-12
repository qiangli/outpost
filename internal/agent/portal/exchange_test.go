package portal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestExchangeRoundTrip covers the happy path: the portal returns a
// well-formed exchange response and Exchange decodes it into a FileConfig.
func TestExchangeRoundTrip(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "agent_name": "laptop-alice",
		  "server_addr": "edge.example.com",
		  "server_port": 443,
		  "protocol": "wss",
		  "token": "secret-token",
		  "remote_port": 7100
		}`)
	}))
	defer server.Close()

	fc, err := Exchange(context.Background(), ExchangeRequest{
		ServerURL: server.URL,
		Code:      "abc123",
		Name:      "laptop",
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if gotPath != "/api/register/exchange" {
		t.Errorf("portal path = %q, want /api/register/exchange", gotPath)
	}
	if gotBody["code"] != "abc123" || gotBody["name"] != "laptop" {
		t.Errorf("payload = %+v, missing code/name", gotBody)
	}
	if _, ok := gotBody["has_auth_url"].(bool); !ok {
		t.Errorf("has_auth_url not present or wrong type: %+v", gotBody)
	}
	if fc.AgentName != "laptop-alice" || fc.Token != "secret-token" || fc.RemotePort != 7100 || fc.Protocol != "wss" {
		t.Errorf("decoded fc = %+v, mismatch", fc)
	}
}

// TestExchangeNon200 surfaces the portal's error body to the caller so
// the admin UI can render something useful ("expired code" etc.).
func TestExchangeNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "code expired", http.StatusGone)
	}))
	defer server.Close()

	_, err := Exchange(context.Background(), ExchangeRequest{
		ServerURL: server.URL,
		Code:      "stale",
		Name:      "x",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "code expired") {
		t.Errorf("error = %v, want it to surface the portal's body", err)
	}
}

// TestExchangeRetriesOn5xx covers the cloudbox-split rollout scenario:
// portal replicas return 503 briefly while the cluster service is
// rolling. The first two attempts get 503 + Retry-After, the third
// succeeds. The CLI should not surface the transient failure to the
// user.
func TestExchangeRetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			// Retry-After: 0 keeps the test fast — production uses
			// 1s/2s/4s backoff per exchangeMaxAttempts.
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rolling restart", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"agent_name":"ok","server_addr":"e","server_port":1,"protocol":"wss","token":"t","remote_port":1}`)
	}))
	defer server.Close()

	start := time.Now()
	fc, err := Exchange(context.Background(), ExchangeRequest{
		ServerURL: server.URL,
		Code:      "c",
		Name:      "n",
	})
	if err != nil {
		t.Fatalf("Exchange after retries: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", calls.Load())
	}
	if fc.AgentName != "ok" {
		t.Errorf("fc.AgentName = %q, want ok", fc.AgentName)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("test took %s; Retry-After:0 should have made it fast", elapsed)
	}
}

// TestExchangeDoesNotRetryOn4xx: 4xx errors are caller bugs (bad code,
// missing fields). Retrying would just waste cycles and produce
// duplicate-looking server logs. Fail fast.
func TestExchangeDoesNotRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad code", http.StatusBadRequest)
	}))
	defer server.Close()

	_, err := Exchange(context.Background(), ExchangeRequest{
		ServerURL: server.URL,
		Code:      "bad",
		Name:      "n",
	})
	if err == nil {
		t.Fatal("expected error on 4xx")
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 attempt on 4xx, got %d (regression: should fail fast)", calls.Load())
	}
}

// TestExchangeRequiresTitleWithAuthURL — when delegating /auth to a
// custom endpoint, there is no OS identity to derive a subtitle from, so
// the title becomes mandatory. This is enforced client-side before any
// HTTP call so the portal doesn't get a malformed exchange.
func TestExchangeRequiresTitleWithAuthURL(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()

	_, err := Exchange(context.Background(), ExchangeRequest{
		ServerURL: server.URL,
		Code:      "abc",
		Name:      "x",
		AuthURL:   "https://example.com/auth",
		// Title intentionally omitted.
	})
	if err == nil {
		t.Fatal("expected error when title missing with auth_url, got nil")
	}
	if called {
		t.Error("portal was contacted; client-side validation should short-circuit")
	}
}

// TestExchangeRetryPreservesFirstError reproduces the 2ivy pairing
// incident: the portal redeems the one-time code, then 500s on a host-
// name conflict; the retry hits "code already used" (401). Before the
// firstErr plumbing, the surfaced error was ONLY the 401 — the root
// cause was invisible. Both must be present in the final error.
func TestExchangeRetryPreservesFirstError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"UNIQUE constraint failed: hosts.name"}`, http.StatusInternalServerError)
			return
		}
		http.Error(w, `{"error":"code already used"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := Exchange(context.Background(), ExchangeRequest{
		ServerURL: server.URL,
		Code:      "c",
		Name:      "dup",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls.Load())
	}
	msg := err.Error()
	if !strings.Contains(msg, "code already used") {
		t.Errorf("error should carry the final attempt's failure, got: %s", msg)
	}
	if !strings.Contains(msg, "UNIQUE constraint failed") {
		t.Errorf("error should preserve the first attempt's root cause, got: %s", msg)
	}
}
