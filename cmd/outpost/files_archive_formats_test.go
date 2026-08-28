package main

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/filebrowser/filebrowser/v2/fbembed"
	fbhttp "github.com/filebrowser/filebrowser/v2/http"
)

// outpost's `files` builtin has always offered nine folder-download formats.
// The ../filebrowser fork later put six of them behind -tags fb_archives so the
// bashy web console could drop ~50 packages it does not need; outpost keeps
// them, and scripts/lib.sh (GO_TAGS) passes the tag on every build path.
//
// A missing build tag is SILENT: the binary still compiles, still serves
// folders, and just quietly stops accepting six formats. These tests are what
// turns that into a build failure, so this file deliberately does NOT carry a
// build tag of its own — an untagged `go test ./...` is SUPPOSED to fail here.
// Use `make test`, which passes the tag.
const wantFormatCount = 9

func TestFolderDownloadFormatCoverage(t *testing.T) {
	got := fbhttp.ArchiveFormats()
	if len(got) != wantFormatCount {
		t.Fatalf("files builtin offers %d folder-download formats %v, want %d.\n"+
			"This build is missing -tags fb_archives. Build with ./scripts/build.sh "+
			"and test with `make test` (see GO_TAGS in scripts/lib.sh).",
			len(got), got, wantFormatCount)
	}
	for _, want := range []string{"zip", "tar", "targz", "tarbz2", "tarxz", "tarlz4", "tarsz", "tarbr", "tarzst"} {
		if !contains(got, want) {
			t.Errorf("folder-download format %q is gone; files builtin narrowed", want)
		}
	}
}

// The list above is a claim. This drives the format through the SAME fbembed
// handler main.go mounts, so the claim is backed by a real response.
func TestFolderDownloadServesEveryAdvertisedFormat(t *testing.T) {
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "sub", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, closer, err := fbembed.New(fbembed.Options{Scope: scope})
	if err != nil {
		t.Fatalf("fbembed.New: %v", err)
	}
	defer func() { _ = closer() }()
	tok := fbToken(t, h)

	for _, algo := range fbhttp.ArchiveFormats() {
		t.Run(algo, func(t *testing.T) {
			w := rawDownload(t, h, tok, "sub", algo)
			if w.Code != 200 {
				t.Fatalf("algo=%s: status %d, want 200 (body %.200q)", algo, w.Code, w.Body.String())
			}
			if w.Body.Len() == 0 {
				t.Fatalf("algo=%s: empty archive body", algo)
			}
		})
	}
}

// zip is what a browser's plain "download folder" sends, so prove that one all
// the way to a readable member rather than just a non-empty body.
func TestFolderDownloadZipIsReadable(t *testing.T) {
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "sub", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, closer, err := fbembed.New(fbembed.Options{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer() }()

	w := rawDownload(t, h, fbToken(t, h), "sub", "zip")
	if w.Code != 200 {
		t.Fatalf("status %d, want 200 (body %.200q)", w.Code, w.Body.String())
	}
	body := w.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("response is not a readable zip: %v", err)
	}
	var found bool
	for _, f := range zr.File {
		if filepath.Base(f.Name) == "a.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("zip has no a.txt; members=%d", len(zr.File))
	}
}

// fbembed runs File Browser in its NoAuth posture: GET /api/login mints the
// single embedded user's JWT with no credentials, which is exactly what the
// browser does before it downloads anything. Every /api/raw request needs it.
func fbToken(t *testing.T, h http.Handler) string {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("fbembed /api/login: status %d, want 200", w.Code)
	}
	tok := strings.TrimSpace(w.Body.String())
	if tok == "" {
		t.Fatal("fbembed /api/login returned an empty token")
	}
	return tok
}

// rawDownload issues the folder download the browser issues, with auth.
func rawDownload(t *testing.T, h http.Handler, tok, dir, algo string) *httptest.ResponseRecorder {
	t.Helper()
	u := "/api/raw/" + dir + "?algo=" + url.QueryEscape(algo) + "&auth=" + url.QueryEscape(tok)
	r := httptest.NewRequest("GET", u, nil)
	r.Header.Set("X-Auth", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
