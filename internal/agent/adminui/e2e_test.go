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
	// registry (the path main.go threads through to the matrix tunnel).
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

// TestProvisioningToken_LifecycleE2E exercises the full provisioning-
// token lifecycle through the public API:
//   - POST with trust_cloud_identity=true auto-mints a token and the
//     live registry accepts it.
//   - Editing the app (POST again with TCI on) preserves the token
//     value — operators shouldn't lose the bearer they just configured
//     into the app whenever they tweak some unrelated field.
//   - POST /api/apps/:name/provisioning-token/rotate generates a fresh
//     token, the registry's bearer lookup switches over, and the old
//     token stops authenticating.
//   - Flipping trust_cloud_identity off clears the token.
//   - Rotation on an app with TCI off is rejected as 400 (operator
//     error: nothing to rotate).
func TestProvisioningToken_LifecycleE2E(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	s, err := New(Deps{
		ConfigPath: configPath,
		ListenAddr: "127.0.0.1:0",
		Auth:       hostauth.StubAuth{},
		Apps:       agent.NewAppRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	client := &http.Client{Timeout: 2 * time.Second}
	base := s.URL()

	post := func(t *testing.T, path string, body any) (*http.Response, []byte) {
		t.Helper()
		buf, _ := json.Marshal(body)
		resp, err := client.Post(base+path, "application/json", bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, b
	}
	listApp := func(t *testing.T, name string) conf.AppConfig {
		t.Helper()
		fc, _ := conf.LoadFile(configPath)
		if fc == nil {
			t.Fatal("config not yet written")
		}
		for _, a := range fc.Apps {
			if a.Name == name {
				return a
			}
		}
		t.Fatalf("app %q not in persisted config", name)
		return conf.AppConfig{}
	}

	// Initial create: trust_cloud_identity=true should auto-mint a token.
	resp, body := post(t, "/api/apps", map[string]any{
		"name": "grafana", "scheme": "http", "host": "127.0.0.1", "port": 3000,
		"enabled": true, "require_login": true, "trust_cloud_identity": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status = %d, body = %s", resp.StatusCode, body)
	}
	app := listApp(t, "grafana")
	if app.ProvisioningToken == "" {
		t.Fatal("expected a provisioning token to be auto-minted")
	}
	originalToken := app.ProvisioningToken
	if regTok := s.deps.Apps.ProvisioningToken("grafana"); regTok != originalToken {
		t.Errorf("live registry token = %q, persisted = %q", regTok, originalToken)
	}

	// Edit: bumping the port shouldn't change the token.
	resp, body = post(t, "/api/apps", map[string]any{
		"name": "grafana", "scheme": "http", "host": "127.0.0.1", "port": 3001,
		"enabled": true, "require_login": true, "trust_cloud_identity": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit: status = %d, body = %s", resp.StatusCode, body)
	}
	if got := listApp(t, "grafana").ProvisioningToken; got != originalToken {
		t.Errorf("edit changed token: got %q, want %q (preserved)", got, originalToken)
	}

	// Rotate: explicit endpoint replaces the token.
	resp, body = post(t, "/api/apps/grafana/provisioning-token/rotate", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: status = %d, body = %s", resp.StatusCode, body)
	}
	var rotateResp struct {
		ProvisioningToken string `json:"provisioning_token"`
	}
	if err := json.Unmarshal(body, &rotateResp); err != nil {
		t.Fatalf("rotate body not json: %v (%q)", err, body)
	}
	if rotateResp.ProvisioningToken == "" {
		t.Fatal("rotate returned no token")
	}
	if rotateResp.ProvisioningToken == originalToken {
		t.Errorf("rotate produced the same token: %q", rotateResp.ProvisioningToken)
	}
	if got := listApp(t, "grafana").ProvisioningToken; got != rotateResp.ProvisioningToken {
		t.Errorf("persisted token = %q, want %q (rotated)", got, rotateResp.ProvisioningToken)
	}
	// Old token must no longer resolve in the live registry.
	if _, ok := s.deps.Apps.LookupByProvisioningToken(originalToken); ok {
		t.Errorf("old token still resolves after rotation")
	}
	if name, ok := s.deps.Apps.LookupByProvisioningToken(rotateResp.ProvisioningToken); !ok || name != "grafana" {
		t.Errorf("new token lookup = (%q, %v), want (grafana, true)", name, ok)
	}

	// Flip TCI off: token gets cleared.
	resp, body = post(t, "/api/apps", map[string]any{
		"name": "grafana", "scheme": "http", "host": "127.0.0.1", "port": 3001,
		"enabled": true, "require_login": true, "trust_cloud_identity": false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable TCI: status = %d, body = %s", resp.StatusCode, body)
	}
	if got := listApp(t, "grafana").ProvisioningToken; got != "" {
		t.Errorf("disabling TCI should clear token, got %q", got)
	}
	if _, ok := s.deps.Apps.LookupByProvisioningToken(rotateResp.ProvisioningToken); ok {
		t.Errorf("registry still resolves token after TCI off")
	}

	// Rotation on TCI-off app must 400.
	resp, body = post(t, "/api/apps/grafana/provisioning-token/rotate", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rotate on TCI-off app: status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}

	// Rotation on unknown app must 404.
	resp, body = post(t, "/api/apps/does-not-exist/provisioning-token/rotate", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("rotate on unknown app: status = %d, want 404 (body=%s)", resp.StatusCode, body)
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
