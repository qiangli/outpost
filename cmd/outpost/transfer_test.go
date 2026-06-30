package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestIsHexDigest guards the anti-path-traversal check on /blob's digest. A
// digest that isn't pure lowercase hex must be rejected — otherwise a malicious
// peer could escape the blobs dir.
func TestIsHexDigest(t *testing.T) {
	good := []string{"abc123", "0123456789abcdef", "deadbeef", "00"}
	bad := []string{"", "ABC", "../etc/passwd", "ab/cd", "ab.cd", "sha256:abc", "g123", "ab cd"}
	for _, s := range good {
		if !isHexDigest(s) {
			t.Errorf("isHexDigest(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isHexDigest(s) {
			t.Errorf("isHexDigest(%q) = true, want false (security)", s)
		}
	}
}

// TestModelBlobsRejectsBadDigest asserts /blob 400s on a non-hex digest before
// touching the filesystem — the traversal guard at the HTTP boundary.
func TestModelBlobsRejectsBadDigest(t *testing.T) {
	h := modelBlobsHandler()
	for _, d := range []string{"../../etc/passwd", "ab/cd", "", "sha256:deadbeef"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/blob?digest="+url.QueryEscape(d), nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("/blob digest=%q → %d, want 400", d, rec.Code)
		}
	}
}
