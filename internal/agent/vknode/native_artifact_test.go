package vknode

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMaterializeNativeArtifactTarGzipAndCache(t *testing.T) {
	payload := []byte("native-tar-payload")
	archive := tarGzipFixture(t, "release/bashy", payload)
	server, sum := artifactServer(t, archive)
	defer server.Close()

	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	be := raw.(*nativeProcessBackend)
	gotPath, err := be.materializeNativeArtifact(context.Background(),
		server.URL+"/bashy-darwin-arm64.tar.gz", sum, "release/bashy")
	if err != nil {
		t.Fatalf("materialize tar.gz: %v", err)
	}
	assertArtifactContents(t, gotPath, payload)

	// The content-addressed result must remain usable without the origin.
	server.Close()
	cachedPath, err := be.materializeNativeArtifact(context.Background(),
		server.URL+"/bashy-darwin-arm64.tar.gz", sum, "release/bashy")
	if err != nil {
		t.Fatalf("read cached artifact: %v", err)
	}
	if cachedPath != gotPath {
		t.Fatalf("cached path = %q, want %q", cachedPath, gotPath)
	}
}

func TestMaterializeNativeArtifactZip(t *testing.T) {
	payload := []byte("native-zip-payload")
	archive := zipFixture(t, "bashy.exe", payload)
	server, sum := artifactServer(t, archive)
	defer server.Close()

	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	gotPath, err := raw.(*nativeProcessBackend).materializeNativeArtifact(context.Background(),
		server.URL+"/bashy-windows-amd64.zip", sum, "bashy.exe")
	if err != nil {
		t.Fatalf("materialize zip: %v", err)
	}
	if filepath.Ext(gotPath) != ".exe" {
		t.Fatalf("materialized Windows artifact path = %q, want .exe suffix", gotPath)
	}
	assertArtifactContents(t, gotPath, payload)
}

func TestMaterializeNativeArtifactRejectsUnsafeInputs(t *testing.T) {
	archive := zipFixture(t, "bashy", []byte("payload"))
	server, sum := artifactServer(t, archive)
	defer server.Close()
	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	be := raw.(*nativeProcessBackend)
	for _, tc := range []struct {
		name   string
		url    string
		sum    string
		member string
	}{
		{name: "unverified transport", url: "http://example.com/bashy.zip", sum: sum, member: "bashy"},
		{name: "bad checksum syntax", url: server.URL + "/bashy.zip", sum: "abc", member: "bashy"},
		{name: "parent traversal", url: server.URL + "/bashy.zip", sum: sum, member: "../bashy"},
		{name: "absolute path", url: server.URL + "/bashy.zip", sum: sum, member: "/bashy"},
		{name: "checksum mismatch", url: server.URL + "/bashy.zip", sum: fmt.Sprintf("%064x", 1), member: "bashy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := be.materializeNativeArtifact(context.Background(), tc.url, tc.sum, tc.member); err == nil {
				t.Fatal("materialize unexpectedly succeeded")
			}
		})
	}
}

func TestResolveCommandRequiresCompleteArtifactDeclaration(t *testing.T) {
	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	pod, _ := makeHelperPod(t, "artifact-pod", "artifact-uid", "exit")
	pod.Annotations = map[string]string{
		NativeArtifactURLAnnotation: "https://example.com/bashy.tar.gz",
	}
	if _, err := raw.(*nativeProcessBackend).resolveCommand(context.Background(), pod, "bashy"); err == nil {
		t.Fatal("resolveCommand accepted an artifact without checksum and path")
	}
}

func artifactServer(t *testing.T, body []byte) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	sum := sha256.Sum256(body)
	return server, fmt.Sprintf("%x", sum[:])
}

func tarGzipFixture(t *testing.T, name string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipFixture(t *testing.T, name string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0o755)
	dst, err := zw.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func assertArtifactContents(t *testing.T, artifactPath string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("artifact contents = %q, want %q", got, want)
	}
	info, err := os.Stat(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o100 == 0 {
		t.Fatalf("artifact mode = %v, owner execute bit is absent", info.Mode())
	}
}
