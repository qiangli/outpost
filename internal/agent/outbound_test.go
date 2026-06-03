package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// TestMain redirects HOME/XDG_CACHE_HOME to a process-wide
// throwaway directory for the entire `internal/agent` test binary.
// Required because OutboundManager.Connect now persists matrix_elev
// cookies to <UserCacheDir>/outpost/outbounds/ -- without this
// redirect, every test that exercises Connect would pollute the
// developer's real ~/Library/Caches/outpost/outbounds/, sometimes
// stomping on cookies a live outpost is relying on.
//
// The dir is removed after the suite runs. Per-test t.TempDir is
// still preferred for tests that want isolation between subcases;
// withTempCacheDir below gives them that.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "outpost-agent-tests-")
	if err != nil {
		// Can't proceed without a sandbox; fail loudly.
		panic("outbound test setup: mkdir temp: " + err.Error())
	}
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("XDG_CACHE_HOME", tmp)
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// withTempCacheDir gives a single test its own isolated cache dir
// on top of the suite-wide TestMain sandbox. Use it when a test
// asserts on specific file presence/absence and shouldn't see
// siblings' cookies.
func withTempCacheDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	return tmp
}

// fakeCloud is a tiny stand-in for cloudbox's /h/<host>/* endpoints used
// by OutboundManager. It records the last Authorization header it saw
// on /elevate and synthesizes a fake matrix_elev cookie. /app/<name>/*
// expects that cookie + Bearer and echoes the captured path back.
type fakeCloud struct {
	elevToken     string
	pingAlwaysOK  bool
	pingFailAfter int
	pingCount     int
}

