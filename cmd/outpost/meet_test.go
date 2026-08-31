package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// findBashyService returns the effective service entry for name, or nil.
func findBashyService(fc *conf.FileConfig, name string) *conf.BashyService {
	for _, s := range effectiveBashyServices(fc) {
		if s.Name == name {
			s := s
			return &s
		}
	}
	return nil
}

func provisionBashyServiceSecrets(t *testing.T, fc *conf.FileConfig) {
	t.Helper()
	if _, err := conf.EnsureBashyServiceSSOSecrets("", fc, effectiveBashyServices(fc)); err != nil {
		t.Fatalf("EnsureBashyServiceSSOSecrets: %v", err)
	}
}

// THE TRAP: the meet catalog entry MUST carry Command=["meet","service"] so the
// supervisor invokes `bashy meet service start` (the web chat daemon) — NOT
// `bashy meet start`, which already exists and starts a deliberation session.
// This test pins the Command base so nobody "simplifies" it back to ["meet"].
func TestDefaultBashyServicesMeetCommandPinned(t *testing.T) {
	var svc *conf.BashyService
	for _, s := range conf.DefaultBashyServices() {
		if s.Name == "meet" {
			s := s
			svc = &s
		}
	}
	if svc == nil {
		t.Fatal("expected a default 'meet' service entry")
	}
	if len(svc.Command) != 2 || svc.Command[0] != "meet" || svc.Command[1] != "service" {
		t.Fatalf("meet default Command = %v, want [meet service] (else 'bashy meet start' starts a meeting, not the daemon)", svc.Command)
	}
	if !svc.TrustCloudIdentity {
		t.Fatal("meet default must trust cloud identity so coopauth receives signed Remote-* headers")
	}
}

// An operator can enable meet with just {name:"meet",enabled:true}; the Command
// base is inherited from the default so `bashy meet service {start,...}` runs.
func TestEffectiveBashyServicesMeetInheritsCommand(t *testing.T) {
	fc := &conf.FileConfig{
		BashyServices: []conf.BashyService{
			{Name: "meet", Enabled: true},
		},
	}
	got := findBashyService(fc, "meet")
	if got == nil {
		t.Fatal("meet service missing from effective set")
	}
	if !got.Enabled {
		t.Fatal("operator-enabled meet should be enabled")
	}
	if len(got.Command) != 2 || got.Command[0] != "meet" || got.Command[1] != "service" {
		t.Fatalf("Command not inherited: %v, want [meet service]", got.Command)
	}
	if !got.TrustCloudIdentity {
		t.Fatal("meet trusted-identity requirement was not inherited")
	}
}

// meet_enabled=true yields a meet service registered as a cloudbox app at
// http://127.0.0.1:8637 with RequireLogin=true and NO mesh service.
func TestEffectiveBashyServicesMeetOn(t *testing.T) {
	on := true
	fc := &conf.FileConfig{MeetEnabled: &on}
	provisionBashyServiceSecrets(t, fc)
	got := findBashyService(fc, "meet")
	if got == nil {
		t.Fatal("meet service missing from effective set when meet_enabled=true")
	}
	if !got.Enabled {
		t.Fatal("meet should be enabled when meet_enabled=true")
	}
	if got.AppName != "meet" {
		t.Fatalf("AppName = %q, want meet", got.AppName)
	}
	if got.AppPort != 8637 {
		t.Fatalf("AppPort = %d, want 8637", got.AppPort)
	}
	if !got.RequireLogin {
		t.Fatal("meet must RequireLogin")
	}
	if !got.TrustCloudIdentity {
		t.Fatal("meet must TrustCloudIdentity")
	}
	if len(got.SSOSecret) != 64 {
		t.Fatalf("meet SSOSecret length = %d, want 64 hex characters", len(got.SSOSecret))
	}
	if got.MeshService != "" {
		t.Fatalf("meet must NOT expose a mesh service, got %q", got.MeshService)
	}

	// registerBashyServiceApps must publish it under name "meet".
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	u := reg.LookupTarget("meet")
	if u == nil {
		t.Fatal("meet app not registered")
	}
	if u.String() != "http://127.0.0.1:8637" {
		t.Fatalf("meet target = %q, want http://127.0.0.1:8637", u.String())
	}
	var requireLogin bool
	for _, e := range reg.Entries() {
		if e.Name == "meet" {
			requireLogin = e.RequireLogin
		}
	}
	if !requireLogin {
		t.Fatal("meet app must be registered with RequireLogin=true")
	}
	if secret := reg.SSOSecret("meet"); secret != got.SSOSecret {
		t.Fatalf("registered meet SSOSecret = %q, want provisioned service secret", secret)
	}
}

