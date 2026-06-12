package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPuller_FetchTarget_OK: a 200 target is decoded into an Envelope,
// and the request carries the bearer + the platform selector.
func TestPuller_FetchTarget_OK(t *testing.T) {
	var gotAuth, gotPlatform, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPlatform = r.URL.Query().Get("platform")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"release_id":"v9.9.9","url":"https://example.com/outpost-v9.9.9-linux-amd64","sha256":"deadbeef","commit":"abc1234"}`))
	}))
	defer srv.Close()

	p := PullerConfig{CloudboxBase: srv.URL, AccessToken: "tok-123", Platform: "linux_amd64"}
	env, ok, err := p.fetchTarget(context.Background())
	if err != nil {
		t.Fatalf("fetchTarget: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true on 200")
	}
	if env.ReleaseID != "v9.9.9" || env.Commit != "abc1234" || env.SHA256 != "deadbeef" ||
		env.URL != "https://example.com/outpost-v9.9.9-linux-amd64" {
		t.Errorf("env = %+v", env)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("auth = %q, want Bearer tok-123", gotAuth)
	}
	if gotPlatform != "linux_amd64" {
		t.Errorf("platform = %q, want linux_amd64", gotPlatform)
	}
	if gotPath != "/api/v1/fleet/target" {
		t.Errorf("path = %q, want /api/v1/fleet/target", gotPath)
	}
}

// TestPuller_FetchTarget_NoContent: 204 means "nothing to do" — not an
// error, and ok=false so checkOnce skips the Worker.
func TestPuller_FetchTarget_NoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	p := PullerConfig{CloudboxBase: srv.URL, AccessToken: "t", Platform: "linux_amd64"}
	env, ok, err := p.fetchTarget(context.Background())
	if err != nil {
		t.Fatalf("fetchTarget: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false on 204; env=%+v", env)
	}
}

// TestPuller_FetchTarget_ServerError: a non-200/204 surfaces as an error
// (logged, not fatal, by checkOnce) rather than a bogus envelope.
func TestPuller_FetchTarget_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	p := PullerConfig{CloudboxBase: srv.URL, AccessToken: "t", Platform: "linux_amd64"}
	if _, ok, err := p.fetchTarget(context.Background()); err == nil || ok {
		t.Fatalf("got (ok=%v, err=%v), want (false, non-nil) on 500", ok, err)
	}
}

// TestPuller_RunUnconfigured: an unpaired puller (no worker/base/token)
// blocks on ctx and returns nil on cancel — it must never tear down the
// errgroup it runs under.
func TestPuller_RunUnconfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- PullerConfig{Platform: "linux_amd64"}.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