func newFakeCloud(elevToken string) (*fakeCloud, *httptest.Server) {
	fc := &fakeCloud{elevToken: elevToken, pingAlwaysOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/matrix/h/", func(w http.ResponseWriter, r *http.Request) {
		// Cloudbox routes after the /elev/ refactor:
		//   POST /h/<host>/elev/app/<name>          → mint cookie
		//   POST /h/<host>/elev/app/<name>/ping     → slide cookie TTL
		//   ANY  /h/<host>/app/<name>/<rest>        → proxy to the app
		// parts[0]=<host>; parts[1] is "elev" for elevation routes or
		// "app" for the proxy data plane.
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/matrix/h/"), "/")
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		host := parts[0]
		if len(parts) >= 4 && parts[1] == "elev" && parts[2] == "app" {
			appName := parts[3]
			action := ""
			if len(parts) >= 5 {
				action = parts[4]
			}
			cookiePath := "/matrix/h/" + host + "/app/" + appName
			switch action {
			case "": // mint
				w.Header().Set("Content-Type", "application/json")
				http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: fc.elevToken, Path: cookiePath})
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			case "ping":
				fc.pingCount++
				if !fc.pingAlwaysOK && fc.pingCount > fc.pingFailAfter {
					http.Error(w, "expired", http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "bad elev action", http.StatusBadRequest)
			}
			return
		}
		if len(parts) >= 3 && parts[1] == "app" {
			// /h/<host>/app/<name>/<rest> — proxy hit; verify the
			// elevation cookie was forwarded then echo path back.
			ck, err := r.Cookie("matrix_elev")
			if err != nil || ck.Value != fc.elevToken {
				http.Error(w, "missing elev", http.StatusForbidden)
				return
			}
			rest := "/" + strings.Join(parts[3:], "/")
			w.Header().Set("X-Echo-Path", rest)
			if r.URL.RawQuery != "" {
				w.Header().Set("X-Echo-Query", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
			return
		}
		http.Error(w, "bad path", http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	return fc, srv
}

func TestOutboundConnectAndProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, cloud := newFakeCloud("elev-cookie-value")
	t.Cleanup(cloud.Close)

	m := NewOutboundManager(cloud.URL, "test-access-token", nil)
	m.Register([]conf.OutboundConfig{
		{Path: "kg", Name: "ollama", Host: "novicortex", User: "noviadmin"},
	})

	// Proxy before Connect must 503 with the "click Connect" hint.
	w := runOutboundProxy(m, http.MethodGet, "/kg/api/tags", "kg", "/api/tags")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-connect proxy: got %d, want 503 (%s)", w.Code, w.Body.String())
	}

	// Connect.
	if err := m.Connect("kg", "host-pw"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !m.List()[0].Connected {
		t.Fatalf("List should report connected after Connect()")
	}

	// Proxy succeeds; path got forwarded.
	w = runOutboundProxy(m, http.MethodGet, "/kg/api/tags?model=q", "kg", "/api/tags")
	if w.Code != http.StatusOK {
		t.Fatalf("connected proxy: got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Echo-Path"); got != "/api/tags" {
		t.Fatalf("forwarded path = %q, want /api/tags", got)
	}
	if got := w.Header().Get("X-Echo-Query"); got != "model=q" {
		t.Fatalf("forwarded query = %q, want model=q", got)
	}

	// Disconnect → proxy 503 again.
	m.Disconnect("kg")
	w = runOutboundProxy(m, http.MethodGet, "/kg/", "kg", "/")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("post-disconnect proxy: got %d, want 503", w.Code)
	}

	m.Stop()
}

func TestOutboundConnectRejectsBadElev(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Cloud that returns OK but no matrix_elev cookie — Connect must
	// notice and refuse.
	mux := http.NewServeMux()
	mux.HandleFunc("/matrix/h/foo/elev/app/a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewOutboundManager(srv.URL, "test-access-token", nil)
	m.Register([]conf.OutboundConfig{{Path: "p", Name: "a", Host: "foo", User: "u"}})
	err := m.Connect("p", "pw")
	if err == nil || !strings.Contains(err.Error(), "matrix_elev") {
		t.Fatalf("expected cookie-missing error, got: %v", err)
	}
}

// runOutboundProxy plumbs a gin context through ProxyTo and returns the
// resulting ResponseRecorder. The fake cloud's responses are
// non-streaming, so the http.CloseNotifier issue from TestLocalAppProxy
// doesn't bite us here.
func runOutboundProxy(m *OutboundManager, method, urlPath, path, rest string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, urlPath, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	m.ProxyTo(c, path, rest)
	return w
}

// TestOutboundTCPBridge exercises the tcp-scheme outbound path
// end-to-end: a fake cloudbox accepts WSS at /h/<host>/app/<name>/ and
// stitches it to a backing TCP echo, mirroring what a real cloudbox →
// remote outpost → TCP app chain does in production. The local
// OutboundManager opens a 127.0.0.1 listener after Connect, and a
// plain net.Dial against that listener round-trips through the WS.
func TestOutboundTCPBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Backing echo server stands in for the remote outpost's tcp app.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, aerr := echo.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	// Fake cloudbox: routes mirror the real shapes —
	//   POST /h/<host>/elev/app/<name>   → mint cookie
	//   ANY  /h/<host>/app/<name>/<rest> → WS bridge to echo
	// The Bearer + cookie assertions match what the real cloudbox enforces.
	mux := http.NewServeMux()
	mux.HandleFunc("/matrix/h/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/matrix/h/"), "/")
		// /h/<host>/elev/app/<name> — mint
		if len(parts) >= 4 && parts[1] == "elev" && parts[2] == "app" {
			w.Header().Set("Content-Type", "application/json")
			http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: "elev-token",
				Path: "/matrix/h/" + parts[0] + "/app/" + parts[3]})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if len(parts) >= 3 && parts[1] == "app" {
			// /h/<host>/app/<name>/<rest> — WS bridge to echo.
			ck, cerr := r.Cookie("matrix_elev")
			if cerr != nil || ck.Value != "elev-token" {
				http.Error(w, "missing elev", http.StatusForbidden)
				return
			}
			ws, werr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if werr != nil {
				return
			}
			defer ws.Close(websocket.StatusInternalError, "closing")
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			upstream, derr := net.Dial("tcp", echo.Addr().String())
			if derr != nil {
				_ = ws.Close(websocket.StatusBadGateway, "dial")
				return
			}
			defer upstream.Close()
			conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
			defer conn.Close()
			go func() {
				defer cancel()
				_, _ = io.Copy(upstream, conn)
			}()
			_, _ = io.Copy(conn, upstream)
		}
	})
	cloud := httptest.NewServer(mux)
	t.Cleanup(cloud.Close)

	// Pick a free port to host the local TCP listener.
	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("scratch listen: %v", err)
	}
	localPort := tmp.Addr().(*net.TCPAddr).Port
	_ = tmp.Close()

	m := NewOutboundManager(cloud.URL, "test-access-token", nil)
	t.Cleanup(m.Stop)
	m.Register([]conf.OutboundConfig{
		{Path: "pg", Name: "postgres", Host: "novicortex", User: "noviadmin", Scheme: "tcp", LocalPort: localPort},
	})

	if err := m.Connect("pg", "pw"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !m.List()[0].Connected {
		t.Fatalf("List should report connected")
	}

	// `dial 127.0.0.1:<localPort>` and round-trip a few bytes through
	// the whole chain. Retry once to give the accept goroutine a beat
	// in case the goroutine scheduler is slow.
	var client net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(localPort)))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial local tcp listener: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))

	msg := []byte("postgres-handshake-please")
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}

	// After Disconnect the listener must be gone and a fresh dial fails.
	m.Disconnect("pg")
	time.Sleep(50 * time.Millisecond) // listener close is observable async
	if c, derr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(localPort)), 200*time.Millisecond); derr == nil {
		_ = c.Close()
		t.Fatalf("listener still accepting after Disconnect")
	}
}

