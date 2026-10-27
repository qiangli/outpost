package storeroute

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBucketRouted(t *testing.T) {
	const host = "127.0.0.1:31890"
	const target = "/s3-peer-a/key%2Fpart?X-Amz-Signature=abc&key=a%2Fb&key=two"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || r.RequestURI != target {
			t.Errorf("request changed: host=%q uri=%q", r.Host, r.RequestURI)
		}
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "body=%s", b)
	}))
	defer backend.Close()
	h := Handler(func(_ context.Context, p string) (string, error) {
		if p != "peer-a" {
			t.Errorf("peer = %q", p)
		}
		return strings.TrimPrefix(backend.URL, "http://"), nil
	})
	r := httptest.NewRequest(http.MethodPut, "http://"+host+target, strings.NewReader("data"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "body=data" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

func TestUnknownBucket(t *testing.T) {
	h := Handler(func(_ context.Context, peer string) (string, error) { return "", ErrUnknownPeer })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/s3-unknown/key", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown peer") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

func TestPeerFromPath(t *testing.T) {
	for _, tc := range []struct{ path, peer string }{
		{"/s3-peer-a/key", "peer-a"}, {"/s3-self", "self"}, {"/s3-123/", "123"},
		{"/", ""}, {"/bucket/key", ""}, {"/s3-/key", ""}, {"/s3--peer/key", ""},
		{"/s3-peer-/key", ""}, {"/s3-Peer/key", ""}, {"/s3-peer.name/key", ""},
		{"/s3-peer%2Fother/key", ""}, {"/s3-peer_id/key", ""}, {"s3-peer/key", ""},
		{"/s3-" + strings.Repeat("a", 61), ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			peer, ok := PeerFromPath(tc.path)
			if peer != tc.peer || ok != (tc.peer != "") {
				t.Fatalf("got %q, %v", peer, ok)
			}
		})
	}
}
