package vknode

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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

func TestResolveCommandRejectsMissingCredentialProfile(t *testing.T) {
	raw, err := NewNativeProcessBackend(NativeProcessConfig{
		DataDir: t.TempDir(),
		ArtifactCredentials: staticNativeArtifactCredentialResolver{
			err: errors.New("profile not found"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pod, _ := makeHelperPod(t, "artifact-pod", "artifact-uid", "exit")
	pod.Namespace = "user-a"
	pod.Annotations = map[string]string{
		NativeArtifactURLAnnotation:               "https://artifacts.example.test/nanochat/runner.tar.gz",
		NativeArtifactSHA256Annotation:            strings.Repeat("0", 64),
		NativeArtifactPathAnnotation:              "bin/runner",
		NativeArtifactCredentialProfileAnnotation: "nanochat-read",
	}
	_, err = raw.(*nativeProcessBackend).resolveCommand(context.Background(), pod, "runner")
	if err == nil || !strings.Contains(err.Error(), "profile \"nanochat-read\" unavailable") {
		t.Fatalf("missing profile error = %v", err)
	}
	if strings.Contains(err.Error(), "profile not found") {
		t.Fatalf("resolver details leaked into error: %v", err)
	}
}

func TestPrivateArtifactRejectsScopeMismatchBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	scope, err := url.Parse(server.URL + "/allowed/")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.(*nativeProcessBackend).materializeNativeArtifactWithCredential(
		context.Background(),
		server.URL+"/other/runner.tar.gz",
		strings.Repeat("0", 64),
		"bin/runner",
		&NativeArtifactCredentialProfile{
			Kind:      nativeArtifactProfileKindAWSSigV4,
			Scope:     scope,
			AccessKey: "access",
			SecretKey: "secret",
			Region:    "us-east-1",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "outside credential profile scope") {
		t.Fatalf("scope mismatch error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("scope mismatch sent %d HTTP requests, want 0", got)
	}
}

func TestPrivateArtifactRejectsRedirectWithoutCredentialLeak(t *testing.T) {
	archive := tarGzipFixture(t, "bin/runner", []byte("runner"))
	sum := sha256.Sum256(archive)
	var sinkAuthorization atomic.Value
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkAuthorization.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Error("origin request was not signed")
		}
		http.Redirect(w, r, sink.URL+"/leak", http.StatusFound)
	}))
	defer origin.Close()
	scope, err := url.Parse(origin.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.(*nativeProcessBackend).materializeNativeArtifactWithCredential(
		context.Background(),
		origin.URL+"/runner.tar.gz",
		fmt.Sprintf("%x", sum[:]),
		"bin/runner",
		&NativeArtifactCredentialProfile{
			Kind:      nativeArtifactProfileKindAWSSigV4,
			Scope:     scope,
			AccessKey: "access",
			SecretKey: "secret",
			Region:    "us-east-1",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect error = %v", err)
	}
	if got := sinkAuthorization.Load(); got != nil {
		t.Fatalf("redirect target was contacted; Authorization=%q", got)
	}
}

func TestPrivateArtifactRejectsQueryMetadata(t *testing.T) {
	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.(*nativeProcessBackend).materializeNativeArtifact(
		context.Background(),
		"https://artifacts.example.test/runner.tar.gz?X-Amz-Signature=secret",
		strings.Repeat("0", 64),
		"bin/runner",
	)
	if err == nil || !strings.Contains(err.Error(), "must not contain userinfo, query, or fragment") {
		t.Fatalf("query-bearing URL error = %v", err)
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked query material: %v", err)
	}
}

func TestNativeArtifactCacheSeparatesArchiveMembers(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for member, payload := range map[string][]byte{
		"bin/one": []byte("one"),
		"bin/two": []byte("two"),
	} {
		if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	server, sum := artifactServer(t, buf.Bytes())
	defer server.Close()
	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	be := raw.(*nativeProcessBackend)
	one, err := be.materializeNativeArtifact(context.Background(), server.URL+"/tools.tar.gz", sum, "bin/one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := be.materializeNativeArtifact(context.Background(), server.URL+"/tools.tar.gz", sum, "bin/two")
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Fatal("different archive members shared one cache path")
	}
	assertArtifactContents(t, one, []byte("one"))
	assertArtifactContents(t, two, []byte("two"))
}

func TestNativeArtifactRequiresExactArchiveMember(t *testing.T) {
	archive := tarGzipFixture(t, "tmp/../bin/runner", []byte("runner"))
	server, sum := artifactServer(t, archive)
	defer server.Close()
	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.(*nativeProcessBackend).materializeNativeArtifact(
		context.Background(), server.URL+"/tools.tar.gz", sum, "bin/runner",
	)
	if err == nil || !strings.Contains(err.Error(), `path "bin/runner" not found`) {
		t.Fatalf("archive alias error = %v", err)
	}
}

type staticNativeArtifactCredentialResolver struct {
	profile NativeArtifactCredentialProfile
	err     error
}

func (r staticNativeArtifactCredentialResolver) ResolveNativeArtifactCredential(
	context.Context,
	string,
	string,
) (NativeArtifactCredentialProfile, error) {
	return r.profile, r.err
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
