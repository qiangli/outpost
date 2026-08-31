package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestBashyAppsFreshDefaultOn(t *testing.T) {
	fc := &conf.FileConfig{}
	provisionBashyServiceSecrets(t, fc)
	svc := findBashyService(fc, "apps")
	if svc == nil || !svc.Enabled {
		t.Fatalf("fresh config should enable bashy Apps by default, got %+v", svc)
	}
	if svc.AppPort != conf.DefaultBashyAppsPort {
		t.Fatalf("apps port = %d, want %d", svc.AppPort, conf.DefaultBashyAppsPort)
	}
	if !svc.RequireLogin || !svc.ElevationRequired || !svc.TrustCloudIdentity {
		t.Fatalf("apps auth posture = require:%v elevation:%v trust:%v", svc.RequireLogin, svc.ElevationRequired, svc.TrustCloudIdentity)
	}
}

func TestBashyAppsExplicitOptOutSurvivesRoundTrip(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(tmp, []byte(`{"bashy_services":[{"name":"apps","enabled":false}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fc, err := conf.LoadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if fc.BashyAppsOn() {
		t.Fatal("explicit apps enabled:false must opt out")
	}
	if svc := findBashyService(fc, "apps"); svc == nil || svc.Enabled {
		t.Fatalf("effective apps service should stay disabled, got %+v", svc)
	}
	if err := conf.SaveFile(tmp, fc); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"enabled":false`) {
		t.Fatalf("explicit enabled:false was not persisted:\n%s", raw)
	}
}

func TestBashyAppsConfiguredServicesDoNotBroadenIntent(t *testing.T) {
	fc := &conf.FileConfig{BashyServices: []conf.BashyService{{Name: "sdlc", Enabled: true}}}
	if fc.BashyAppsOn() {
		t.Fatal("a curated bashy_services list without apps must not default apps on")
	}
	if svc := findBashyService(fc, "apps"); svc == nil || svc.Enabled {
		t.Fatalf("apps service should be present but disabled, got %+v", svc)
	}
}

func TestBashyAppsLegacyMissingKeyDefaultsOn(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(tmp, []byte(`{"agent_name":"old","server_addr":"edge","server_port":443,"token":"t","remote_port":7000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fc, err := conf.LoadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !fc.BashyAppsOn() {
		t.Fatal("legacy config missing bashy_services should default bashy Apps on")
	}
}

func TestBashyAppsPortOverridesAndLegacy8639Preserved(t *testing.T) {
	fc := &conf.FileConfig{BashyAppsPort: 24000}
	if got := findBashyService(fc, "apps").AppPort; got != 24000 {
		t.Fatalf("top-level apps port = %d, want 24000", got)
	}

	fc = &conf.FileConfig{BashyServices: []conf.BashyService{{
		Name: "apps", Enabled: true, EnabledSet: true, AppPort: 8639,
	}}}
	if got := findBashyService(fc, "apps").AppPort; got != 8639 {
		t.Fatalf("legacy apps app_port = %d, want 8639", got)
	}
}

func TestBashyAppsRegistersLoopbackCloudboxAppWithElevation(t *testing.T) {
	fc := &conf.FileConfig{}
	provisionBashyServiceSecrets(t, fc)
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	u := reg.LookupTarget("apps")
	if u == nil {
		t.Fatal("apps target not registered")
	}
	if u.Scheme != "http" || u.Host != "127.0.0.1:22749" {
		t.Fatalf("apps target = %s, want loopback http://127.0.0.1:22749", u)
	}
	for _, e := range reg.Entries() {
		if e.Name == "apps" {
			if !e.RequireLogin || !e.ElevationRequired {
				t.Fatalf("apps entry auth = require:%v elevation:%v", e.RequireLogin, e.ElevationRequired)
			}
			return
		}
	}
	t.Fatal("apps entry missing")
}

func TestBashyAppsProxyPrefixAndIdentity(t *testing.T) {
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
		mac := hmac.New(sha256.New, []byte("apps-secret"))
		_, _ = mac.Write([]byte(user + "\n" + role + "\n" + prefix + "\n" + ts))
		if user != "owner@example.com" || prefix != "/matrix/h/dragon/app/apps" || !hmac.Equal(sig, mac.Sum(nil)) {
			http.Error(w, "untrusted identity", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	fc := &conf.FileConfig{BashyServices: []conf.BashyService{{
		Name: "apps", Enabled: true, EnabledSet: true, AppPort: port, TrustCloudIdentity: true, SSOSecret: "apps-secret",
	}}}
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	router := gin.New()
	router.Any("/app/:name/*p", func(c *gin.Context) { reg.ProxyTo(c, c.Param("name"), c.Param("p")) })
	proxy := httptest.NewServer(router)
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodGet, proxy.URL+"/app/apps/ui", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Prefix", "/matrix/h/dragon/app/apps")
	req.Header.Set("X-Periscope-User", "owner@example.com")
	req.Header.Set("X-Periscope-Role", "owner")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy status = %d body=%s", resp.StatusCode, body)
	}
}

func TestBashyAppsCloudboxAndLANAuthGate(t *testing.T) {
	fc := &conf.FileConfig{}
	provisionBashyServiceSecrets(t, fc)
	reg := agent.NewAppRegistry()
	if err := registerBashyServiceApps(fc, reg); err != nil {
		t.Fatalf("registerBashyServiceApps: %v", err)
	}
	router := gin.New()
	router.Any("/app/:name/*p", func(c *gin.Context) { reg.ProxyTo(c, c.Param("name"), c.Param("p")) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/app/apps/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/matrix/h/dragon/app/apps")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cloudbox request without OS auth role = %d, want 403", w.Code)
	}

	admin := gin.New()
	admin.Use(func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
	admin.Any("/apps/*p", func(c *gin.Context) { reg.ProxyTo(c, "apps", c.Param("p")) })
	w = httptest.NewRecorder()
	admin.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("LAN/admin app route without host login = %d, want 401", w.Code)
	}
}

func TestStartBashyAppsPassesPortAndLANBind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX fake bashy script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	bin := filepath.Join(dir, "bashy")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(log) + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BASHY_BIN", bin)
	bashyResolver = &bashyBinaryResolver{}
	oldLANBind := bashyAppsLANBind
	bashyAppsLANBind = func() string { return "192.168.50.12" }
	t.Cleanup(func() { bashyAppsLANBind = oldLANBind })

	fc := &conf.FileConfig{AgentName: "dragon", ServerAddr: "ai.dhnt.io", ServerPort: 443, Protocol: "wss"}
	svc := *findBashyService(fc, "apps")
	if err := startBashyService(context.Background(), fc, svc); err != nil {
		t.Fatalf("startBashyService: %v", err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(raw))
	if !strings.Contains(got, "apps service start") || !strings.Contains(got, "--port 22749") {
		t.Fatalf("apps start args missing service/port: %q", got)
	}
	if !strings.Contains(got, "--bind 192.168.50.12") {
		t.Fatalf("apps start args missing exact private LAN bind: %q", got)
	}
	if strings.Contains(got, "--root-url") {
		t.Fatalf("apps start args contain unsupported --root-url: %q", got)
	}
}

type testNetAddr string

func (a testNetAddr) Network() string { return "ip+net" }
func (a testNetAddr) String() string  { return string(a) }

func TestFirstPrivateLANIPv4RejectsPublicAndLoopback(t *testing.T) {
	got := firstPrivateLANIPv4([]net.Addr{
		testNetAddr("127.0.0.1/8"),
		testNetAddr("203.0.113.40/24"),
		testNetAddr("192.168.44.9/24"),
	})
	if got != "192.168.44.9" {
		t.Fatalf("firstPrivateLANIPv4 = %q, want 192.168.44.9", got)
	}
	if got := firstPrivateLANIPv4([]net.Addr{testNetAddr("203.0.113.40/24")}); got != "" {
		t.Fatalf("public-only interfaces produced bind %q; want no LAN listener", got)
	}
}

func TestBashyAppsMissingCapabilityIsActionable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX fake bashy script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bashy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'unknown command: apps service' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BASHY_BIN", bin)
	bashyResolver = &bashyBinaryResolver{}
	err := startBashyService(context.Background(), &conf.FileConfig{}, *findBashyService(&conf.FileConfig{}, "apps"))
	if err == nil {
		t.Fatal("missing Apps capability should return an actionable error")
	}
	if msg := err.Error(); !strings.Contains(msg, "apps service start") || !strings.Contains(msg, "unknown command") {
		t.Fatalf("error not actionable: %v", err)
	}
}

func TestBashyAppsSupervisorRestartsStoppedService(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX fake bashy script")
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "starts")
	bin := filepath.Join(dir, "bashy")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *' start'*) n=0; [ -f " + strconv.Quote(count) + " ] && n=$(cat " + strconv.Quote(count) + "); n=$((n+1)); echo $n > " + strconv.Quote(count) + "; exit 0;;\n" +
		"  *' status'*) echo stopped; exit 0;;\n" +
		"  *' stop'*) exit 0;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BASHY_BIN", bin)
	bashyResolver = &bashyBinaryResolver{}
	old := bashyServicePollInterval
	bashyServicePollInterval = 10 * time.Millisecond
	t.Cleanup(func() { bashyServicePollInterval = old })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- superviseBashyService(ctx, &conf.FileConfig{}, conf.BashyService{Name: "apps", Enabled: true, Command: []string{"apps", "service"}}, nil)
	}()
	deadline := time.After(500 * time.Millisecond)
	for {
		raw, _ := os.ReadFile(count)
		if strings.TrimSpace(string(raw)) >= "2" {
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		}
		select {
		case <-deadline:
			cancel()
			_ = <-done
			t.Fatalf("service was not restarted; starts=%q", raw)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
