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

// TestCookieOnlyKeepAlive_NoCookieErrors is the guard for the
// daemonize-friendly path: if there's no cached cookie for the host,
// --cookie-only must refuse explicitly (rather than hanging or
// silently starting a useless loop). The error message must guide
// the operator to seed via interactive connect.
func TestCookieOnlyKeepAlive_NoCookieErrors(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)

	// Seed a minimal config at whatever path conf.DefaultConfigPath
	// resolves to under the redirected HOME (macOS:
	// Library/Application Support/matrix; Linux: .config/matrix).
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		t.Fatalf("default config path: %v", err)
	}
	if err := os.MkdirAll(strings.TrimSuffix(cfgPath, "/agent.json"), 0o700); err != nil {
		t.Fatalf("mkdir cfg: %v", err)
	}
	if err := os.WriteFile(cfgPath,
		[]byte(`{"agent_name":"x","server_addr":"ai.dhnt.io","server_port":443,"protocol":"wss","access_token":"tok"}`),
		0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rerr := runCookieOnlyKeepAlive(ctx, "unseeded-host")
	if rerr == nil {
		t.Fatal("expected error when no cached cookie exists")
	}
	if !strings.Contains(rerr.Error(), "no cached cookie") {
		t.Errorf("error should mention 'no cached cookie', got: %v", rerr)
	}
	if !strings.Contains(rerr.Error(), "outpost connect") {
		t.Errorf("error should mention the seeding command, got: %v", rerr)
	}
}

// TestRetryBackoff_DoublesUntilCap locks in the wait sequence so a
// silent regression to a tighter or looser backoff is caught — the
// values here are what the supervisor depends on.
func TestRetryBackoff_DoublesUntilCap(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 5 * time.Minute},   // capped (would be 480s)
		{10, 5 * time.Minute},  // still capped
		{100, 5 * time.Minute}, // overflow-safe
		{0, 30 * time.Second},  // clamps n<1 to 1
	}
	for _, tc := range cases {
		got := retryBackoff(tc.n)
		if got != tc.want {
			t.Errorf("retryBackoff(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

// TestKeepAlive_RetriesTransientThenSucceeds drives the retry path:
// the first 3 pings return 503 (transient), the 4th returns 200.
// runKeepAlive should not exit; it should retry and reset the
// failure counter on success.
func TestKeepAlive_RetriesTransientThenSucceeds(t *testing.T) {
	saved := keepAliveInterval
	keepAliveInterval = 10 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = saved })
	// Override backoff to fast values for the test — production
	// backoff (30s) would make the test glacial.
	savedCap := keepAliveBackoffCap
	keepAliveBackoffCap = 5 * time.Millisecond
	t.Cleanup(func() { keepAliveBackoffCap = savedCap })

	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	host := "flakyhost"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= 3 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	port, _ := parsePort(u.Port())
	fc := &conf.FileConfig{
		ServerAddr: "http://" + u.Hostname(),
		ServerPort: port,
		Protocol:   "tcp",
	}
	_ = writeCookie(host, "ck")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runKeepAlive(ctx, fc, "tk", host, "ck") }()

	// Wait until we see at least 4 successful pings worth of activity
	// (3 503s + at least one 200), then cancel and verify clean exit.
	deadline := time.After(2 * time.Second)
	for {
		if hits.Load() >= 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d hits within 2s", hits.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runKeepAlive returned error after recovery: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("runKeepAlive did not exit within 1s of ctx cancel")
	}
}

// TestKeepAlive_GivesUpAfterMaxConsecutive verifies the bounded
// retry budget — after N consecutive transient errors the loop
// returns a non-nil error so the supervisor can act.
func TestKeepAlive_GivesUpAfterMaxConsecutive(t *testing.T) {
	saved := keepAliveInterval
	keepAliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = saved })
	savedCap := keepAliveBackoffCap
	keepAliveBackoffCap = 1 * time.Millisecond
	t.Cleanup(func() { keepAliveBackoffCap = savedCap })
	savedMax := keepAliveMaxConsecutiveFailures
	keepAliveMaxConsecutiveFailures = 3
	t.Cleanup(func() { keepAliveMaxConsecutiveFailures = savedMax })

	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	host := "deadhost"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "always down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	port, _ := parsePort(u.Port())
	fc := &conf.FileConfig{
		ServerAddr: "http://" + u.Hostname(),
		ServerPort: port,
		Protocol:   "tcp",
	}
	_ = writeCookie(host, "ck")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runKeepAlive(ctx, fc, "tk", host, "ck")
	if err == nil {
		t.Fatal("expected non-nil error after max consecutive failures")
	}
	if !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error should mention 'gave up', got: %v", err)
	}
}

// TestKeepAlive_FatalExitsImmediately: a 403 must NOT retry — the
// cookie is dead and the supervisor needs to know so the operator
// can re-elevate.
func TestKeepAlive_FatalExitsImmediately(t *testing.T) {
	saved := keepAliveInterval
	keepAliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = saved })

	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	host := "revokedhost"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "elevation revoked", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	port, _ := parsePort(u.Port())
	fc := &conf.FileConfig{
		ServerAddr: "http://" + u.Hostname(),
		ServerPort: port,
		Protocol:   "tcp",
	}
	_ = writeCookie(host, "ck")

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := runKeepAlive(ctx, fc, "tk", host, "ck")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "fatal") {
		t.Errorf("error should be marked 'fatal', got: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("403 must NOT be retried; got %d hits, want 1", hits.Load())
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
