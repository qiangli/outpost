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
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

// testUnitStore pins a store to a scratch registry so a test never touches
// the developer's real units, and stubs the monitor's state.
func testUnitStore(t *testing.T, states []service.Status, seed ...fleet.Service) *svcunit.Store {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "services"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cat := fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
	for _, u := range seed {
		if err := cat.SaveService(u); err != nil {
			t.Fatalf("seed %s: %v", u.Name, err)
		}
	}
	return &svcunit.Store{Catalog: cat, Monitor: stubMonitor(states)}
}

type stubMonitor []service.Status

func (s stubMonitor) Statuses(context.Context) ([]service.Status, error) {
	return []service.Status(s), nil
}

// A unit whose app route asks for the trusted-identity contract reaches its
// upstream with headers the upstream can VERIFY: outpost signs
// user\nrole\nprefix\nts with the per-app HMAC key, so an attacker who can
// set Remote-User on a request that bypasses outpost still cannot forge one.
//
// This is the end-to-end contract the old meet-specific wiring proved. It is
// asserted here against a unit named nothing in particular, because the
// exposure path has no knowledge of which unit it is publishing.
func TestTrustedUnitAppReceivesVerifiableIdentity(t *testing.T) {
	const secret = "unit-test-secret"
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
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(user + "\n" + role + "\n" + prefix + "\n" + ts))
		if user == "" || !hmac.Equal(sig, mac.Sum(nil)) {
			http.Error(w, "untrusted identity", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer upstream.Close()
	port := listenerPort(t, upstream)

	unit := fleet.Service{
		Name: "rooms", Kind: fleet.KindService,
		Exec:  []string{"some-daemon", "serve"},
		Ports: []fleet.ServicePort{{Name: "http", Port: port}},
		Expose: fleet.ServiceExpose{App: &fleet.ServiceApp{
			Name: "rooms", RequireLogin: true, TrustCloudIdentity: true,
		}},
		Enabled: true,
	}
	st := testUnitStore(t, []service.Status{{Unit: "rooms", State: service.Active}}, unit)

	reg := agent.NewAppRegistry()
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	fc := &conf.FileConfig{ServiceSSO: map[string]string{"rooms": secret}}
	r := svcunit.New(svcunit.Deps{
		Source:  st,
		Apps:    appExposer{reg: reg},
		Secrets: serviceSecrets{path: cfgPath, fc: fc},
	})
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Any("/app/:name/*p", func(c *gin.Context) {
		reg.ProxyTo(c, c.Param("name"), c.Param("p"))
	})
	proxy := httptest.NewServer(router)
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodGet, proxy.URL+"/app/rooms/api/rooms", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Prefix", "/matrix/h/dragon/app/rooms")
	req.Header.Set("X-Periscope-User", "owner@example.com")
	req.Header.Set("X-Periscope-Role", "owner")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "[]" {
		t.Fatalf("GET through the published route = %d, %q; want 200, []", resp.StatusCode, body)
	}
}

// The HMAC key a unit's trust_cloud_identity asks for is MINTED on first
// publication and persisted, because a fresh key on every boot would
// silently invalidate every signature the upstream had learned.
func TestTrustedUnitSSOKeyIsMintedOnceAndPersisted(t *testing.T) {
	unit := fleet.Service{
		Name: "console", Kind: fleet.KindService,
		Exec:  []string{"some-daemon", "serve"},
		Ports: []fleet.ServicePort{{Name: "http", Port: 22749}},
		Expose: fleet.ServiceExpose{App: &fleet.ServiceApp{
			Name: "console", RequireLogin: true, Elevation: true, TrustCloudIdentity: true,
		}},
		Enabled: true,
	}
	st := testUnitStore(t, []service.Status{{Unit: "console", State: service.Active}}, unit)
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	fc := &conf.FileConfig{}
	reg := agent.NewAppRegistry()
	r := svcunit.New(svcunit.Deps{
		Source:  st,
		Apps:    appExposer{reg: reg},
		Secrets: serviceSecrets{path: cfgPath, fc: fc},
	})
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	minted := fc.ServiceSSO["console"]
	if minted == "" {
		t.Fatal("no sso key was minted for a trust_cloud_identity unit")
	}
	if got := reg.SSOSecret("console"); got != minted {
		t.Fatalf("registry signs with %q but the config holds %q", got, minted)
	}
	// Persisted: a reload of the file sees the same key.
	saved, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if saved.ServiceSSO["console"] != minted {
		t.Fatalf("minted key was not persisted: %q", saved.ServiceSSO["console"])
	}
	// And a second reconcile does not rotate it.
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if fc.ServiceSSO["console"] != minted {
		t.Fatal("the sso key rotated on an unchanged reconcile")
	}
	// The declared elevation requirement survived into the live registry.
	entries := reg.Entries()
	var found bool
	for _, e := range entries {
		if e.Name == "console" {
			found = true
			if !e.RequireLogin {
				t.Fatal("require_login from the declaration was dropped")
			}
		}
	}
	if !found {
		t.Fatalf("console is not in the registry: %+v", entries)
	}
}

// localForgeURL replaces `fc.LoomOn() && fc.LoomPortOrDefault()`: the forge
// a dependent unit reaches comes from the loom unit's declaration, so an
// operator port override moves both and there is no second copy to drift.
func TestLocalForgeURLFollowsTheUnitDeclaration(t *testing.T) {
	loom := fleet.Service{
		Name: "loom", Kind: fleet.KindService,
		Exec:    []string{"bashy", "loom", "serve"},
		Ports:   []fleet.ServicePort{{Name: "http", Port: 31880}},
		Enabled: false,
	}
	st := testUnitStore(t, nil, loom)

	if got := localForgeURL(st, "loom"); got != "" {
		t.Fatalf("a disabled loom resolved to %q, want no forge", got)
	}
	if err := st.SetEnabled("loom", true); err != nil {
		t.Fatalf("enable loom: %v", err)
	}
	if got := localForgeURL(st, "loom"); got != "http://127.0.0.1:31880" {
		t.Fatalf("forge = %q, want the declared port", got)
	}
	over, _ := st.Unit("loom")
	over.Ports[0].Port = 31900
	if err := st.Save(over); err != nil {
		t.Fatalf("override port: %v", err)
	}
	if got := localForgeURL(st, "loom"); got != "http://127.0.0.1:31900" {
		t.Fatalf("forge after override = %q, want the overridden port", got)
	}
	if got := localForgeURL(st, "no-such-unit"); got != "" {
		t.Fatalf("an undeclared unit resolved to %q", got)
	}

}

// Declaration overrides go through `bashy service set` because fleet's
// dotted-path editor is unexported. Assert the argv, not the effect: the
// effect is fleet's and is covered by fleet's own tests.
func TestSetFieldsCallsTheUnitStoresOwnSetVerb(t *testing.T) {
	st := testUnitStore(t, nil, fleet.Service{
		Name: "loom", Kind: fleet.KindService,
		Exec: []string{"bashy", "loom", "serve"},
	})
	var argv []string
	ctl := &serviceControl{Store: st, cli: func(_ context.Context, args ...string) ([]byte, error) {
		argv = args
		return nil, nil
	}}
	err := ctl.SetFields(context.Background(), "loom", map[string]string{
		"settings.root_url": "https://x", "ports.0.port": "31900",
	})
	if err != nil {
		t.Fatalf("set fields: %v", err)
	}
	want := "service set loom --set ports.0.port=31900 --set settings.root_url=https://x"
	if got := strings.Join(argv, " "); got != want {
		t.Fatalf("argv = %q\nwant      %q", got, want)
	}

	// An undeclared unit is refused before any subprocess runs.
	argv = nil
	if err := ctl.SetFields(context.Background(), "nope", map[string]string{"a": "b"}); err == nil {
		t.Fatal("expected a refusal for an undeclared unit")
	}
	if argv != nil {
		t.Fatalf("a subprocess ran for an undeclared unit: %v", argv)
	}
}

func TestParseServiceToggles(t *testing.T) {
	got, err := parseServiceToggles([]string{"meet=on", "loom=off", "openclaw=true"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got["meet"] || got["loom"] || !got["openclaw"] {
		t.Fatalf("toggles = %v", got)
	}
	for _, bad := range []string{"meet", "=on", "meet=maybe"} {
		if _, err := parseServiceToggles([]string{bad}); err == nil {
			t.Fatalf("--service %q was accepted", bad)
		}
	}
}

func TestParseServiceSets(t *testing.T) {
	got, err := parseServiceSets([]string{"loom.ports.0.port=31900", "loom.settings.root_url=https://x/y=z"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["loom"]["ports.0.port"] != "31900" {
		t.Fatalf("sets = %v", got)
	}
	// Only the FIRST '=' splits: a value may contain one.
	if got["loom"]["settings.root_url"] != "https://x/y=z" {
		t.Fatalf("value with an '=' was truncated: %q", got["loom"]["settings.root_url"])
	}
	for _, bad := range []string{"loom=1", "loom.", ".port=1", "noequals"} {
		if _, err := parseServiceSets([]string{bad}); err == nil {
			t.Fatalf("--service-set %q was accepted", bad)
		}
	}
}

// The init boundary: outpost's own capability registry must never claim a
// capability twice, and the wiring must survive being empty.
func TestOutpostCapabilitiesRegisterCleanly(t *testing.T) {
	caps := outpostCapabilities(capabilityDeps{})
	prov, err := svcunit.Hosted(caps)
	if err != nil {
		t.Fatalf("outpost's capability wiring is rejected by the registry: %v", err)
	}
	if prov == nil {
		t.Fatal("no internal provider was built")
	}
	if got := prov.Name(); got != fleet.ServiceProviderInternal {
		t.Fatalf("provider name = %q, want %q", got, fleet.ServiceProviderInternal)
	}
	// Every registered capability must satisfy the contract rather than
	// panicking on a nil closure: an id here is a promise the monitor keeps.
	for id := range caps {
		u := fleet.Service{
			Name: id, Kind: fleet.KindService, Provider: fleet.ServiceProviderInternal,
			Capability: id, Owner: fleet.ServiceOwnerOutpost, Enabled: true,
		}
		if err := prov.Deactivate(context.Background(), u.Name); err != nil {
			t.Fatalf("capability %q: Deactivate on an inactive unit must be a no-op: %v", id, err)
		}
	}
}

func listenerPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// Unrelated to service units, and retained from the removed meet tests: a
// custom APP (agent.json `apps`) that asks for the trusted-identity contract
// without a key must be refused at boot rather than proxied with a blank
// signature the upstream would reject on every request.
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

// The CI runner capability reads its forge from the loom unit when the
// declaration names none, and refuses to activate when neither is available
// — the old boot block logged "skipping" and returned, which left an
// enabled unit looking fine while nothing ran.
func TestActrunnerCapabilityResolvesInstanceFromTheForgeUnit(t *testing.T) {
	st := testUnitStore(t, nil, fleet.Service{
		Name: "loom", Kind: fleet.KindService,
		Exec:    []string{"bashy", "loom", "serve"},
		Ports:   []fleet.ServicePort{{Name: "http", Port: 31880}},
		Enabled: true,
	})
	cap := &actrunnerCapability{Forge: func(unit string) string { return localForgeURL(st, unit) }}

	// No instance declared: the enabled loom unit supplies it. Registration
	// is not attempted (no token, no .runner), so activation is refused with
	// the actionable reason rather than silently doing nothing.
	cfg := service.Config{Unit: "ci", Settings: map[string]string{"data_dir": t.TempDir()}}
	err := cap.Activate(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "registration token") {
		t.Fatalf("activate without a token = %v; want a refusal naming the token", err)
	}

	// A declaration that names neither an instance nor a reachable forge
	// unit is refused for THAT reason, so an operator can tell the two apart.
	cap.Forge = func(string) string { return "" }
	err = cap.Activate(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "no forge instance") {
		t.Fatalf("activate without a forge = %v; want a refusal naming the forge", err)
	}

	// Settings are the declaration's half of the contract.
	got := readActrunnerSettings(service.Config{
		Unit: "ci",
		Settings: map[string]string{
			"instance": "https://forge.example.com", "labels": "host:host,gpu:gpu",
			"sandbox": "on", "sandbox_image": "img", "version": "0.2.13",
		},
		Env: map[string]string{"ACTRUNNER_TOKEN": "vault-token"},
	})
	if got.instance != "https://forge.example.com" || got.labels != "host:host,gpu:gpu" ||
		!got.sandbox || got.sandboxImage != "img" || got.version != "0.2.13" {
		t.Fatalf("settings = %+v", got)
	}
	// The token comes from the vault-resolved env, never from plaintext YAML
	// when both are present.
	if got.token != "vault-token" {
		t.Fatalf("token = %q, want the env (vault) value", got.token)
	}
	if plain := readActrunnerSettings(service.Config{
		Unit: "ci", Settings: map[string]string{"token": "yaml-token"},
	}); plain.token != "yaml-token" {
		t.Fatalf("settings.token = %q, want the declared fallback", plain.token)
	}
	// Unset labels fall back to the host lane, as the retired
	// actrunner_labels default did.
	if bare := readActrunnerSettings(service.Config{Unit: "ci"}); bare.labels != "host:host" {
		t.Fatalf("default labels = %q, want host:host", bare.labels)
	}
}
