package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gtsteffaniak/filebrowser/backend/fbembed"
)

// outpost's `files` builtin offers folder downloads. The engine behind it
// (the fbembed seam over the maintained File Browser) fixes the archive set at
// zip and tar.gz — there is no build tag to forget any more, which is why
// this file no longer carries one and `go test ./...` runs it untagged.
//
// These drive each format through the SAME fbembed handler main.go mounts, so
// the claim is backed by a real, readable archive rather than a status code.
var wantFormats = []string{"zip", "tar.gz"}

func TestFolderDownloadServesEveryAdvertisedFormat(t *testing.T) {
	h := filesHandler(t)
	for _, algo := range wantFormats {
		t.Run(algo, func(t *testing.T) {
			w := rawDownload(t, h, "/sub", algo)
			if w.Code != 200 {
				t.Fatalf("algo=%s: status %d, want 200 (body %.200q)", algo, w.Code, w.Body.String())
			}
			if w.Body.Len() == 0 {
				t.Fatalf("algo=%s: empty archive body", algo)
			}
			if !archiveHas(t, algo, w.Body.Bytes(), "a.txt") {
				t.Fatalf("algo=%s: archive has no a.txt", algo)
			}
		})
	}
}

// zip is what a browser's plain "download folder" sends (the default when no
// algo is named), so prove that one all the way to a readable member.
func TestFolderDownloadZipIsReadable(t *testing.T) {
	h := filesHandler(t)
	w := rawDownload(t, h, "/sub", "")
	if w.Code != 200 {
		t.Fatalf("status %d, want 200 (body %.200q)", w.Code, w.Body.String())
	}
	if !archiveHas(t, "zip", w.Body.Bytes(), "a.txt") {
		t.Fatalf("default download is not a zip with a.txt")
	}
}

// filesHandler boots the embed over a scratch scope holding sub/a.txt.
// fbembed runs noauth — the embedding host is the access gate — so no token
// is minted or carried; the engine's lone user answers every request.
func filesHandler(t *testing.T) http.Handler {
	t.Helper()
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
	t.Cleanup(func() { _ = closer() })
	return h
}

// rawDownload issues the folder download the browser issues.
func rawDownload(t *testing.T, h http.Handler, dir, algo string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{"source": {fbembed.SourceName}, "file": {dir}}
	if algo != "" {
		q.Set("algo", algo)
	}
	r := httptest.NewRequest("GET", "/api/resources/download?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// archiveHas reports whether the archive body of the given format contains a
// member with the given base name.
func archiveHas(t *testing.T, algo string, body []byte, name string) bool {
	t.Helper()
	switch algo {
	case "zip":
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatalf("response is not a readable zip: %v", err)
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == name {
				return true
			}
		}
	case "tar.gz":
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("response is not gzip: %v", err)
		}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("response is not a readable tar: %v", err)
			}
			if filepath.Base(hdr.Name) == name {
				return true
			}
		}
	default:
		t.Fatalf("unknown archive format %q", algo)
	}
	return false
}
