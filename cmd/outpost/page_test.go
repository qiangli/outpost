package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPageCLIInvokesOnceAndReadsResult(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-page-token" {
			t.Errorf("missing bearer")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/page/invoke":
			posts.Add(1)
			w.Write([]byte(`{"id":"command-1","status":"queued"}`))
		case "/api/page/result":
			w.Write([]byte(`{"id":"command-1","status":"completed","result":{"shape_only":true}}`))
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	old := rootDialOpts
	defer func() { rootDialOpts = old }()
	rootDialOpts.Host = server.URL
	rootDialOpts.Token = "test-page-token"
	rootDialOpts.Remote = ""
	cmd := pageCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"shape-only", "page-1", "on"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || !strings.Contains(output.String(), `"shape_only":true`) {
		t.Fatalf("posts=%d output=%s", posts.Load(), output.String())
	}
}

func TestPageCLIRejectsInvalidInputBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{{"invoke", "page-1", "outpost.navigate", "{"}, {"shape-only", "page-1", "maybe"}} {
		cmd := pageCmd()
		cmd.SetArgs(args)
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestPageCLICookieHostDoesNotRequireBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("cookie request borrowed bearer")
		}
		cookie, err := r.Cookie("outpost_admin")
		if err != nil || cookie.Value != "test-cookie" {
			t.Errorf("missing selected cookie: %v", err)
		}
		w.Write([]byte(`{"pages":[]}`))
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "cookie")
	if err := os.WriteFile(file, []byte("test-cookie\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := rootDialOpts
	defer func() { rootDialOpts = old }()
	rootDialOpts.Host = server.URL
	rootDialOpts.Token = ""
	rootDialOpts.Remote = ""
	cmd := pageCmd()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"--cookie-file", file, "list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestPageCLIDoesNotReplayRedirectedInvocation(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts.Add(1)
				}
				if r.URL.Path == "/api/page/invoke" {
					w.Header().Set("Location", "/replayed")
					w.WriteHeader(status)
					return
				}
				w.Write([]byte(`{"id":"replayed","status":"queued"}`))
			}))
			defer server.Close()
			old := rootDialOpts
			defer func() { rootDialOpts = old }()
			rootDialOpts.Host = server.URL
			rootDialOpts.Token = "test-page-token"
			rootDialOpts.Remote = ""
			cmd := pageCmd()
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs([]string{"invoke", "page-1", "outpost.networking.submit"})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "not retried") {
				t.Fatalf("expected no-retry error: %v", err)
			}
			if posts.Load() != 1 {
				t.Fatalf("redirect replayed invocation %d times", posts.Load())
			}
		})
	}
}
