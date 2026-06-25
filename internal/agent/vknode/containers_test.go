package vknode

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startFakeLibpod stands up a unix-socket HTTP server impersonating
// libpod. handler decides what to return; the returned cleanup closes
// the listener and unlinks the socket file.
//
// We deliberately use /tmp (not t.TempDir()) because Darwin's
// sockaddr_un.sun_path is capped at 104 bytes — t.TempDir() with the
// long /var/folders/... prefix plus a test name like
// TestClient_ListContainers_LabelFilter blows past the limit and the
// bind fails with EINVAL.
func startFakeLibpod(t *testing.T, handler http.HandlerFunc) (sockPath string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vk")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	})
	return sock
}

func TestClient_Ping(t *testing.T) {
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5.0.0/libpod/_ping" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "OK\n")
	})
	c, err := NewClient(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestClient_Ping_ErrorStatus(t *testing.T) {
	sock := startFakeLibpod(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"cause":"x","message":"daemon broken","response":500}`)
	})
	c, _ := NewClient(sock)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Status != 500 || ae.Message != "daemon broken" {
		t.Fatalf("unexpected APIError: %+v", ae)
	}
}

func TestNewClient_EmptySocket(t *testing.T) {
	if _, err := NewClient(""); err == nil {
		t.Fatal("expected error on empty socket")
	}
	if _, err := NewClient("   "); err == nil {
		t.Fatal("expected error on whitespace socket")
	}
}

func TestClient_CreateContainer(t *testing.T) {
	var gotBody SpecGenerator
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5.0.0/libpod/containers/create" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "wrong", http.StatusBadRequest)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("missing JSON content-type: %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"Id":"abc123","Warnings":["minor"]}`)
	})
	c, _ := NewClient(sock)

	spec := &SpecGenerator{
		Name:    "hello",
		Image:   "docker.io/library/alpine:3.20",
		Command: []string{"echo", "hi"},
		Labels:  map[string]string{ManagedLabel: "true"},
	}
	resp, err := c.CreateContainer(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.ID != "abc123" {
		t.Errorf("ID: got %q want abc123", resp.ID)
	}
	if len(resp.Warnings) != 1 || resp.Warnings[0] != "minor" {
		t.Errorf("Warnings: %+v", resp.Warnings)
	}
	if gotBody.Name != "hello" || gotBody.Image != spec.Image {
		t.Errorf("body round-trip wrong: %+v", gotBody)
	}
	if gotBody.Labels[ManagedLabel] != "true" {
		t.Errorf("ManagedLabel not transmitted: %+v", gotBody.Labels)
	}
}

func TestClient_StartContainer(t *testing.T) {
	hits := map[string]int{}
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/v5.0.0/libpod/containers/abc/start":
			w.WriteHeader(http.StatusNoContent)
		case "/v5.0.0/libpod/containers/already/start":
			// Already running — libpod returns 304.
			w.WriteHeader(http.StatusNotModified)
		case "/v5.0.0/libpod/containers/bad/start":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"broken"}`)
		}
	})
	c, _ := NewClient(sock)
	if err := c.StartContainer(context.Background(), "abc"); err != nil {
		t.Errorf("plain start: %v", err)
	}
	if err := c.StartContainer(context.Background(), "already"); err != nil {
		t.Errorf("idempotent start (304): %v", err)
	}
	err := c.StartContainer(context.Background(), "bad")
	if err == nil {
		t.Error("expected error from 500")
	}
	if hits["/v5.0.0/libpod/containers/abc/start"] != 1 {
		t.Errorf("abc hit count: %d", hits["/v5.0.0/libpod/containers/abc/start"])
	}
}

func TestClient_StopContainer_EncodesTimeout(t *testing.T) {
	var gotQuery string
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})
	c, _ := NewClient(sock)
	if err := c.StopContainer(context.Background(), "abc", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "timeout=30" {
		t.Errorf("query: got %q want timeout=30", gotQuery)
	}
}

func TestClient_RemoveContainer_QueryFlags(t *testing.T) {
	var gotQuery string
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})
	c, _ := NewClient(sock)
	if err := c.RemoveContainer(context.Background(), "abc", true, true); err != nil {
		t.Fatal(err)
	}
	// Query encoding order is alphabetical via url.Values.
	if gotQuery != "force=true&v=true" {
		t.Errorf("query: got %q want force=true&v=true", gotQuery)
	}
}

func TestClient_RemoveContainer_NotFound(t *testing.T) {
	sock := startFakeLibpod(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"no such container"}`)
	})
	c, _ := NewClient(sock)
	err := c.RemoveContainer(context.Background(), "ghost", false, false)
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound: got false; err=%v", err)
	}
}

