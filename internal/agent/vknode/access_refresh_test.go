package vknode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchAccess_SuccessfulDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AccessEndpointPath || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "wrong", http.StatusBadRequest)
			return
		}
		if got := r.URL.Query().Get("node_name"); got != "home-mini" {
			t.Errorf("node_name: got %q want home-mini", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer my-token" {
			t.Errorf("Authorization: got %q want %q", got, "Bearer my-token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"node_name": "home-mini",
			"owner_namespace": "user-aaaaaa",
			"allowed_namespaces": ["user-aaaaaa", "user-bbbbbb"]
		}`)
	}))
	defer srv.Close()

	got, err := FetchAccess(context.Background(), srv.URL, "my-token", "home-mini")
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeName != "home-mini" || got.OwnerNamespace != "user-aaaaaa" {
		t.Errorf("decoded: %+v", got)
	}
	if !reflect.DeepEqual(got.AllowedNamespaces, []string{"user-aaaaaa", "user-bbbbbb"}) {
		t.Errorf("AllowedNamespaces: %v", got.AllowedNamespaces)
	}
}

func TestFetchAccess_Returns403AsFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"no host with that name registered to your account"}`)
	}))
	defer srv.Close()

	_, err := FetchAccess(context.Background(), srv.URL, "my-token", "demo")
	if err == nil {
		t.Fatal("expected error")
	}
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Status != http.StatusForbidden {
		t.Errorf("want FetchError 403; got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "no host with that name") {
		t.Errorf("error message lost: %v", err)
	}
}

func TestFetchAccess_Returns503AsClusterDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"cluster mode is not enabled"}`)
	}))
	defer srv.Close()

	_, err := FetchAccess(context.Background(), srv.URL, "my-token", "demo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsClusterDisabled(err) {
		t.Errorf("IsClusterDisabled = false; err = %v", err)
	}
}

func TestFetchAccess_RejectsEmptyInputs(t *testing.T) {
	for _, tc := range []struct{ base, tok, name string }{
		{"", "tok", "n"},
		{"  ", "tok", "n"},
		{"http://x", "", "n"},
		{"http://x", "tok", ""},
	} {
		if _, err := FetchAccess(context.Background(), tc.base, tc.tok, tc.name); err == nil {
			t.Errorf("expected error for empty input (base=%q tok=%q name=%q)", tc.base, tc.tok, tc.name)
		}
	}
}

// TestAccessRefresher_AppliesFetchedSetToAccess verifies the single
// happy-path iteration: one cloudbox call returns three namespaces, the
// refresher reflects them into the live Access gate before the next
// sleep. We tear it down via context cancel as soon as we observe the
// effect so the test doesn't burn 60s on accessRefreshInterval.
func TestAccessRefresher_AppliesFetchedSetToAccess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"node_name": "demo",
			"owner_namespace": "user-alice0",
			"allowed_namespaces": ["user-alice0", "user-bob000", "user-carol0"]
		}`)
	}))
	defer srv.Close()

	access := NewAccess("user-alice0") // pre-seed with just the owner
	deps := AccessRefreshDeps{
		CloudboxBase: srv.URL,
		AccessToken:  "tok",
		NodeName:     "demo",
		Access:       access,
	}
	r := NewAccessRefresher(deps)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Wait for the first fetch + Set to land. We poll the access set
	// rather than sleeping a fixed delay so the test runs fast on a
	// healthy machine and doesn't go flaky on a slow CI.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if has := contains(access.Snapshot(), "user-bob000"); has {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("access.Snapshot() never picked up bob: got %v", access.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := access.Snapshot()
	sort.Strings(got)
	want := []string{"user-alice0", "user-bob000", "user-carol0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Snapshot after refresh: got %v want %v", got, want)
	}
	if hits.Load() < 1 {
		t.Errorf("expected at least 1 fetch, got %d", hits.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of ctx cancel")
	}
}

// TestAccessRefresher_KeepsExistingSetOnError verifies that a fetch
// failure does NOT zero the allow-set. If cloudbox is briefly down, the
// previously-applied namespaces must keep working — otherwise a
// transient blip would reject every sharee's pod.
func TestAccessRefresher_KeepsExistingSetOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	access := NewAccess("user-alice0", "user-bob000")
	deps := AccessRefreshDeps{
		CloudboxBase: srv.URL,
		AccessToken:  "tok",
		NodeName:     "demo",
		Access:       access,
	}
	r := NewAccessRefresher(deps)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Let the first failing fetch run.
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	got := access.Snapshot()
	sort.Strings(got)
	want := []string{"user-alice0", "user-bob000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Snapshot after failed refresh: got %v want %v (must not zero on error)", got, want)
	}
}

// TestAccessRefresher_StopsOnContextCancel: Run must exit within a tight
// budget when ctx is canceled — the errgroup tearing down at process
// shutdown shouldn't have to wait a full refresh interval.
func TestAccessRefresher_StopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"node_name":"x","owner_namespace":"user-z","allowed_namespaces":["user-z"]}`)
	}))
	defer srv.Close()

	access := NewAccess()
	r := NewAccessRefresher(AccessRefreshDeps{
		CloudboxBase: srv.URL, AccessToken: "t", NodeName: "x", Access: access,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Let the first iteration complete so we're sleeping in the select.
	time.Sleep(100 * time.Millisecond)

	t0 := time.Now()
	cancel()
	select {
	case <-done:
		elapsed := time.Since(t0)
		if elapsed > 500*time.Millisecond {
			t.Errorf("Run took %v to exit after cancel (want < 500ms)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of ctx cancel")
	}
}

// TestNamespaceDiff covers the three diff shapes that surface in logs.
func TestNamespaceDiff(t *testing.T) {
	cases := []struct {
		name           string
		before, after  []string
		wantSubstrings []string // anything in here must appear in the diff
		wantEmpty      bool     // when both unchanged
	}{
		{name: "unchanged", before: []string{"a", "b"}, after: []string{"b", "a"}, wantEmpty: true},
		{name: "added", before: []string{"a"}, after: []string{"a", "b"}, wantSubstrings: []string{"+b"}},
		{name: "removed", before: []string{"a", "b"}, after: []string{"a"}, wantSubstrings: []string{"-b"}},
		{name: "both", before: []string{"a", "b"}, after: []string{"a", "c"}, wantSubstrings: []string{"+c", "-b"}},
	}
	for _, c := range cases {
		got := namespaceDiff(c.before, c.after)
		if c.wantEmpty {
			if got != "" {
				t.Errorf("%s: want empty, got %q", c.name, got)
			}
			continue
		}
		for _, s := range c.wantSubstrings {
			if !strings.Contains(got, s) {
				t.Errorf("%s: diff %q missing substring %q", c.name, got, s)
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
