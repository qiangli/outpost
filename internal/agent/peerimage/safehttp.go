package peerimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxRedirectHops bounds the explicit redirect loop. Five is generous for a
// recipe fetch and small enough that a redirect cycle terminates promptly.
const maxRedirectHops = 5

// maxRecipeBytes bounds a fetched recipe document.
const maxRecipeBytes = 1 << 20

// ErrLoopbackTarget is returned when a URL — at ANY hop — resolves to an
// address the daemon must never be talked into fetching. The daemon's own
// admin/MCP surface listens on loopback, so a redirect that lands there is an
// SSRF primitive against this process, not a routing detail.
var ErrLoopbackTarget = errors.New("refusing to fetch a loopback/link-local address")

// AddrPolicy decides which URLs the recipe fetcher may contact.
//
// The peer path legitimately fetches over loopback: a mesh forward is a local
// TCP listener this daemon opened itself. So the rule is not "no loopback" but
// "no loopback OTHER than the exact forward addresses we opened" — which is why
// the allowance is an exact host:port set rather than a subnet.
type AddrPolicy struct {
	// AllowLoopback is the exact "host:port" set permitted to resolve to a
	// loopback address. Populate it with the mesh forward listeners this
	// daemon opened, and nothing else.
	AllowLoopback map[string]struct{}

	// Resolve maps a hostname to IPs. nil → net.DefaultResolver. Injected so
	// the multi-hop tests run offline with no DNS.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
}

// Allow returns a policy permitting exactly these host:port loopback targets.
func Allow(addrs ...string) AddrPolicy {
	m := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		if a = strings.TrimSpace(a); a != "" {
			m[a] = struct{}{}
		}
	}
	return AddrPolicy{AllowLoopback: m}
}

func (p AddrPolicy) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if p.Resolve != nil {
		return p.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// Check validates ONE hop. Callers must invoke it for every hop, including
// every Location a redirect produces — a check applied only to the first URL is
// bypassed by a chain whose last hop is the interesting one.
func (p AddrPolicy) Check(ctx context.Context, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("nil url")
	}
	switch u.Scheme {
	case "http", "https":
	default:
		// file://, gopher://, unix:// and friends are not fetchable here.
		return fmt.Errorf("refusing scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url has no host")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	allowed := p.AllowLoopback
	_, exempt := allowed[net.JoinHostPort(host, port)]

	ips, err := p.resolve(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		// Never "allow because we learned nothing".
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		if !blockedIP(ip) {
			continue
		}
		if exempt {
			// An explicitly-allowed forward address. Still bounded to the
			// exact host:port — a different loopback port is not covered.
			continue
		}
		return fmt.Errorf("%w: %s resolves to %s", ErrLoopbackTarget, host, ip)
	}
	return nil
}

// blockedIP reports whether an address is one the daemon must not be steered
// at: its own loopback surfaces, the unspecified address (which reaches
// loopback on most stacks), link-local (cloud metadata lives at 169.254.169.254)
// and multicast.
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IsLoopback already covers the IPv4-mapped form (::ffff:127.0.0.1).
	return ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
}

// Fetcher performs recipe fetches with redirects DISABLED at the transport and
// re-driven explicitly, so the policy runs before every single request.
//
// http.Client's CheckRedirect hook is not used to make the decision. Returning
// an error from it after the fact still leaves the redirect semantics inside
// the client; driving the chain here makes "validate, then request" the only
// order that exists.
type Fetcher struct {
	Policy AddrPolicy
	// Client is the underlying transport. Its CheckRedirect is overwritten —
	// this type never lets the stdlib follow a redirect on its own.
	Client *http.Client
}

// NewFetcher builds a Fetcher with redirects disabled and a bounded timeout.
func NewFetcher(policy AddrPolicy) *Fetcher {
	return &Fetcher{
		Policy: policy,
		Client: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Get fetches rawURL, following redirects MANUALLY and re-validating each
// Location before it is requested. Returns the body of the first non-redirect
// 2xx response.
func (f *Fetcher) Get(ctx context.Context, rawURL string) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Belt and braces: even a caller-supplied Client must not follow on its own.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	next, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	for hop := 0; hop <= maxRedirectHops; hop++ {
		// Validate THIS hop before issuing it. Hop 0 is the caller's URL; every
		// later hop is a Location we just received and have not yet trusted.
		if err := f.Policy.Check(ctx, next); err != nil {
			return nil, fmt.Errorf("hop %d: %w", hop, err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		loc := resp.Header.Get("Location")
		status := resp.StatusCode
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRecipeBytes))
		resp.Body.Close()

		if status >= 300 && status <= 399 {
			if loc == "" {
				return nil, fmt.Errorf("hop %d: %d with no Location", hop, status)
			}
			target, perr := next.Parse(loc) // resolves relative Locations
			if perr != nil {
				return nil, fmt.Errorf("hop %d: parse Location: %w", hop, perr)
			}
			next = target
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read body: %w", readErr)
		}
		if status != http.StatusOK {
			// The upstream body is not echoed: it is attacker-influenced and
			// this host's PTY capture is unredacted.
			return nil, fmt.Errorf("fetch recipe: HTTP %d", status)
		}
		return body, nil
	}
	return nil, fmt.Errorf("too many redirects (max %d)", maxRedirectHops)
}