func TestClient_InspectContainer(t *testing.T) {
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5.0.0/libpod/containers/abc/json" {
			http.Error(w, "wrong", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"Id":"abc",
			"Name":"hello",
			"State":{"Status":"running","Running":true,"Pid":1234,"ExitCode":0,"StartedAt":"2026-01-01T00:00:00Z"},
			"Config":{"Labels":{"outpost.io/managed":"true"},"Env":["FOO=bar"]},
			"Image":"sha256:deadbeef",
			"ImageName":"alpine:3.20"
		}`)
	})
	c, _ := NewClient(sock)
	got, err := c.InspectContainer(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "abc" || got.Name != "hello" {
		t.Errorf("identity: %+v", got)
	}
	if !got.State.Running || got.State.Pid != 1234 {
		t.Errorf("state: %+v", got.State)
	}
	if got.Config.Labels[ManagedLabel] != "true" {
		t.Errorf("labels not parsed: %+v", got.Config.Labels)
	}
	if got.ImageName != "alpine:3.20" {
		t.Errorf("imageName: %q", got.ImageName)
	}
}

func TestClient_ListContainers_LabelFilter(t *testing.T) {
	var gotFilters string
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		gotFilters = r.URL.Query().Get("filters")
		if r.URL.Query().Get("all") != "true" {
			t.Errorf("all flag missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[
			{"Id":"a","Names":["/foo"],"Image":"alpine","State":"running","Status":"Up 5s","Labels":{"outpost.io/managed":"true"}}
		]`)
	})
	c, _ := NewClient(sock)
	items, err := c.ListContainers(context.Background(), true, map[string]string{
		ManagedLabel: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "a" {
		t.Errorf("items: %+v", items)
	}
	// Verify filter encoding shape. The exact JSON depends on map ordering,
	// so just check for the key+value substring.
	if !strings.Contains(gotFilters, `"outpost.io/managed=true"`) {
		t.Errorf("filters payload missing managed=true: %q", gotFilters)
	}
	if !strings.HasPrefix(gotFilters, `{"label":`) {
		t.Errorf("filters payload shape wrong: %q", gotFilters)
	}
}

func TestClient_PullImage_StreamSuccess(t *testing.T) {
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reference") != "alpine:3.20" {
			t.Errorf("ref: %q", r.URL.Query().Get("reference"))
		}
		w.Header().Set("Content-Type", "application/json")
		// Libpod streams JSON lines; an empty stream means "done, no error".
		_, _ = io.WriteString(w, `{"stream":"Pulling..."}`+"\n")
		_, _ = io.WriteString(w, `{"images":["alpine:3.20"]}`+"\n")
	})
	c, _ := NewClient(sock)
	if err := c.PullImage(context.Background(), "alpine:3.20"); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func TestClient_PullImage_StreamError(t *testing.T) {
	sock := startFakeLibpod(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"stream":"trying..."}`+"\n")
		_, _ = io.WriteString(w, `{"error":"unauthorized: bad creds"}`+"\n")
	})
	c, _ := NewClient(sock)
	err := c.PullImage(context.Background(), "private/foo:latest")
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("expected wrapped error; got %v", err)
	}
}

func TestIsNotFound_IsConflict(t *testing.T) {
	if IsNotFound(nil) || IsConflict(nil) {
		t.Error("nil should be neither")
	}
	if IsNotFound(&APIError{Status: 500}) {
		t.Error("500 is not NotFound")
	}
	if !IsNotFound(&APIError{Status: 404}) {
		t.Error("404 should be NotFound")
	}
	if !IsConflict(&APIError{Status: 409}) {
		t.Error("409 should be Conflict")
	}
}