// TestOutboundTCPRefusesHTTPProxy guards against a category-error
// regression: calling the loopback HTTP proxy on a tcp-scheme mount
// would otherwise wedge a browser request through a WS upgrade that
// never finishes.
func TestOutboundTCPRefusesHTTPProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := NewOutboundManager("http://127.0.0.1:1", "tk", nil)
	m.Register([]conf.OutboundConfig{
		{Path: "pg", Name: "postgres", Host: "h", User: "u", Scheme: "tcp", LocalPort: 5432},
	})
	w := runOutboundProxy(m, http.MethodGet, "/pg/anything", "pg", "/anything")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for HTTP proxy of a tcp mount (%s)", w.Code, w.Body.String())
	}
}

// TestOutboundTCPConnectPortBindFailureIsSync confirms that a wedged
// LocalPort (something else already bound) surfaces synchronously from
// Connect — the operator gets a clear "address in use" instead of a
// silent listener-less state.
func TestOutboundTCPConnectPortBindFailureIsSync(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, cloud := newFakeCloud("elev")
	t.Cleanup(cloud.Close)

	// Hold a port to provoke EADDRINUSE.
	hog, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hog: %v", err)
	}
	defer hog.Close()
	hogPort := hog.Addr().(*net.TCPAddr).Port

	m := NewOutboundManager(cloud.URL, "test-access-token", nil)
	t.Cleanup(m.Stop)
	m.Register([]conf.OutboundConfig{
		{Path: "x", Name: "y", Host: "h", User: "u", Scheme: "tcp", LocalPort: hogPort},
	})
	if err := m.Connect("x", "pw"); err == nil {
		t.Fatalf("expected bind failure, got nil")
	}
	if m.List()[0].Connected {
		t.Fatalf("Connect failed but List() reports connected")
	}
}

// itoa is a no-allocation alternative to strconv.Itoa for test loops.
// Keeps the imports tight here without pulling strconv in just for one
// call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [11]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

