// Package storeroute is the stable peer storage name: a loopback listener
// that routes <peer>.<sat>.localhost requests to that peer's `s3` mesh
// service, passing Host through unchanged (SigV4 presigned URLs sign it).
package storeroute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
)

// DefaultPort mirrors yoke loom.DefaultStorePort.
const DefaultPort = 31890

// ErrUnknownPeer means no peer by that name exposes the s3 service.
var ErrUnknownPeer = errors.New("unknown peer")

// Resolver maps a peer label to a dialable host:port of its s3 service
// (the local backend for self, else a mesh forward address).
type Resolver func(ctx context.Context, peer string) (addr string, err error)

// Handler returns the Host-routing reverse proxy.
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
		peer, ok := PeerFromHost(r.Host)
		if !ok {
			http.Error(w, fmt.Sprintf("host %q is not <peer>.<sat>.localhost", r.Host), http.StatusNotFound)
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

// PeerFromHost extracts the leading label of <peer>.<sat>.localhost[:port].
func PeerFromHost(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	parts := strings.Split(strings.ToLower(host), ".")
	if len(parts) != 3 || parts[len(parts)-1] != "localhost" || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0], true
}
