package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

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
	mux.HandleFunc("/h/", func(w http.ResponseWriter, r *http.Request) {
		// Cloudbox routes after the per-(host, app) elevation refactor:
		//   /h/<host>/app/<name>/elevate
		//   /h/<host>/app/<name>/elevate-ping
		//   /h/<host>/app/<name>/<rest>     (proxied to the upstream app)
		// parts[0]=<host>, parts[1]="app", parts[2]=<name>, parts[3]=action.
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/h/"), "/")
		if len(parts) < 4 || parts[1] != "app" {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		host, appName, action := parts[0], parts[2], parts[3]
		// Per-(host, app) cookie scope: Path narrows to the one app so
		// elevation cannot be replayed against a sibling app on the
		// same host. Tests then assert via the action handler that the
		// cookie actually came through.
		cookiePath := "/h/" + host + "/app/" + appName
		switch action {
		case "elevate":
			http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: fc.elevToken, Path: cookiePath})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "elevate-ping":
			fc.pingCount++
			if !fc.pingAlwaysOK && fc.pingCount > fc.pingFailAfter {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			// Anything else under /h/<host>/app/<name>/* is treated as
			// a proxy hit to the upstream app. Validate the elevation
			// cookie was forwarded then echo back the residual path.
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
		}
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
	mux.HandleFunc("/h/foo/app/a/elevate", func(w http.ResponseWriter, r *http.Request) {
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

	// Fake cloudbox: routes mirror the per-(host, app) shape —
	//   /h/<host>/app/<name>/elevate           → mint cookie
	//   /h/<host>/app/<name>/<rest>            → WS bridge to echo
	// The Bearer + cookie assertions match what the real cloudbox enforces.
	mux := http.NewServeMux()
	mux.HandleFunc("/h/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/h/"), "/")
		if len(parts) < 4 || parts[1] != "app" {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		action := parts[3]
		switch action {
		case "elevate":
			http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: "elev-token",
				Path: "/h/" + parts[0] + "/app/" + parts[2]})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			// Treat any other path under /app/<name>/* as a proxy hit;
			// the WS upgrade gets bridged to the echo TCP server.
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
	mux.HandleFunc("/h/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		// Strip "/h/<host>/" prefix and dispatch by the remaining path.
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/h/"), "/", 2)
		if len(parts) < 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		host, rest := parts[0], parts[1]
		switch rest {
		case "ssh/elevate":
			elevateHits++
			http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: "ssh-elev-token",
				Path: "/h/" + host + "/ssh"})
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
