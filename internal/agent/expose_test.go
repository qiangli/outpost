package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// exposeHash returns the hex sha256 of tok — what the operator hands
// to `outpost apps add --expose-token-sha256`.
func exposeHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// splitHostPort splits an httptest server URL into host and port.
func splitHostPortOf(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", raw, err)
	}
	return u.Hostname(), port
}

// TestExposeGuardRefusesMissingWrongAndExpiredToken: an HTTP app
// registered with an expose hash serves only requests carrying the
// matching X-Dhnt-Expose-Token, and only until expose_expires. The
// header never reaches the upstream.
func TestExposeGuardRefusesMissingWrongAndExpiredToken(t *testing.T) {
	const token = "expose-token-for-test"

	var leaked string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-Dhnt-Expose-Token"); v != "" {
			leaked = v
		}
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()
	host, port := splitHostPortOf(t, upstream.URL)

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "web", Scheme: "http", Host: host, Port: port, Enabled: true,
		ExposeTokenSHA256: exposeHash(token),
		ExposeExpires:     time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("register guarded app: %v", err)
	}
	// Same upstream, already expired.
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "old", Scheme: "http", Host: host, Port: port, Enabled: true,
		ExposeTokenSHA256: exposeHash(token),
		ExposeExpires:     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("register expired app: %v", err)
	}
	// No expose hash — unchanged behaviour.
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "open", Scheme: "http", Host: host, Port: port, Enabled: true,
	}); err != nil {
		t.Fatalf("register open app: %v", err)
	}

	front := httptest.NewServer(newTestRouter(reg))
	defer front.Close()

	get := func(path, tok string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, front.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if tok != "" {
			req.Header.Set("X-Dhnt-Expose-Token", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, body := get("/app/web/", ""); code != http.StatusForbidden {
		t.Errorf("missing token: status=%d body=%q, want 403", code, body)
	}
	if code, body := get("/app/web/", "wrong-token"); code != http.StatusForbidden {
		t.Errorf("wrong token: status=%d body=%q, want 403", code, body)
	}
	if code, body := get("/app/old/", token); code != http.StatusForbidden {
		t.Errorf("expired app: status=%d body=%q, want 403", code, body)
	}
	if code, body := get("/app/web/", token); code != http.StatusOK || body != "upstream-ok" {
		t.Errorf("valid token: status=%d body=%q, want 200 upstream-ok", code, body)
	}
	if leaked != "" {
		t.Errorf("upstream saw X-Dhnt-Expose-Token=%q, want it stripped", leaked)
	}
	// Unguarded app keeps working with no header at all.
	if code, body := get("/app/open/", ""); code != http.StatusOK || body != "upstream-ok" {
		t.Errorf("unguarded app: status=%d body=%q, want 200 upstream-ok", code, body)
	}
}

// TestExposeGuardAppliesToTCPAndEgressWebSocket: the guard runs before
// the WebSocket upgrade, so a tokenless tcp- or egress-scheme dial is
// refused with 403 at the handshake (never accepted); with the token
// the bridge works.
func TestExposeGuardAppliesToTCPAndEgressWebSocket(t *testing.T) {
	egressAllowLoopbackForTest = true
	t.Cleanup(func() { egressAllowLoopbackForTest = false })

	const token = "expose-token-ws"
	port, done := startTCPEcho(t)
	defer done()

	reg := NewAppRegistry()
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "raw", Scheme: "tcp", Host: "127.0.0.1", Port: port, Enabled: true,
		ExposeTokenSHA256: exposeHash(token), ExposeExpires: expires,
	}); err != nil {
		t.Fatalf("register tcp app: %v", err)
	}
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "eg", Scheme: "egress", Enabled: true,
		Allow:             []string{"127.0.0.1:" + strconv.Itoa(port)},
		ExposeTokenSHA256: exposeHash(token), ExposeExpires: expires,
	}); err != nil {
		t.Fatalf("register egress app: %v", err)
	}

	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()
	wsBase := strings.Replace(srv.URL, "http", "ws", 1)

	refused := func(wsURL string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, resp, err := websocket.Dial(ctx, wsURL, nil)
		if err == nil {
			ws.Close(websocket.StatusNormalClosure, "done")
			t.Fatalf("ws dial %s succeeded, want 403 before accept", wsURL)
		}
		if resp == nil {
			t.Fatalf("ws dial %s: no handshake response (err %v)", wsURL, err)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("ws dial %s: status=%d, want 403", wsURL, resp.StatusCode)
		}
	}
	refused(wsBase + "/app/raw/")
	refused(wsBase + "/app/eg/?dst=127.0.0.1:" + strconv.Itoa(port))

	echo := func(wsURL string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hdr := http.Header{}
		hdr.Set("X-Dhnt-Expose-Token", token)
		ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			t.Fatalf("ws dial %s with token: %v", wsURL, err)
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")
		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		defer conn.Close()
		want := []byte("hello expose")
		if _, err := conn.Write(want); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("echo got %q, want %q", got, want)
		}
	}
	echo(wsBase + "/app/raw/")
	echo(wsBase + "/app/eg/?dst=127.0.0.1:" + strconv.Itoa(port))
}

// TestExposeGuardAppliesToGitApp: an exposed git app is guarded like
// every other kind. The guard runs BEFORE the git handler, so a
// tokenless or wrong-token clone is refused with 403 without the
// allowlist, the credential filler or the upstream ever being reached;
// with the token the clone proceeds and the upstream never sees the
// expose header.
func TestExposeGuardAppliesToGitApp(t *testing.T) {
	const token = "expose-token-git"

	var mu sync.Mutex
	hits := 0
	var leaked string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		if v := r.Header.Get("X-Dhnt-Expose-Token"); v != "" {
			leaked = v
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "001e# service=git-upload-pack")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	reg := NewAppRegistry()
	filler := &fakeGitFiller{user: "u", pass: "p"}
	reg.gitCredentialFill = filler.fill
	if err := reg.registerGit("code", target, []string{"octocat/hello"}, AppMeta{
		ExposeTokenSHA256: exposeHash(token),
		ExposeExpires:     time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("registerGit guarded: %v", err)
	}

	front := httptest.NewServer(newTestRouter(reg))
	defer front.Close()

	// An otherwise-allowlisted clone: only the expose token is in question.
	const clone = "/app/code/octocat/hello.git/info/refs?service=git-upload-pack"
	get := func(tok string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, front.URL+clone, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if tok != "" {
			req.Header.Set("X-Dhnt-Expose-Token", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", clone, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if code := get(""); code != http.StatusForbidden {
		t.Errorf("git clone without token: status=%d, want 403", code)
	}
	if code := get("wrong-token"); code != http.StatusForbidden {
		t.Errorf("git clone with wrong token: status=%d, want 403", code)
	}
	mu.Lock()
	refusedHits := hits
	mu.Unlock()
	if refusedHits != 0 {
		t.Errorf("upstream hit %d times by refused requests, want 0", refusedHits)
	}
	if n := filler.callCount(); n != 0 {
		t.Errorf("credential fill called %d times for refused requests, want 0", n)
	}

	if code := get(token); code != http.StatusOK {
		t.Errorf("git clone with valid token: status=%d, want 200", code)
	}
	mu.Lock()
	okHits, sawHeader := hits, leaked
	mu.Unlock()
	if okHits != 1 {
		t.Errorf("upstream hit %d times, want 1 (the token-carrying clone)", okHits)
	}
	if sawHeader != "" {
		t.Errorf("git upstream saw X-Dhnt-Expose-Token=%q, want it stripped", sawHeader)
	}
}
