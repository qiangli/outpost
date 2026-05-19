package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// TestServerEndToEnd binds a real loopback listener and drives it with
// an http.Client. Covers the wiring main.go relies on: New → Addr →
// Serve(ctx) → ctx cancellation triggers a clean shutdown.
func TestServerEndToEnd(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	s, err := New(Deps{
		ConfigPath: configPath,
		ListenAddr: "127.0.0.1:0", // OS-assigned port; safe under parallel test runs
		Auth:       hostauth.StubAuth{},
		Apps:       agent.NewAppRegistry(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Addr() == "" || s.URL() == "" {
		t.Fatalf("Addr/URL empty after New")
	}

	ctx, cancel := context.WithCancel(t.Context())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx) }()

	client := &http.Client{Timeout: 2 * time.Second}
	base := s.URL()

	// /healthz comes up immediately and needs no auth.
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}

	// First-run gate is open — /api/status without a cookie returns 200.
	resp, err = client.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	var status map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	if status["configured"] != false {
		t.Errorf("expected configured=false on first run, got %v", status["configured"])
	}

	// Add a custom app via the API and verify it lands in the live
	// registry (the path main.go threads through to the FRP tunnel).
	body, _ := json.Marshal(map[string]any{
		"name": "jupyter", "scheme": "http", "host": "127.0.0.1", "port": 8888, "enabled": true,
	})
	resp, err = client.Post(base+"/api/apps", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/apps: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/apps = %d body=%s", resp.StatusCode, respBody)
	}
	if s.deps.Apps.LookupTarget("jupyter") == nil {
		t.Error("jupyter not in live registry after e2e POST")
	}
	// And the persisted config has it.
	fc, _ := conf.LoadFile(configPath)
	if fc == nil || len(fc.Apps) != 1 || fc.Apps[0].Name != "jupyter" {
		t.Errorf("persisted apps after e2e POST = %+v", fc)
	}

	// Cancel — Serve should return cleanly within shutdown timeout.
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("Serve returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}

// TestServerEmbeddedUI fetches "/" and asserts the embedded HTML SPA is
// served. Regression guard: if //go:embed silently drops the ui/
// directory (path typo, build tag, etc.), this test will catch it.
func TestServerEmbeddedUI(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	s, err := New(Deps{
		ConfigPath: configPath,
		ListenAddr: "127.0.0.1:0",
		Auth:       hostauth.StubAuth{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(s.URL() + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("outpost admin")) {
		t.Errorf("served body missing the admin title — embed broken? first 200 bytes: %s", body[:min(200, len(body))])
	}
}
