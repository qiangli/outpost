package adminui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

const testMCPToken = "synthetic-mcp-bearer-0123456789abcdef0123456789"

// bearerServer is a paired (gate engaged) loopback admin server with the
// MCP token wired, as main.go does.
func bearerServer(t *testing.T) *Server {
	t.Helper()
	return bearerServerWith(t, &conf.FileConfig{AgentName: "x", Token: "t"})
}

func bearerServerWith(t *testing.T, fc *conf.FileConfig) *Server {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, fc); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, configPath, nil, nil)
	tok := testMCPToken
	s.deps.MCPToken = func() string { return tok }
	s.deps.RotateMCPToken = func() (string, error) { tok = "rotated-" + testMCPToken; return tok, nil }
	s.deps.MCPEndpoint = "http://127.0.0.1:1/mcp/"
	s.engine = gin.New()
	s.registerRoutes()
	return s
}

func bearerReq(s *Server, method, path, remote, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	return w
}

func TestBearerAdmitsLoopbackAsOSUser(t *testing.T) {
	s := bearerServer(t)
	w := bearerReq(s, http.MethodGet, "/api/status", "127.0.0.1:5000", "Bearer "+testMCPToken)
	if w.Code != http.StatusOK {
		t.Fatalf("valid bearer on loopback = %d, want 200: %s", w.Code, w.Body.String())
	}
	cur, _ := hostauth.CurrentUser()
	if !strings.Contains(w.Body.String(), cur) {
		t.Errorf("identity is not the OS user: %s", w.Body.String())
	}
	if w = bearerReq(s, http.MethodGet, "/api/status", "[::1]:5000", "Bearer "+testMCPToken); w.Code != http.StatusOK {
		t.Errorf("valid bearer on ::1 = %d, want 200", w.Code)
	}
}

func TestBearerDenied(t *testing.T) {
	s := bearerServer(t)
	cases := []struct {
		name, remote, auth string
	}{
		{"absent", "127.0.0.1:5000", ""},
		{"wrong", "127.0.0.1:5000", "Bearer wrong-" + testMCPToken},
		{"empty", "127.0.0.1:5000", "Bearer "},
		{"prefix of real", "127.0.0.1:5000", "Bearer " + testMCPToken[:10]},
		{"wrong scheme", "127.0.0.1:5000", "Basic " + testMCPToken},
		{"non-loopback peer", "10.0.0.7:5000", "Bearer " + testMCPToken},
		{"unparseable peer", "garbage", "Bearer " + testMCPToken},
	}
	for _, tc := range cases {
		w := bearerReq(s, http.MethodGet, "/api/status", tc.remote, tc.auth)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", tc.name, w.Code)
		}
		if strings.Contains(w.Body.String(), testMCPToken) {
			t.Errorf("%s: response echoes the token", tc.name)
		}
	}
}

func TestBearerIgnoresSpoofedForwarding(t *testing.T) {
	s := bearerServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.RemoteAddr = "10.0.0.7:5000"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("Authorization", "Bearer "+testMCPToken)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed X-Forwarded-For admitted a remote peer: %d", w.Code)
	}
}

func TestBearerNeverOnNonLoopbackBind(t *testing.T) {
	s := bearerServer(t)
	s.loopbackOnly = false
	w := bearerReq(s, http.MethodGet, "/api/status", "127.0.0.1:5000", "Bearer "+testMCPToken)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer admitted on a LAN-bound listener: %d", w.Code)
	}
}

func TestBearerRotationRevokesOldToken(t *testing.T) {
	s := bearerServer(t)
	if _, err := s.deps.RotateMCPToken(); err != nil {
		t.Fatal(err)
	}
	if w := bearerReq(s, http.MethodGet, "/api/status", "127.0.0.1:5000", "Bearer "+testMCPToken); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token admitted: %d", w.Code)
	}
	if w := bearerReq(s, http.MethodGet, "/api/status", "127.0.0.1:5000", "Bearer rotated-"+testMCPToken); w.Code != http.StatusOK {
		t.Fatalf("rotated token refused: %d", w.Code)
	}
}

