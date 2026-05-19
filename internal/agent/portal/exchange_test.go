package portal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
