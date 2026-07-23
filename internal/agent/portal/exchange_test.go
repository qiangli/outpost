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

// TestExchangeSecureDefaultsForNewHost — a NEWLY paired host starts with
// the host-access built-ins OFF (opt-in). These defaults are written only
// on this fresh-config path; existing hosts (reattach) are untouched.
func TestExchangeSecureDefaultsForNewHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"agent_name":"n","server_addr":"a","server_port":443,"protocol":"wss","token":"t","remote_port":7100}`)
	}))
	defer server.Close()

	fc, err := Exchange(context.Background(), ExchangeRequest{ServerURL: server.URL, Code: "c", Name: "n"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if fc.ShellOn() || fc.DesktopOn() || fc.ClipboardOn() || fc.SSHOn() || fc.FilesOn() || fc.PodmanOn() {
		t.Errorf("new host should be opt-in (all off): shell=%v desktop=%v clip=%v ssh=%v files=%v podman=%v",
			fc.ShellOn(), fc.DesktopOn(), fc.ClipboardOn(), fc.SSHOn(), fc.FilesOn(), fc.PodmanOn())
	}
	// Cluster is opt-in too; ollama/otel are deliberately left default-on.
	if fc.ClusterOn() {
		t.Error("new host: cluster should be off (opt-in)")
	}
	if !fc.OllamaOn() || !fc.OtelOn() {
		t.Error("ollama/otel should remain default-on (not part of the host-access opt-in set)")
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

// TestExchangeRetriesOn429 is the regression guard for the outpost half of
// hub admission control: a 429 (registration throttled during a reconnect
// storm) is backpressure, not a caller bug — Exchange must honor Retry-After
// and retry, not treat it as a terminal 4xx. Before the fix, 429 fell into
// the terminal-4xx bucket and pairing failed on the first throttle.
func TestExchangeRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0") // keep the test fast
			http.Error(w, "registration throttled", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"agent_name":"ok","server_addr":"e","server_port":1,"protocol":"wss","token":"t","remote_port":1}`)
	}))
	defer server.Close()

	start := time.Now()
	fc, err := Exchange(context.Background(), ExchangeRequest{ServerURL: server.URL, Code: "c", Name: "n"})
	if err != nil {
		t.Fatalf("Exchange should retry through 429 backpressure: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts (429 is retryable), got %d", calls.Load())
	}
	if fc.AgentName != "ok" {
		t.Errorf("fc.AgentName = %q, want ok", fc.AgentName)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("test took %s; Retry-After:0 should keep it fast", elapsed)
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{http.StatusTooManyRequests, true}, {500, true}, {502, true}, {503, true},
		{400, false}, {401, false}, {404, false}, {410, false}, {200, false},
	}
	for _, c := range cases {
		if got := retryableStatus(c.code); got != c.want {
			t.Errorf("retryableStatus(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestRetryDelayHonorsRetryAfterWithJitter(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "5")
	for i := 0; i < 200; i++ {
		d := retryDelay(1, resp)
		// At least the server's 5s floor, at most 5s + the 2.5s jitter window.
		if d < 5*time.Second || d >= 5*time.Second+2500*time.Millisecond {
			t.Fatalf("retryDelay honoring Retry-After=5 out of bounds: %v", d)
		}
	}
	// nil resp (network error) → jittered exponential base; attempt 2 = 2s
	// floor + up to a 1s window.
	for i := 0; i < 200; i++ {
		d := retryDelay(2, nil)
		if d < 2*time.Second || d >= 3*time.Second {
			t.Fatalf("retryDelay(2,nil) out of bounds: %v", d)
		}
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

// TestExchangeRetryPreservesFirstError reproduces the host-f pairing
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