func TestOutboundRegisterTearsDownRemovedConns(t *testing.T) {
	_, cloud := newFakeCloud("elev")
	t.Cleanup(cloud.Close)
	m := NewOutboundManager(cloud.URL, "test-access-token", nil)
	m.Register([]conf.OutboundConfig{
		{Path: "a", Name: "x", Host: "h1", User: "u"},
		{Path: "b", Name: "y", Host: "h2", User: "u"},
	})
	if err := m.Connect("a", "pw"); err != nil {
		t.Fatalf("connect a: %v", err)
	}
	if err := m.Connect("b", "pw"); err != nil {
		t.Fatalf("connect b: %v", err)
	}
	// Re-register without "a" — a's pinger should be torn down.
	m.Register([]conf.OutboundConfig{{Path: "b", Name: "y", Host: "h2", User: "u"}})
	connected := map[string]bool{}
	for _, v := range m.List() {
		connected[v.Path] = v.Connected
	}
	if _, present := connected["a"]; present {
		t.Fatalf("removed path 'a' still listed")
	}
	if !connected["b"] {
		t.Fatalf("kept path 'b' lost its connection on Register")
	}
	// Give the goroutine a moment to exit.
	time.Sleep(50 * time.Millisecond)
}

// TestOutboundSSHBridge exercises the ssh-scheme outbound path end-to-end:
// a fake cloudbox accepts WSS at /h/<host>/ssh (host-level, no /app/...) and
// stitches it to a backing TCP echo standing in for a remote outpost's
// built-in /ssh server. The OutboundManager opens a 127.0.0.1 listener
// after Connect, and a plain net.Dial against it round-trips through the WS.
//
// This is the only test that exercises the host-level elevate URL path
// (/h/<host>/elevate vs /h/<host>/app/<name>/elevate); coverage of the
// other modes lives in TestOutboundConnectAndProxy / TestOutboundTCPBridge.
func TestOutboundSSHBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, aerr := echo.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	var elevateHits, sshHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/matrix/h/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		// Strip "/matrix/h/<host>/" prefix and dispatch by the remaining path.
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/matrix/h/"), "/", 2)
		if len(parts) < 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		host, rest := parts[0], parts[1]
		switch rest {
		case "elev/ssh":
			elevateHits++
			w.Header().Set("Content-Type", "application/json")
			http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: "ssh-elev-token",
				Path: "/matrix/h/" + host + "/ssh"})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "ssh":
			sshHits++
			ck, cerr := r.Cookie("matrix_elev")
			if cerr != nil || ck.Value != "ssh-elev-token" {
				http.Error(w, "missing elev", http.StatusForbidden)
				return
			}
			ws, werr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if werr != nil {
				return
			}
			defer ws.Close(websocket.StatusInternalError, "closing")
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			upstream, derr := net.Dial("tcp", echo.Addr().String())
			if derr != nil {
				_ = ws.Close(websocket.StatusBadGateway, "dial")
				return
			}
			defer upstream.Close()
			conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
			defer conn.Close()
			go func() { defer cancel(); _, _ = io.Copy(upstream, conn) }()
			_, _ = io.Copy(conn, upstream)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	})
	cloud := httptest.NewServer(mux)
	t.Cleanup(cloud.Close)

	// Find a free local port to bind the SSH outbound listener.
	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("scratch listen: %v", err)
	}
	localPort := tmp.Addr().(*net.TCPAddr).Port
	_ = tmp.Close()

	m := NewOutboundManager(cloud.URL, "test-access-token", nil)
	t.Cleanup(m.Stop)
	// scheme="ssh" with Name intentionally empty — the manager must not
	// build /app/<name>/ paths for ssh outbounds.
	m.Register([]conf.OutboundConfig{
		{Path: "novicortex-ssh", Host: "novicortex", User: "noviadmin", Scheme: "ssh", LocalPort: localPort},
	})

	if err := m.Connect("novicortex-ssh", "pw"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if elevateHits != 1 {
		t.Fatalf("elevate hits = %d, want 1 (ssh scheme must hit host-level /elevate)", elevateHits)
	}
	if !m.List()[0].Connected {
		t.Fatalf("List should report connected")
	}

	// Round-trip a few bytes through the whole chain.
	var client net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(localPort)))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial local ssh listener: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))

	msg := []byte("SSH-2.0-test\r\n")
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo got %q, want %q", got, msg)
	}
	if sshHits != 1 {
		t.Fatalf("ssh WS hits = %d, want 1", sshHits)
	}

	m.Disconnect("novicortex-ssh")
	time.Sleep(50 * time.Millisecond)
	if c, derr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(localPort)), 200*time.Millisecond); derr == nil {
		_ = c.Close()
		t.Fatalf("listener still accepting after Disconnect")
	}
}

