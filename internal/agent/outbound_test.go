package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		// /h/<host>/elevate, /h/<host>/elevate-ping, or /h/<host>/app/<name>/*
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/h/"), "/")
		if len(parts) < 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		switch parts[1] {
		case "elevate":
			http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: fc.elevToken, Path: "/h/" + parts[0]})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "elevate-ping":
			fc.pingCount++
			if !fc.pingAlwaysOK && fc.pingCount > fc.pingFailAfter {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "app":
			// Validate elev cookie was forwarded.
			ck, err := r.Cookie("matrix_elev")
			if err != nil || ck.Value != fc.elevToken {
				http.Error(w, "missing elev", http.StatusForbidden)
				return
			}
			// Echo the remainder so the test can inspect path forwarding.
			rest := "/" + strings.Join(parts[3:], "/")
			w.Header().Set("X-Echo-Path", rest)
			if r.URL.RawQuery != "" {
				w.Header().Set("X-Echo-Query", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		default:
			http.Error(w, "unknown sub", http.StatusNotFound)
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
	mux.HandleFunc("/h/foo/elevate", func(w http.ResponseWriter, r *http.Request) {
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