func TestBearerCannotReachCredentialEndpoints(t *testing.T) {
	s := bearerServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/mcp/credentials"},
		{http.MethodPost, "/api/mcp/token/rotate"},
		{http.MethodGet, "/api/cluster/control-plane/token"},
		{http.MethodPost, "/api/cluster/control-plane/token/rotate"},
	} {
		w := bearerReq(s, tc.method, tc.path, "127.0.0.1:5000", "Bearer "+testMCPToken)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, w.Code)
		}
		if strings.Contains(w.Body.String(), testMCPToken) {
			t.Errorf("%s %s leaked the token", tc.method, tc.path)
		}
	}
}

func TestBearerDoesNotLoginOrOpenLocalAppProxy(t *testing.T) {
	s := bearerServer(t)
	// No cookie is minted by a bearer request.
	w := bearerReq(s, http.MethodGet, "/api/status", "127.0.0.1:5000", "Bearer "+testMCPToken)
	if c := w.Result().Cookies(); len(c) != 0 {
		t.Errorf("bearer request minted cookies: %v", c)
	}
	// Local-app proxy stays cookie-only (and unknown names stay 404).
	if w := bearerReq(s, http.MethodGet, "/nosuchapp/x", "127.0.0.1:5000", "Bearer "+testMCPToken); w.Code != http.StatusNotFound {
		t.Errorf("unknown app = %d, want 404", w.Code)
	}
}

func TestCookieGateUnchangedWithoutBearer(t *testing.T) {
	s := bearerServer(t)
	w := doJSON(s, http.MethodGet, "/api/status", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("standalone gate weakened: %d", w.Code)
	}
}

func TestLocalAppProxyDoesNotForwardBearer(t *testing.T) {
	s := bearerServer(t)
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	if err := s.core.Deps().Apps.Register("probe", up.URL); err != nil {
		t.Fatal(err)
	}
	cookie, err := s.sessions.Mint("someone")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(s.engine)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/probe/x", nil)
	req.Header.Set("Authorization", "Bearer "+testMCPToken)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("proxy = %d, want 204", resp.StatusCode)
	}
	if seen != "" {
		t.Fatalf("upstream app received the admin bearer: %q", seen)
	}
}

const (
	testProvToken = "synthetic-provisioning-token-aaaaaaaaaaaaaaaa"
	testSSOSecret = "synthetic-sso-secret-bbbbbbbbbbbbbbbbbbbbbbbb"
	testSvcSecret = "synthetic-service-sso-secret-cccccccccccccc"
)

func secretServer(t *testing.T) *Server {
	t.Helper()
	return bearerServerWith(t, &conf.FileConfig{
		AgentName: "x", Token: "t",
		Apps: []conf.AppConfig{{
			Name: "probe", Scheme: "http", Host: "127.0.0.1", Port: 9, Enabled: true,
			ProvisioningToken: testProvToken, SSOSecret: testSSOSecret,
		}},
		BashyServices: []conf.BashyService{{
			Name: "svc", Enabled: true, TrustCloudIdentity: true, SSOSecret: testSvcSecret,
		}},
	})
}

func TestBearerCannotRotateProvisioningToken(t *testing.T) {
	s := secretServer(t)
	w := bearerReq(s, http.MethodPost, "/api/apps/probe/provisioning-token/rotate", "127.0.0.1:5000", "Bearer "+testMCPToken)
	if w.Code != http.StatusForbidden {
		t.Fatalf("bearer rotated a provisioning token: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "provisioning_token") {
		t.Errorf("response carries a token: %s", w.Body.String())
	}
}

func TestBearerReadsNeverCarryAppSecrets(t *testing.T) {
	s := secretServer(t)
	for _, path := range []string{"/api/apps", "/api/config"} {
		w := bearerReq(s, http.MethodGet, path, "127.0.0.1:5000", "Bearer "+testMCPToken)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "probe") {
			t.Errorf("GET %s lost the (non-secret) app row: %s", path, body)
		}
		for _, secret := range []string{testProvToken, testSSOSecret, testSvcSecret, testMCPToken} {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s leaked a secret to the bearer caller", path)
			}
		}
	}
}

func TestSessionReadsKeepAppSecretsForTheInteractiveUI(t *testing.T) {
	s := secretServer(t)
	cookie, err := s.sessions.Mint("someone")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), testProvToken) {
		t.Fatalf("interactive session no longer sees app tokens: %d %s", w.Code, w.Body.String())
	}
}
