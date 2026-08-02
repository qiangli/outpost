package vknode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPeerCredential_Validate(t *testing.T) {
	cases := []struct {
		name string
		in   PeerCredential
		ok   bool
	}{
		{"token ok", PeerCredential{APIURL: "https://127.0.0.1:6443", Token: "t"}, true},
		{"cert ok", PeerCredential{APIURL: "https://127.0.0.1:6443", ClientCert: []byte("c"), ClientKey: []byte("k")}, true},
		{"no url", PeerCredential{Token: "t"}, false},
		{"no credential", PeerCredential{APIURL: "https://127.0.0.1:6443"}, false},
		{"half cert (no key)", PeerCredential{APIURL: "https://x", ClientCert: []byte("c")}, false},
		{"half cert (no cert)", PeerCredential{APIURL: "https://x", ClientKey: []byte("k")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected invalid, got nil")
			}
		})
	}
}

func TestPeerCredential_CredentialKind_CertWins(t *testing.T) {
	both := PeerCredential{APIURL: "https://x", Token: "t", ClientCert: []byte("c"), ClientKey: []byte("k")}
	if got := both.CredentialKind(); got != "client-cert" {
		t.Fatalf("client cert should win over token; got %q", got)
	}
	if got := (PeerCredential{Token: "t"}).CredentialKind(); got != "token" {
		t.Fatalf("token kind; got %q", got)
	}
	if got := (PeerCredential{}).CredentialKind(); got != "none" {
		t.Fatalf("none kind; got %q", got)
	}
}

func TestPeerCredential_Materialize_TokenAndRestConfig(t *testing.T) {
	dir := t.TempDir()
	cred := PeerCredential{APIURL: "https://127.0.0.1:6443", CA: []byte("PEER-CA"), Token: "tok-v1"}
	files, err := cred.Materialize(dir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if files.TokenFile == "" || files.CertFile != "" || files.KeyFile != "" {
		t.Fatalf("token cred should write only a token file; got %+v", files)
	}
	got, err := os.ReadFile(files.TokenFile)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(got) != "tok-v1" {
		t.Fatalf("token file = %q, want tok-v1", got)
	}
	// 0600 (credential secrecy).
	if fi, _ := os.Stat(files.TokenFile); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", fi.Mode().Perm())
	}

	cfg, err := cred.RestConfig(files)
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	if cfg.Host != cred.APIURL {
		t.Fatalf("rest host = %q, want %q", cfg.Host, cred.APIURL)
	}
	// Bearer token is a FILE reference (rotation-capable), not inlined.
	if cfg.BearerTokenFile != files.TokenFile {
		t.Fatalf("BearerTokenFile = %q, want %q", cfg.BearerTokenFile, files.TokenFile)
	}
	if cfg.BearerToken != "" {
		t.Fatalf("bearer token must not be inlined (breaks rotation): %q", cfg.BearerToken)
	}
	// Peer CA identity is pinned, not the system roots.
	if string(cfg.TLSClientConfig.CAData) != "PEER-CA" {
		t.Fatalf("CAData = %q, want PEER-CA", cfg.TLSClientConfig.CAData)
	}
}

func TestPeerCredential_Materialize_ClientCertRestConfig(t *testing.T) {
	dir := t.TempDir()
	cred := PeerCredential{
		APIURL:     "https://127.0.0.1:6443",
		CA:         []byte("PEER-CA"),
		ClientCert: []byte("CERT"),
		ClientKey:  []byte("KEY"),
	}
	files, err := cred.Materialize(dir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if files.CertFile == "" || files.KeyFile == "" || files.TokenFile != "" {
		t.Fatalf("cert cred should write cert+key only; got %+v", files)
	}
	cfg, err := cred.RestConfig(files)
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	if cfg.TLSClientConfig.CertFile != files.CertFile || cfg.TLSClientConfig.KeyFile != files.KeyFile {
		t.Fatalf("cert/key files not wired: %+v", cfg.TLSClientConfig)
	}
	if cfg.BearerTokenFile != "" {
		t.Fatalf("client-cert config must not carry a bearer token file")
	}
}

// TestPeerCredential_Materialize_RestartIdempotent proves a restart
// (re-materializing the same credential) lands on identical, stable file
// contents — no churn, same paths.
func TestPeerCredential_Materialize_RestartIdempotent(t *testing.T) {
	dir := t.TempDir()
	cred := PeerCredential{APIURL: "https://127.0.0.1:6443", Token: "stable"}
	f1, err := cred.Materialize(dir)
	if err != nil {
		t.Fatalf("materialize 1: %v", err)
	}
	f2, err := cred.Materialize(dir)
	if err != nil {
		t.Fatalf("materialize 2 (restart): %v", err)
	}
	if f1 != f2 {
		t.Fatalf("restart changed file paths: %+v vs %+v", f1, f2)
	}
	got, _ := os.ReadFile(f2.TokenFile)
	if string(got) != "stable" {
		t.Fatalf("token after restart = %q, want stable", got)
	}
}

// TestPeerCredentialRefresher_LiveTokenRotation proves the cloudbox-free
// rotation path: when the persisted config's bearer token changes, the
// refresher rewrites the token file (which client-go re-reads) — no restart,
// no cloudbox call.
func TestPeerCredentialRefresher_LiveTokenRotation(t *testing.T) {
	dir := t.TempDir()
	cur := PeerCredential{APIURL: "https://127.0.0.1:6443", Token: "tok-v1"}
	files, err := cur.Materialize(dir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// Reloader is a stand-in for conf.LoadFile — a mutable pointer the test
	// flips to simulate an operator rotating cluster.token.
	next := cur
	reload := func() (PeerCredential, error) { return next, nil }

	r := NewPeerCredentialRefresher(reload, files, cur)

	// No change yet → no rotation.
	if rotated, err := r.refreshOnce(); err != nil || rotated {
		t.Fatalf("unchanged token should not rotate; rotated=%v err=%v", rotated, err)
	}

	// Operator rotates the token.
	next.Token = "tok-v2"
	rotated, err := r.refreshOnce()
	if err != nil {
		t.Fatalf("refresh after rotation: %v", err)
	}
	if !rotated {
		t.Fatalf("expected a rotation to be applied")
	}
	got, _ := os.ReadFile(files.TokenFile)
	if string(got) != "tok-v2" {
		t.Fatalf("token file after rotation = %q, want tok-v2", got)
	}

	// Idempotent: a second pass with no further change does nothing.
	if rotated, err := r.refreshOnce(); err != nil || rotated {
		t.Fatalf("second pass should be a no-op; rotated=%v err=%v", rotated, err)
	}
}

func TestDefaultPeerCredentialDir(t *testing.T) {
	got, err := DefaultPeerCredentialDir()
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if filepath.Base(got) != "cluster-peer" {
		t.Fatalf("dir = %q, want .../cluster-peer", got)
	}
}
