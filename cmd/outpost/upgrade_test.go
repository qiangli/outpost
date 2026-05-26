package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeOutpostBinary returns the path to a small Go-built program that
// pretends to be an outpost binary for probe-only purposes: it answers
// `version --json` with the provided BuildInfo-shaped JSON. Everything
// else exits non-zero. Used to drive probeCandidate without coupling to
// the real `outpost` binary.
func fakeOutpostBinary(t *testing.T, jsonBody string, exit int) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.go")
	body := "package main\nimport (\"fmt\"; \"os\")\nfunc main() {\nif len(os.Args) >= 3 && os.Args[1] == \"version\" && os.Args[2] == \"--json\" {\n  fmt.Print(`" + jsonBody + "`)\n  os.Exit(" + itoa(exit) + ")\n}\nos.Exit(2)\n}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "fake")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	if err := runGo(t, dir, "build", "-o", out, src); err != nil {
		t.Fatal(err)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 8)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func runGo(t *testing.T, dir string, args ...string) error {
	t.Helper()
	c := exec.Command("go", args...)
	c.Dir = dir
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	return c.Run()
}

func TestProbeCandidate_Valid(t *testing.T) {
	bin := fakeOutpostBinary(t, `{"commit":"abc1234567","vcs_time":"2026-05-26T16:00:00Z","dirty":false,"go_version":"go1.26.0"}`, 0)
	got, err := probeCandidate(bin)
	if err != nil {
		t.Fatalf("probeCandidate: %v", err)
	}
	if got.Commit != "abc1234567" || got.GoVersion != "go1.26.0" {
		t.Fatalf("unexpected BuildInfo: %+v", got)
	}
}

func TestProbeCandidate_RejectsNonJSON(t *testing.T) {
	bin := fakeOutpostBinary(t, "not json at all", 0)
	if _, err := probeCandidate(bin); err == nil {
		t.Fatal("expected error for non-JSON output")
	}
}

func TestProbeCandidate_RejectsMissingGoVersion(t *testing.T) {
	bin := fakeOutpostBinary(t, `{"commit":"abc1234"}`, 0)
	if _, err := probeCandidate(bin); err == nil {
		t.Fatal("expected error when go_version is empty")
	}
}

func TestProbeCandidate_RejectsNonZeroExit(t *testing.T) {
	bin := fakeOutpostBinary(t, `{"go_version":"go1.26.0"}`, 1)
	if _, err := probeCandidate(bin); err == nil {
		t.Fatal("expected error when probe exits non-zero")
	}
}

func TestStageCandidate_FromLocal(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	body := []byte("hello outpost candidate")
	if err := os.WriteFile(src, body, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "outpost.upgrading")
	if err := stageCandidate(context.Background(), dst, "", src, ""); err != nil {
		t.Fatalf("stageCandidate local: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("body mismatch: %q", got)
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("candidate not executable: %v", info.Mode())
	}
}

func TestStageCandidate_FromURL_WithSHA(t *testing.T) {
	body := []byte("hello from server")
	sum := sha256.Sum256(body)
	wantHex := hex.EncodeToString(sum[:])

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Use the TLS server's client so cert validation passes; injected via
	// http.DefaultClient swap for the duration of the test.
	prev := http.DefaultClient
	http.DefaultClient = srv.Client()
	defer func() { http.DefaultClient = prev }()

	dir := t.TempDir()
	dst := filepath.Join(dir, "outpost.upgrading")
	if err := stageCandidate(context.Background(), dst, srv.URL, "", wantHex); err != nil {
		t.Fatalf("stageCandidate url: %v", err)
	}

	// Wrong sha → error and leaves caller responsible for cleanup.
	_ = os.Remove(dst)
	wrong := "0000000000000000000000000000000000000000000000000000000000000000"
	err := stageCandidate(context.Background(), dst, srv.URL, "", wrong)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch, got %v", err)
	}
}

func TestStageCandidate_RejectsHTTP(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "outpost.upgrading")
	err := stageCandidate(context.Background(), dst, "http://example.com/bin", "", "")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected https-only error, got %v", err)
	}
}

func TestStageCandidate_RefusesStaleCandidate(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "outpost.upgrading")
	if err := os.WriteFile(dst, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := stageCandidate(context.Background(), dst, "", src, "")
	if err == nil || !strings.Contains(err.Error(), "stale upgrade candidate") {
		t.Fatalf("expected stale-candidate error, got %v", err)
	}
}
