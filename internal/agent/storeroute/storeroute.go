// Package storeroute provides a loopback listener that routes path-style
// s3-<peer-id> buckets to the peer's s3 service, preserving signed request URLs.
package storeroute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
)

// DefaultPort mirrors yoke loom.DefaultStorePort.
const DefaultPort = 31890

// ErrUnknownPeer means no peer by that name exposes the s3 service.
var ErrUnknownPeer = errors.New("unknown peer")

// Resolver maps a peer ID to a dialable host:port of its s3 service
// (the local backend for self, else a mesh forward address).
type Resolver func(ctx context.Context, peer string) (addr string, err error)

// Handler returns the bucket-routing reverse proxy.
func Handler(resolve Resolver) http.Handler {
	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			addr, _ := r.Context().Value(addrKey{}).(string)
			r.URL.Scheme = "http"
			r.URL.Host = addr // r.Host is left untouched: Host passes through
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "storage peer unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, ok := PeerFromPath(r.URL.EscapedPath())
		if !ok {
			http.Error(w, "storage bucket must be s3-<peer-id>", http.StatusNotFound)
			return
		}
		addr, err := resolve(r.Context(), peer)
		if err != nil {
			code := http.StatusBadGateway
			if errors.Is(err, ErrUnknownPeer) {
				code = http.StatusNotFound
			}
			http.Error(w, fmt.Sprintf("storage peer %q: %v", peer, err), code)
			return
		}
		rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), addrKey{}, addr)))
	})
}

type addrKey struct{}

// PeerFromPath extracts the peer ID from the first path segment s3-<peer-id>.
// Inspect the escaped path so an encoded separator cannot change the bucket.
func PeerFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	bucket, _, _ := strings.Cut(path[1:], "/")
	if !strings.HasPrefix(bucket, "s3-") || len(bucket) > 63 {
		return "", false
	}
	peer := strings.TrimPrefix(bucket, "s3-")
	if peer == "" || peer[0] == '-' || peer[len(peer)-1] == '-' {
		return "", false
	}
	for _, c := range peer {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", false
		}
	}
	return peer, true
}
