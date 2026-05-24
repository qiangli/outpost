package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// TestKeepAlivePingsAndUpdatesCookie stands up a fake cloudbox that
// answers /h/<host>/elev/ssh/ping with a new Set-Cookie each time, runs
// runKeepAlive with a tight interval, and confirms the cookie file is
// rewritten after each ping. Exits cleanly on context cancellation.
func TestKeepAlivePingsAndUpdatesCookie(t *testing.T) {
	// Speed up the test by overriding the package-level interval. Reset
	// before returning so other tests aren't affected.
	saved := keepAliveInterval
	keepAliveInterval = 30 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = saved })

	// Redirect XDG_CACHE_HOME so writeCookie lands in t.TempDir() and we
	// don't pollute the developer's real ~/.cache/outpost/sessions/.
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	if err := os.Setenv("HOME", tmp); err != nil {
		t.Fatalf("set HOME: %v", err)
	}

	var pings atomic.Int32
	host := "testhost"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/elev/ssh/ping") {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-bearer" {
			http.Error(w, "wrong auth", http.StatusUnauthorized)
			return
		}
		// Verify the cookie came through (anything matrix_elev=…).
		ck, err := r.Cookie("matrix_elev")
		if err != nil || ck.Value == "" {
			http.Error(w, "missing cookie", http.StatusUnauthorized)
			return
		}
		n := pings.Add(1)
		// Slide the cookie with a new value each ping, scoped to the
		// data URL like cloudbox does in production.
		http.SetCookie(w, &http.Cookie{
			Name:  "matrix_elev",
			Value: "refreshed-" + strings.Repeat("v", int(n)),
			Path:  "/h/" + host + "/ssh",
		})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	port := 0
	if p := u.Port(); p != "" {
		// httptest always assigns a port; parse it.
		if n, err := parsePort(p); err == nil {
			port = n
		}
	}
	fc := &conf.FileConfig{
		ServerAddr: "http://" + u.Hostname(),
		ServerPort: port,
		Protocol:   "tcp",
	}

	if err := writeCookie(host, "initial-cookie"); err != nil {
		t.Fatalf("seed cookie: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runKeepAlive(ctx, fc, "test-bearer", host, "initial-cookie")
	}()

	// Wait until at least 2 pings have landed, then cancel.
	deadline := time.After(2 * time.Second)
	for {
		if pings.Load() >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d pings within 2s", pings.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runKeepAlive returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("runKeepAlive did not exit within 1s of ctx cancel")
	}

	// Cookie file should reflect the most recent slide.
	got, err := readCookie(host)
	if err != nil {
		t.Fatalf("read cookie: %v", err)
	}
	if !strings.HasPrefix(got, "refreshed-") {
		t.Errorf("cookie not rewritten — got %q, want refreshed-…", got)
	}
}

// TestKeepAliveExitsOn401 verifies the ping returning 401/403 (absolute
// cap reached) propagates as an error from runKeepAlive so a supervisor
// script can detect the failure.
func TestKeepAliveExitsOn401(t *testing.T) {
	saved := keepAliveInterval
	keepAliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = saved })

	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	os.Setenv("HOME", tmp)

	host := "expirhost"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "elevation expired", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	port, _ := parsePort(u.Port())
	fc := &conf.FileConfig{
		ServerAddr: "http://" + u.Hostname(),
		ServerPort: port,
		Protocol:   "tcp",
	}
	_ = writeCookie(host, "doesnt-matter")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runKeepAlive(ctx, fc, "test-bearer", host, "doesnt-matter")
	if err == nil {
		t.Fatal("expected runKeepAlive to error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected error to mention 401, got %v", err)
	}
}

// TestPostElevate_TTLInPayload verifies the --ttl plumbing all the
// way from the CLI flag down to the elevate POST body. Three cases:
//
//	default → no ttl_seconds field at all (server gets cloudbox default)
//	24h     → ttl_seconds=86400
//	infinite → ttl_seconds=ttlInfiniteSeconds
//
// Body presence/absence is the contract: cloudbox's Elevate handler
// reads an absent field as "use my default", which is how older
// cloudboxes (no MaxLifetimeSeconds awareness) keep working.
func TestPostElevate_TTLInPayload(t *testing.T) {
	cases := []struct {
		name      string
		ttlInput  string
		wantField bool
		wantValue int64
	}{
		{"default-flag-empty", "", false, 0},
		{"default-keyword", "default", false, 0},
		{"24h", "24h", true, 24 * 3600},
		{"infinite", "infinite", true, ttlInfiniteSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPayload map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := decodeJSON(r, &gotPayload); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: "ok"})
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)

			u, _ := url.Parse(srv.URL)
			port := 0
			if p := u.Port(); p != "" {
				if n, err := parsePort(p); err == nil {
					port = n
				}
			}
			fc := &conf.FileConfig{
				ServerAddr: "http://" + u.Hostname(),
				ServerPort: port,
				Protocol:   "tcp",
			}

			ttl, err := parseTTL(tc.ttlInput)
			if err != nil {
				t.Fatalf("parseTTL(%q): %v", tc.ttlInput, err)
			}
			cookie, err := postElevate(context.Background(), fc, "bearer", "host1", "noviadmin", "pw", ttl)
			if err != nil {
				t.Fatalf("postElevate: %v", err)
			}
			if cookie != "ok" {
				t.Errorf("cookie=%q, want ok", cookie)
			}
			if tc.wantField {
				v, ok := gotPayload["ttl_seconds"]
				if !ok {
					t.Fatalf("ttl_seconds missing from payload: %v", gotPayload)
				}
				// JSON numbers come back as float64.
				got := int64(v.(float64))
				if got != tc.wantValue {
					t.Errorf("ttl_seconds=%d, want %d", got, tc.wantValue)
				}
			} else if _, ok := gotPayload["ttl_seconds"]; ok {
				t.Errorf("ttl_seconds present in payload but should be omitted: %v", gotPayload)
			}
		})
	}
}

func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(out)
}

func parsePort(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errParsePort
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

var errParsePort = stringErr("invalid port")

type stringErr string

func (e stringErr) Error() string { return string(e) }