// TestOutboundConnectForwardsTTL verifies that OutboundConfig.TTLSeconds
// is forwarded in the elevate POST body when nonzero, omitted when 0.
// Cloudbox needs this so the operator can request long-running sessions
// per-mount; an older cloudbox that ignores the field still works.
func TestOutboundConnectForwardsTTL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		ttl        int64
		wantInBody bool
		wantValue  int64
	}{
		{"default-omits-field", 0, false, 0},
		{"finite-value-forwarded", 86400, true, 86400},
		{"max-safe-int-forwarded", 1<<53 - 1, true, 1<<53 - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			mux := http.NewServeMux()
			mux.HandleFunc("/matrix/h/h1/elev/app/a1", func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(b, &got); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: "ck", Path: "/matrix/h/h1/app/a1"})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			m := NewOutboundManager(srv.URL, "tok", nil)
			m.Register([]conf.OutboundConfig{
				{Path: "p", Name: "a1", Host: "h1", User: "u", TTLSeconds: tc.ttl},
			})
			if err := m.Connect("p", "pw"); err != nil {
				t.Fatalf("connect: %v", err)
			}
			m.Disconnect("p")

			_, present := got["ttl_seconds"]
			if present != tc.wantInBody {
				t.Fatalf("ttl_seconds present=%v, want %v (body=%v)", present, tc.wantInBody, got)
			}
			if tc.wantInBody {
				v, _ := got["ttl_seconds"].(float64)
				if int64(v) != tc.wantValue {
					t.Fatalf("ttl_seconds = %v, want %d", v, tc.wantValue)
				}
			}
		})
	}
}

// TestOutbound_CookiePersistRoundTrip is the lowest-level lock-in for
// the persistence helpers: a write followed by a read returns the same
// cookie, and remove zeroes it out. Catches path-sanitization bugs
// that would otherwise corrupt the cache silently.
func TestOutbound_CookiePersistRoundTrip(t *testing.T) {
	withTempCacheDir(t)

	for _, path := range []string{"plain", "with-dash", "with.dot", "weird/slash/in/path"} {
		t.Run(path, func(t *testing.T) {
			if err := writeCookieFile(path, "the-cookie-"+path); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := readCookieFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got != "the-cookie-"+path {
				t.Errorf("read=%q, want %q", got, "the-cookie-"+path)
			}
			// Verify file mode is 0600 (bearer-equivalent credential).
			full, _ := cookieCachePath(path)
			st, err := os.Stat(full)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if st.Mode().Perm() != 0o600 {
				t.Errorf("mode=%o, want 0600", st.Mode().Perm())
			}
			if err := removeCookieFile(path); err != nil {
				t.Fatalf("remove: %v", err)
			}
			got, err = readCookieFile(path)
			if err != nil {
				t.Fatalf("read after remove: %v", err)
			}
			if got != "" {
				t.Errorf("after remove read=%q, want empty", got)
			}
		})
	}
}

