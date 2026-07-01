// Copyright (c) 2026, the outpost authors
// See LICENSE for licensing information

package peerhosts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistry_NilSafe(t *testing.T) {
	var r *Registry
	if r.IsPeer(context.Background(), "anything") {
		t.Fatalf("nil registry must answer false")
	}
}

func TestRegistry_EmptyTokenIsNoOp(t *testing.T) {
	r := New(Config{ServerAddr: "http://example.invalid", Token: ""})
	if r.IsPeer(context.Background(), "host-c") {
		t.Fatalf("empty-token registry must answer false (loopback fallback covers unpaired outposts)")
	}
}

func TestRegistry_PopulatesFromServer(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/api/v1/ssh/hosts" {
			t.Errorf("path=%q, want /api/v1/ssh/hosts", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer T0KEN" {
			t.Errorf("auth header=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hosts": []map[string]string{
				{"host": "host-c"},
				{"host": "Host-b"}, // mixed-case → lowered
				{"host": ""},       // skipped
			},
		})
	}))
	defer srv.Close()
	r := New(serverConfig(t, srv.URL, "T0KEN"))

	ctx := context.Background()
	for _, want := range []string{"host-c", "host-b", "HOST-C"} {
		if !r.IsPeer(ctx, want) {
			t.Errorf("IsPeer(%q) = false, want true", want)
		}
	}
	if r.IsPeer(ctx, "not-paired") {
		t.Errorf("IsPeer for unknown host should be false")
	}
	if calls.Load() == 0 {
		t.Fatalf("server never queried")
	}
}

func TestRegistry_CachesWithinTTL(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"hosts":[{"host":"a"}]}`))
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, "tk")
	cfg.TTL = 1 * time.Hour
	r := New(cfg)
	ctx := context.Background()
	for range 5 {
		_ = r.IsPeer(ctx, "a")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1 (TTL should cache)", calls.Load())
	}
}

func TestRegistry_ServesStaleOnFailure(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"hosts":[{"host":"a"}]}`))
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, "tk")
	cfg.TTL = 1 * time.Nanosecond // force refresh on every call
	r := New(cfg)
	ctx := context.Background()

	if !r.IsPeer(ctx, "a") {
		t.Fatalf("first IsPeer false; want true")
	}
	fail.Store(true)
	if !r.IsPeer(ctx, "a") {
		t.Fatalf("IsPeer should return stale=true when refresh fails")
	}
}

func serverConfig(t *testing.T, srvURL, token string) Config {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	port := 0
	if u.Port() != "" {
		// Parse port from URL; net/url returns string.
		var n int
		for _, c := range u.Port() {
			n = n*10 + int(c-'0')
		}
		port = n
	}
	return Config{
		ServerAddr: strings.SplitN(u.Host, ":", 2)[0],
		ServerPort: port,
		Token:      token,
	}
}
