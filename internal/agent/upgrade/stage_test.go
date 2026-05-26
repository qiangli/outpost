package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageFromLocal(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	body := []byte("hello outpost candidate")
	if err := os.WriteFile(src, body, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "outpost.upgrading")
	if err := StageFromLocal(src, dst, ""); err != nil {
		t.Fatalf("StageFromLocal: %v", err)
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

func TestStageFromURL_WithSHA(t *testing.T) {
	body := []byte("hello from server")
	sum := sha256.Sum256(body)
	wantHex := hex.EncodeToString(sum[:])

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "outpost.upgrading")
	if err := StageFromURL(context.Background(), dst, srv.URL, wantHex, srv.Client()); err != nil {
		t.Fatalf("StageFromURL: %v", err)
	}

	// Wrong sha → error.
	_ = os.Remove(dst)
	wrong := strings.Repeat("0", 64)
	err := StageFromURL(context.Background(), dst, srv.URL, wrong, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch, got %v", err)
	}
}

func TestStageFromURL_RejectsHTTP(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "outpost.upgrading")
	err := StageFromURL(context.Background(), dst, "http://example.com/bin", "", nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected https-only error, got %v", err)
	}
}

func TestStageFromLocal_RefusesStaleCandidate(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "outpost.upgrading")
	if err := os.WriteFile(dst, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := StageFromLocal(src, dst, "")
	if err == nil || !strings.Contains(err.Error(), "stale upgrade candidate") {
		t.Fatalf("expected stale-candidate error, got %v", err)
	}
}