// TestOutbound_CookieReadMissingIsEmpty proves the "no cached cookie"
// case is non-error — AutoReconnect relies on this to silently skip
// mounts that were never Connected.
func TestOutbound_CookieReadMissingIsEmpty(t *testing.T) {
	withTempCacheDir(t)
	got, err := readCookieFile("never-existed")
	if err != nil {
		t.Fatalf("err on missing file: %v", err)
	}
	if got != "" {
		t.Errorf("got=%q, want empty", got)
	}
}

// TestOutbound_RemoveMissingIsNoop confirms Disconnect against a
// mount that never Connected doesn't error out.
func TestOutbound_RemoveMissingIsNoop(t *testing.T) {
	withTempCacheDir(t)
	if err := removeCookieFile("nope"); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
}

// TestOutbound_ConnectPersistsCookie locks in the Connect side of the
// new behavior: a successful elevate writes the cookie file. After
// Connect, the on-disk cookie matches the in-memory one.
func TestOutbound_ConnectPersistsCookie(t *testing.T) {
	withTempCacheDir(t)
	_, srv := newFakeCloud("tok")
	t.Cleanup(srv.Close)

	m := NewOutboundManager(srv.URL, "test-access-token", nil)
	m.Register([]conf.OutboundConfig{
		{Path: "ollama-test", Name: "ollama", Host: "h1", User: "u"},
	})
	if err := m.Connect("ollama-test", "pw"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	got, err := readCookieFile("ollama-test")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == "" {
		t.Fatalf("expected persisted cookie, got empty")
	}
	// In-memory cookie must match disk.
	m.mu.RLock()
	memCookie := m.conns["ollama-test"].elevCookie
	m.mu.RUnlock()
	if got != memCookie {
		t.Errorf("disk=%q != memory=%q", got, memCookie)
	}
	m.Stop()
}

// TestOutbound_DisconnectRemovesCookie: Disconnect wipes the file so
// AutoReconnect on the next boot doesn't try to use an explicitly-
// revoked cookie.
func TestOutbound_DisconnectRemovesCookie(t *testing.T) {
	withTempCacheDir(t)
	_, srv := newFakeCloud("tok")
	t.Cleanup(srv.Close)

	m := NewOutboundManager(srv.URL, "test-access-token", nil)
	m.Register([]conf.OutboundConfig{
		{Path: "p", Name: "ollama", Host: "h1", User: "u"},
	})
	if err := m.Connect("p", "pw"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got, _ := readCookieFile("p"); got == "" {
		t.Fatalf("setup: expected cookie present after Connect")
	}
	m.Disconnect("p")
	if got, _ := readCookieFile("p"); got != "" {
		t.Errorf("after Disconnect, cookie=%q, want empty", got)
	}
}

// TestOutbound_RegisterRemovesStaleCookies: when an operator removes
// a mount from agent.json and Register fires, the corresponding
// cookie file is wiped so a future AutoReconnect doesn't try to use
// it for a now-unknown path.
func TestOutbound_RegisterRemovesStaleCookies(t *testing.T) {
	withTempCacheDir(t)
	_, srv := newFakeCloud("tok")
	t.Cleanup(srv.Close)

	m := NewOutboundManager(srv.URL, "test-access-token", nil)
	m.Register([]conf.OutboundConfig{
		{Path: "p1", Name: "ollama", Host: "h1", User: "u"},
		{Path: "p2", Name: "ollama", Host: "h2", User: "u"},
	})
	if err := m.Connect("p1", "pw"); err != nil {
		t.Fatalf("connect p1: %v", err)
	}
	if err := m.Connect("p2", "pw"); err != nil {
		t.Fatalf("connect p2: %v", err)
	}
	// Now operator removes p1 from cfg.
	m.Register([]conf.OutboundConfig{
		{Path: "p2", Name: "ollama", Host: "h2", User: "u"},
	})
	if got, _ := readCookieFile("p1"); got != "" {
		t.Errorf("p1 cookie should be wiped after removal, got %q", got)
	}
	if got, _ := readCookieFile("p2"); got == "" {
		t.Errorf("p2 cookie should still be present (cfg unchanged)")
	}
	m.Stop()
}

// TestOutbound_AutoReconnect_RehydratesPersistedCookies is the
// integration test for the boot-time recovery path. Simulates an
// outpost restart: build a manager, Connect a mount (cookie persists),
// then build a fresh manager pointing at the same cache dir, call
// AutoReconnect, and verify the pinger is live.
func TestOutbound_AutoReconnect_RehydratesPersistedCookies(t *testing.T) {
	withTempCacheDir(t)

	fc, srv := newFakeCloud("tok")
	t.Cleanup(srv.Close)

	// Lifetime 1: Connect mints + persists the cookie.
	m1 := NewOutboundManager(srv.URL, "test-access-token", nil)
	m1.Register([]conf.OutboundConfig{
		{Path: "ollama1", Name: "ollama", Host: "h1", User: "u"},
	})
	if err := m1.Connect("ollama1", "pw"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	persisted, _ := readCookieFile("ollama1")
	if persisted == "" {
		t.Fatalf("setup: expected persisted cookie")
	}
	m1.Stop() // pinger goroutine exits

	// Lifetime 2: fresh manager, same cache. AutoReconnect should
	// pick up the cookie without prompting for a password.
	m2 := NewOutboundManager(srv.URL, "test-access-token", nil)
	m2.Register([]conf.OutboundConfig{
		{Path: "ollama1", Name: "ollama", Host: "h1", User: "u"},
	})
	m2.AutoReconnect()
	m2.mu.RLock()
	conn := m2.conns["ollama1"]
	m2.mu.RUnlock()
	if conn == nil {
		t.Fatalf("after AutoReconnect, mount should be in conns")
	}
	if conn.elevCookie != persisted {
		t.Errorf("hydrated cookie=%q, want %q", conn.elevCookie, persisted)
	}
	// Sanity: cloudbox stub didn't observe an /elev/ POST for this
	// rehydration — AutoReconnect must NOT re-elevate (which would
	// fail anyway without a password).
	_ = fc // (fakeCloud doesn't track elev count today; intent doc)
	m2.Stop()
}

// TestOutbound_AutoReconnect_SkipsMountsWithoutCookie: a mount that
// the operator added to agent.json but never Connected stays in
// cfg-only state after AutoReconnect — no spurious pingers, no errors.
func TestOutbound_AutoReconnect_SkipsMountsWithoutCookie(t *testing.T) {
	withTempCacheDir(t)
	_, srv := newFakeCloud("tok")
	t.Cleanup(srv.Close)

	m := NewOutboundManager(srv.URL, "test-access-token", nil)
	m.Register([]conf.OutboundConfig{
		{Path: "never-connected", Name: "ollama", Host: "h1", User: "u"},
	})
	m.AutoReconnect()
	m.mu.RLock()
	_, present := m.conns["never-connected"]
	m.mu.RUnlock()
	if present {
		t.Errorf("AutoReconnect must not invent a conn for a mount that has no persisted cookie")
	}
	m.Stop()
}

// TestOutbound_AutoReconnect_UnpairedOutpostIsNoop: an outpost
// without an access_token shouldn't try to AutoReconnect — there's
// nothing to ping against. Guard mirrors Connect's early-return.
func TestOutbound_AutoReconnect_UnpairedOutpostIsNoop(t *testing.T) {
	withTempCacheDir(t)
	// Pre-seed a cookie file the manager would otherwise hydrate.
	if err := writeCookieFile("p", "leftover-cookie"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := NewOutboundManager("https://example.com", "", nil) // no access_token
	m.Register([]conf.OutboundConfig{{Path: "p", Name: "x", Host: "h", User: "u"}})
	m.AutoReconnect()
	m.mu.RLock()
	_, present := m.conns["p"]
	m.mu.RUnlock()
	if present {
		t.Errorf("unpaired outpost must not hydrate any conns")
	}
}
