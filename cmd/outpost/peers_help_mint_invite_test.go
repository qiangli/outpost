package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// mintInviteFixture puts a paired agent.json on disk (ServerAddr/Port
// pointing at srv, so cloudboxHTTPBase resolves to the test server) and
// returns the ready-to-run command. Pairing data is described only in
// terms of the public wire: register/exchange/recover.
func mintInviteFixture(t *testing.T, srv *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	if p := u.Port(); p != "" {
		if n, err := parsePort(p); err == nil {
			port = n
		}
	}
	dir := filepath.Join(tmp, "matrix")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fc := &conf.FileConfig{
		AgentName:   "host-a",
		AccessToken: "test-bearer",
		ServerAddr:  u.Hostname(),
		ServerPort:  port,
		Protocol:    "tcp",
	}
	if err := conf.SaveFile(filepath.Join(dir, "agent.json"), fc); err != nil {
		t.Fatal(err)
	}
}

// newMintInviteServer counts POSTs to /api/v1/hosts/invites and answers
// them with a realistic one-time invite payload.
func newMintInviteServer(posts *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/hosts/invites") {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method: "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    "inv-test-code-1234",
			"expires": time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
		})
	}))
}

// captureMintOutput runs fn with both stdout and stderr piped and returns
// what was printed on each. The command prints the code to stdout and the
// follow-up hint to stderr, so a leak is detectable on either stream.
func captureMintOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()
	fn()
	_ = outW.Close()
	_ = errW.Close()
	var ob, eb bytes.Buffer
	_, _ = io.Copy(&ob, outR)
	_, _ = io.Copy(&eb, errR)
	return ob.String(), eb.String()
}

// TestHelpMintInviteRequiresExplicitConfirm — the command's name reads as
// documentation, so running it to learn usage must NOT mint a real invite.
// Without --yes it must make ZERO HTTP requests, exit non-zero, print what
// it would have done and how to confirm, and never print a code.
func TestHelpMintInviteRequiresExplicitConfirm(t *testing.T) {
	var posts atomic.Int32
	srv := newMintInviteServer(&posts)
	t.Cleanup(srv.Close)
	mintInviteFixture(t, srv)

	cmd := peersHelpMintInviteCmd()
	cmd.SetArgs([]string{"host-b"})
	stdout, stderr := captureMintOutput(t, func() {
		if err := cmd.Execute(); err == nil {
			t.Fatal("help-mint-invite without --yes must exit non-zero")
		}
	})

	if got := posts.Load(); got != 0 {
		t.Fatalf("no --yes: made %d HTTP requests, want 0", got)
	}
	if strings.Contains(stdout, "inv-test-code-1234") || strings.Contains(stderr, "inv-test-code-1234") {
		t.Errorf("no --yes: printed a minted code:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == "" {
		t.Error("no --yes: printed nothing — must explain what it would do and how to confirm")
	}
	lower := strings.ToLower(stdout + stderr)
	if !strings.Contains(lower, "--yes") {
		t.Errorf("no --yes: output must tell the operator how to confirm:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}

// TestHelpMintInviteWithConfirmMints — with --yes the mint happens exactly
// once and the code is printed for the operator.
func TestHelpMintInviteWithConfirmMints(t *testing.T) {
	var posts atomic.Int32
	srv := newMintInviteServer(&posts)
	t.Cleanup(srv.Close)
	mintInviteFixture(t, srv)

	cmd := peersHelpMintInviteCmd()
	cmd.SetArgs([]string{"host-b", "--yes"})
	stdout, stderr := captureMintOutput(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("help-mint-invite --yes: %v", err)
		}
	})

	if got := posts.Load(); got != 1 {
		t.Fatalf("with --yes: made %d HTTP requests, want 1", got)
	}
	if !strings.Contains(stdout, "inv-test-code-1234") {
		t.Errorf("with --yes: code not printed on stdout:\n%s", stdout)
	}
	_ = stderr
}

// TestHelpMintInviteUnpairedNeedsNoConfirm — the guard fires before any
// network I/O, so an unpaired host still errors out without --yes and
// without a request.
func TestHelpMintInviteUnpairedNeedsNoConfirm(t *testing.T) {
	var posts atomic.Int32
	srv := newMintInviteServer(&posts)
	t.Cleanup(srv.Close)
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	cmd := peersHelpMintInviteCmd()
	cmd.SetArgs([]string{"host-b"})
	stdout, stderr := captureMintOutput(t, func() {
		if err := cmd.Execute(); err == nil {
			t.Fatal("unpaired help-mint-invite must exit non-zero")
		}
	})
	if got := posts.Load(); got != 0 {
		t.Fatalf("unpaired: made %d HTTP requests, want 0", got)
	}
	if strings.Contains(stdout, "inv-test-code-1234") || strings.Contains(stderr, "inv-test-code-1234") {
		t.Errorf("unpaired: printed a code:\n%s%s", stdout, stderr)
	}
}

var _ = context.Background