// Default OFF: a config with no meet_enabled key registers NO meet app and
// yields no enabled meet service.
func TestEffectiveBashyServicesMeetDefaultOff(t *testing.T) {
	fc := &conf.FileConfig{} // no meet_enabled
	if got := findBashyService(fc, "meet"); got != nil && got.Enabled {
		t.Fatalf("meet must default OFF, got enabled entry: %+v", got)
	}
	provisionBashyServiceSecrets(t, fc)
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	if u := reg.LookupTarget("meet"); u != nil {
		t.Fatalf("no meet app should be registered by default, got %q", u.String())
	}
}

// meet_port override flows through to the registered app target.
func TestEffectiveBashyServicesMeetPortOverride(t *testing.T) {
	on := true
	fc := &conf.FileConfig{MeetEnabled: &on, MeetPort: 9999}
	provisionBashyServiceSecrets(t, fc)
	got := findBashyService(fc, "meet")
	if got == nil {
		t.Fatal("meet service missing")
	}
	if got.AppPort != 9999 {
		t.Fatalf("AppPort = %d, want 9999 (override)", got.AppPort)
	}
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	u := reg.LookupTarget("meet")
	if u == nil || u.String() != "http://127.0.0.1:9999" {
		t.Fatalf("meet target = %v, want http://127.0.0.1:9999", u)
	}
}

func TestRegisteredMeetAPIReceivesVerifiableIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("Remote-User")
		role := r.Header.Get("X-Periscope-Role")
		prefix := r.Header.Get("X-Forwarded-Prefix")
		ts := r.Header.Get("X-Outpost-Identity-Ts")
		sig, err := hex.DecodeString(r.Header.Get("X-Outpost-Identity-Sig"))
		if err != nil {
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
		mac := hmac.New(sha256.New, []byte("meet-test-secret"))
		_, _ = mac.Write([]byte(user + "\n" + role + "\n" + prefix + "\n" + ts))
		if user == "" || !hmac.Equal(sig, mac.Sum(nil)) {
			http.Error(w, "untrusted identity", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer upstream.Close()

	_, portText, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	fc := &conf.FileConfig{
		MeetEnabled: &on,
		MeetPort:    port,
		BashyServices: []conf.BashyService{{
			Name:               "meet",
			TrustCloudIdentity: true,
			SSOSecret:          "meet-test-secret",
		}},
	}
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Any("/app/:name/*p", func(c *gin.Context) {
		reg.ProxyTo(c, c.Param("name"), c.Param("p"))
	})
	proxy := httptest.NewServer(router)
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodGet, proxy.URL+"/app/meet/api/rooms", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Prefix", "/matrix/h/dragon/app/meet")
	req.Header.Set("X-Periscope-User", "owner@example.com")
	req.Header.Set("X-Periscope-Role", "owner")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "[]" {
		t.Fatalf("GET /app/meet/api/rooms = %d, %q; want 200, []", resp.StatusCode, body)
	}
}

func TestRegisterBashyServiceAppsRejectsTrustedIdentityWithoutSecret(t *testing.T) {
	fc := &conf.FileConfig{
		BashyServices: []conf.BashyService{{
			Name:               "unsigned",
			Enabled:            true,
			AppPort:            8765,
			TrustCloudIdentity: true,
		}},
	}
	err := registerBashyServiceApps(fc, agent.NewAppRegistry())
	if err == nil {
		t.Fatal("trusted bashy service without sso_secret was silently registered")
	}
	if !strings.Contains(err.Error(), "requires a non-empty sso_secret") {
		t.Fatalf("registration failure was not actionable: %v", err)
	}
}

func TestBuildAppRegistryRejectsTrustedIdentityWithoutSecret(t *testing.T) {
	fc := &conf.FileConfig{
		Apps: []conf.AppConfig{{
			Name:               "unsigned",
			Enabled:            true,
			Port:               8765,
			TrustCloudIdentity: true,
		}},
	}
	if _, err := buildAppRegistry(fc, ""); err == nil {
		t.Fatal("trusted app without sso_secret was silently registered")
	} else if !strings.Contains(err.Error(), "requires a non-empty sso_secret") {
		t.Fatalf("registration failure was not actionable: %v", err)
	}
}
