package agent

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// A site app's /mcp is the same origin and the same stamp as its pages: the
// engine receives POST and the SSE GET at /mcp, signed with the app's key
// for the same caller and prefix, the event stream arrives unbuffered, and
// nothing is redirected.
func TestSiteMCPProxySameOriginSameStamp(t *testing.T) {
	type seen struct{ method, path, user, rel, sig, ts, prefix string }
	pagesCh, mcpCh := make(chan seen, 4), make(chan seen, 4)
	record := func(ch chan seen, r *http.Request) {
		ch <- seen{r.Method, r.URL.Path, r.Header.Get("Remote-User"), r.Header.Get("X-Outpost-Relationship"),
			r.Header.Get("X-Outpost-Relationship-Sig"), r.Header.Get("X-Outpost-Identity-Ts"), r.Header.Get("X-Forwarded-Prefix")}
	}
	pages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { record(pagesCh, r) }))
	t.Cleanup(pages.Close)
	release := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(mcpCh, r)
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("event: message\ndata: first\n\n"))
			w.(http.Flusher).Flush()
			<-release
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(engine.Close)
	t.Cleanup(func() { close(release) })
	phost, pport := splitHostPort(t, pages.URL)
	_, eport := splitHostPort(t, engine.URL)

	const secret = "s3cret"
	reg := NewAppRegistry()
	reg.trustLoopback = true
	if err := reg.RegisterFromConfig(conf.AppConfig{Name: "site", Scheme: "http", Host: phost, Port: pport, BasePath: "/site",
		Enabled: true, TrustCloudIdentity: true, SSOSecret: secret, MCPPort: eport}); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.SetHostGrants(NewHostGrants("owner@example.com"))
	front := httptest.NewServer(newTestRouter(reg))
	t.Cleanup(front.Close)

	do := func(method, path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, front.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("X-Forwarded-Prefix", "/matrix/h/host-a/app/site")
		req.Header.Set("X-Periscope-User", "owner@example.com")
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	next := func(ch chan seen) seen {
		t.Helper()
		select {
		case got := <-ch:
			return got
		case <-time.After(2 * time.Second):
			t.Fatal("upstream never received the request")
			return seen{}
		}
	}

	resp := do(http.MethodGet, "/app/site/about")
	resp.Body.Close()
	page := next(pagesCh)

	resp = do(http.MethodPost, "/app/site/mcp")
	body := new(strings.Builder)
	bufio.NewReader(resp.Body).WriteTo(body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body.String(), `"result"`) {
		t.Fatalf("POST /mcp = %d %q", resp.StatusCode, body)
	}
	post := next(mcpCh)
	if post.method != http.MethodPost || post.path != "/mcp" {
		t.Fatalf("engine saw %s %s, want POST /mcp (not under the pages' base path)", post.method, post.path)
	}
	if post.user != page.user || post.rel != "owner" || page.rel != "owner" || post.prefix != page.prefix {
		t.Fatalf("mcp stamp %+v differs from pages stamp %+v", post, page)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(relationshipPayload(post.rel, "", "", "", post.user, post.prefix, post.ts)))
	if post.sig != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("mcp relationship stamp does not verify with the app key: %+v", post)
	}

	// The SSE GET streams: the first event arrives while the engine still
	// holds the stream open.
	resp = do(http.MethodGet, "/app/site/mcp")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("GET /mcp = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "event: message\n" {
			t.Fatalf("first SSE line = %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSE event buffered by the proxy")
	}
	if got := next(mcpCh); got.method != http.MethodGet || got.user != "owner@example.com" {
		t.Fatalf("engine SSE request = %+v", got)
	}
}

// The authorization metadata names the site's own origin, is readable before
// sign-in even on a login-required site, and a refused MCP call is a 401
// challenge pointing at it, never a redirect elsewhere.
func TestSiteMCPMetadataAndChallenge(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(engine.Close)
	host, port := splitHostPort(t, engine.URL)
	reg := NewAppRegistry()
	reg.trustLoopback = true
	if err := reg.RegisterFromConfig(conf.AppConfig{Name: "site", Scheme: "http", Host: host, Port: port, Enabled: true,
		RequireLogin: true, TrustCloudIdentity: true, SSOSecret: "k", MCPPort: port}); err != nil {
		t.Fatal(err)
	}
	reg.SetHostGrants(NewHostGrants("owner@example.com"))
	front := httptest.NewServer(newTestRouter(reg))
	t.Cleanup(front.Close)

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/app/site/.well-known/oauth-protected-resource", nil)
	req.Header.Set("X-Forwarded-Prefix", "/h/host-a/app/site")
	req.Header.Set("X-Forwarded-Host", "dhnt.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	origin := "https://dhnt.example/h/host-a/app/site"
	if resp.StatusCode != http.StatusOK || meta.Resource != origin+"/mcp" || len(meta.AuthorizationServers) != 1 || meta.AuthorizationServers[0] != origin {
		t.Fatalf("metadata = %d %+v", resp.StatusCode, meta)
	}

	req, _ = http.NewRequest(http.MethodPost, front.URL+"/app/site/mcp", strings.NewReader("{}"))
	req.Header.Set("X-Forwarded-Prefix", "/h/host-a/app/site")
	req.Header.Set("X-Forwarded-Host", "dhnt.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err = (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized ||
		resp.Header.Get("WWW-Authenticate") != `Bearer resource_metadata="`+origin+`/.well-known/oauth-protected-resource"` {
		t.Fatalf("anonymous /mcp on a login-required site = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// The page path keeps the pages' 403; an app without MCPPort has no /mcp.
	req, _ = http.NewRequest(http.MethodGet, front.URL+"/app/site/about", nil)
	req.Header.Set("X-Forwarded-Prefix", "/h/host-a/app/site")
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("page gate = %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if err := reg.RegisterFromConfig(conf.AppConfig{Name: "site", Scheme: "http", Host: host, Port: port, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	reg.mu.RLock()
	_, still := reg.siteMCP["site"]
	reg.mu.RUnlock()
	if still {
		t.Fatal("re-registering without mcp_port must drop the MCP proxy")
	}
}
