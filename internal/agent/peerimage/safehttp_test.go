package peerimage

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// hostMapTransport rewrites request hosts so a test can hand the Fetcher URLs
// with FAKE public hostnames (which the policy's resolver treats as routable)
// while the actual dial lands on a real loopback httptest server. This is what
// lets the redirect tests run offline: the URL's identity (what the policy
// judges) is decoupled from the dial (what the transport reaches).
type hostMapTransport struct {
	m map[string]string // fake host[:port] -> real loopback host:port
}

func (t hostMapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	real, ok := t.m[req.URL.Host]
	if !ok {
		return nil, &net.OpError{Op: "dial", Err: errors.New("no route (test transport)")}
	}
	r := req.Clone(req.Context())
	r.URL.Host = real
	r.Host = real
	return http.DefaultTransport.RoundTrip(r)
}

// fixedResolver returns a DNS answer per host: public IPs for the fake peers,
// loopback for anything named in loopbackHosts.
func fixedResolver(public map[string]string, loopbackHosts ...string) func(context.Context, string) ([]net.IP, error) {
	loop := map[string]bool{}
	for _, h := range loopbackHosts {
		loop[h] = true
	}
	return func(_ context.Context, host string) ([]net.IP, error) {
		if loop[host] {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		if ip, ok := public[host]; ok {
			return []net.IP{net.ParseIP(ip)}, nil
		}
		return nil, &net.DNSError{IsNotFound: true, Name: host}
	}
}

func TestAddrPolicy_Check(t *testing.T) {
	ctx := context.Background()
	public := map[string]string{"peer-a.test": "203.0.113.10"} // TEST-NET-3
	resolve := fixedResolver(public, "loop.test")

	mk := func(rawurl string) *url.URL {
		u, err := url.Parse(rawurl)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	// A plain public peer is fine.
	p := AddrPolicy{Resolve: resolve}
	if err := p.Check(ctx, mk("http://peer-a.test/recipes/x")); err != nil {
		t.Fatalf("public peer refused: %v", err)
	}

	// Loopback is refused unless it is the EXACT allowed forward address.
	if err := p.Check(ctx, mk("http://loop.test:9999/")); !errors.Is(err, ErrLoopbackTarget) {
		t.Fatalf("loopback = %v, want ErrLoopbackTarget", err)
	}
	p2 := Allow("loop.test:9999")
	p2.Resolve = resolve
	if err := p2.Check(ctx, mk("http://loop.test:9999/")); err != nil {
		t.Fatalf("the allowed forward address was refused: %v", err)
	}
	// ... but a DIFFERENT loopback port is not covered by that allowance.
	if err := p2.Check(ctx, mk("http://loop.test:17777/")); !errors.Is(err, ErrLoopbackTarget) {
		t.Fatalf("a non-allowlisted loopback port = %v, want ErrLoopbackTarget", err)
	}

	// The daemon's own admin surface, by literal IP.
	if err := p.Check(ctx, mk("http://127.0.0.1:17777/mcp/")); !errors.Is(err, ErrLoopbackTarget) {
		t.Fatalf("literal loopback = %v, want ErrLoopbackTarget", err)
	}
	// Unspecified (0.0.0.0 reaches loopback on most stacks).
	if err := p.Check(ctx, mk("http://0.0.0.0:9999/")); !errors.Is(err, ErrLoopbackTarget) {
		t.Fatalf("unspecified = %v, want ErrLoopbackTarget", err)
	}
	// Link-local (cloud metadata).
	if err := p.Check(ctx, mk("http://169.254.169.254/latest/meta-data")); !errors.Is(err, ErrLoopbackTarget) {
		t.Fatalf("link-local = %v, want ErrLoopbackTarget", err)
	}
	// Non-http(s) schemes are not fetchable.
	if err := p.Check(ctx, mk("file:///etc/passwd")); err == nil {
		t.Fatal("file:// was accepted")
	}
	// A host that resolves to NOTHING is refused — never "allow because we
	// learned nothing".
	empty := AddrPolicy{Resolve: func(context.Context, string) ([]net.IP, error) { return nil, nil }}
	if err := empty.Check(ctx, mk("http://peer-a.test/")); err == nil {
		t.Fatal("an unresolvable host was allowed")
	}
}

// redirectServer serves /start → 302 to the next URL in the chain.
func redirectServer(t *testing.T, target string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, target, http.StatusFound)
	}))
}

// THE requirement-5 test: a redirect chain where only the LAST hop targets
// loopback. A first-hop-only check passes this — that is the common mistake.
func TestFetcher_MultiHopEndingAtLoopbackRefused(t *testing.T) {
	var hitsA, hitsB, hitsAdmin atomic.Int32

	// The "admin surface" the chain tries to reach. It must NEVER be hit.
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsAdmin.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer admin.Close()
	adminHost := strings.TrimPrefix(admin.URL, "http://")

	// hop1 (fake public) → hop2 (fake public) → admin (loopback).
	hop2 := redirectServer(t, "http://internal-admin.test/", &hitsB)
	defer hop2.Close()
	hop1 := redirectServer(t, "http://hop2.test/", &hitsA)
	defer hop1.Close()

	realMap := map[string]string{
		"hop1.test": strings.TrimPrefix(hop1.URL, "http://"),
		"hop2.test": strings.TrimPrefix(hop2.URL, "http://"),
	}
	// Policy sees hop1/hop2 as public; internal-admin.test resolves to loopback.
	public := map[string]string{"hop1.test": "203.0.113.1", "hop2.test": "203.0.113.2"}
	policy := Allow(adminHost) // the admin port IS an allowed forward... but not the TARGET
	policy.Resolve = fixedResolver(public, "internal-admin.test")
	f := &Fetcher{Policy: policy, Client: &http.Client{Transport: hostMapTransport{m: realMap}}}

	_, err := f.Get(context.Background(), "http://hop1.test/start")
	if !errors.Is(err, ErrLoopbackTarget) {
		t.Fatalf("multi-hop to loopback = %v, want ErrLoopbackTarget", err)
	}
	if hitsA.Load() == 0 || hitsB.Load() == 0 {
		t.Fatalf("the chain did not traverse the public hops (A=%d B=%d)", hitsA.Load(), hitsB.Load())
	}
	if hitsAdmin.Load() != 0 {
		t.Fatal("the loopback admin surface was reached through the redirect chain")
	}
}

// The allowed forward address itself is fetchable — the legitimate path.
func TestFetcher_AllowedForwardFetches(t *testing.T) {
	body := "name: demo\nlocal_ref: localhost/cluster/demo\ncontext_type: local\ncontext_path: /x\ndockerfile: Dockerfile\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	f := NewFetcher(Allow(addr))
	got, err := f.Get(context.Background(), srv.URL+"/recipes/demo")
	if err != nil {
		t.Fatalf("allowed forward fetch: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body drifted: %q", got)
	}
}

// A redirect OFF the allowed forward address — even to another loopback port —
// is refused at that hop.
func TestFetcher_RedirectOffTheAllowanceRefused(t *testing.T) {
	var hitsOther atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsOther.Add(1)
		_, _ = w.Write([]byte("secret"))
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	f := NewFetcher(Allow(addr)) // only the FIRST server is allowed
	_, err := f.Get(context.Background(), srv.URL)
	if !errors.Is(err, ErrLoopbackTarget) {
		t.Fatalf("redirect off the allowance = %v, want ErrLoopbackTarget", err)
	}
	if hitsOther.Load() != 0 {
		t.Fatal("the off-allowance loopback target was reached")
	}
}

// A redirect cycle terminates rather than spinning.
func TestFetcher_RedirectCycleBounded(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL, http.StatusFound)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	f := NewFetcher(Allow(addr))
	if _, err := f.Get(context.Background(), srv.URL); err == nil ||
		!strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("redirect cycle = %v, want a bounded-redirects error", err)
	}
}

// A non-2xx terminal status is an error, and the (attacker-influenced) body is
// not echoed into it.
func TestFetcher_Non200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "teapot-body-that-must-not-leak", http.StatusTeapot)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	f := NewFetcher(Allow(addr))
	_, err := f.Get(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "418") {
		t.Fatalf("non-200 = %v, want an HTTP-status error", err)
	}
	if strings.Contains(err.Error(), "teapot-body") {
		t.Fatal("the upstream body was echoed into the error")
	}
}

// A redirect with no Location cannot be followed.
func TestFetcher_RedirectWithoutLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound) // no Location header
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	f := NewFetcher(Allow(addr))
	if _, err := f.Get(context.Background(), srv.URL); err == nil ||
		!strings.Contains(err.Error(), "no Location") {
		t.Fatalf("locationless redirect = %v, want a no-Location error", err)
	}
}

// The stdlib must never follow a redirect on its own: even a caller-supplied
// Client has its CheckRedirect overridden by the Fetcher.
func TestFetcher_NeverLetsStdlibFollow(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("reached"))
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	// Both on the allowance — so the ONLY thing stopping the second hop is the
	// Fetcher driving redirects itself. If the stdlib followed silently, the
	// policy would never see hop 2; here it does (and allows it), which proves
	// every hop passes through Check.
	addr1 := strings.TrimPrefix(srv.URL, "http://")
	addr2 := strings.TrimPrefix(target.URL, "http://")
	f := NewFetcher(Allow(addr1, addr2))
	got, err := f.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("two-hop allowed fetch: %v", err)
	}
	if string(got) != "reached" || hits.Load() != 1 {
		t.Fatalf("manual follow failed: body=%q hits=%d", got, hits.Load())
	}
}
