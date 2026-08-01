package overlaykey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(url string) *Client {
	return &Client{BaseURL: url, AccessToken: "tok", AgentName: "dragon"}
}

func TestFetchHappyPath(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(Credentials{
			LoginServer: "https://cb/overlay/headscale",
			AuthKey:     "tskey-abc",
			PodCIDR:     "10.42.7.0/24",
		})
	}))
	defer srv.Close()

	creds, err := testClient(srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if creds.AuthKey != "tskey-abc" || creds.PodCIDR != "10.42.7.0/24" {
		t.Errorf("unexpected creds: %+v", creds)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", gotAuth)
	}
	if !strings.Contains(gotBody, "dragon") {
		t.Errorf("request body %q did not carry agent_name", gotBody)
	}
}

// TestFetchRejectsEmptyKey: a 200 carrying no key is NOT success. Returning
// it would have the caller "re-register" with an empty credential and
// report progress it did not make.
func TestFetchRejectsEmptyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Credentials{LoginServer: "https://cb"})
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).Fetch(context.Background()); err == nil {
		t.Fatal("empty auth key was accepted; a 200 with no key is not success")
	}
}

// TestFetchDistinguishesDisabledAndThrottled: the caller behaves very
// differently for each — stop polling vs. wait and retry — so collapsing
// them into a generic error would make one of the two behaviours wrong.
func TestFetchDistinguishesDisabledAndThrottled(t *testing.T) {
	for _, c := range []struct {
		code int
		want error
	}{
		{http.StatusServiceUnavailable, ErrOverlayDisabled},
		{http.StatusTooManyRequests, ErrThrottled},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
		}))
		_, err := testClient(srv.URL).Fetch(context.Background())
		srv.Close()
		if !errors.Is(err, c.want) {
			t.Errorf("status %d gave %v, want %v", c.code, err, c.want)
		}
	}
}

func TestHealthyReadsBackendState(t *testing.T) {
	for _, c := range []struct {
		name   string
		status string
		want   bool
	}{
		{"running and in network map", `{"BackendState":"Running","Self":{"InNetworkMap":true}}`, true},
		{
			"running and cached in network map but coordination server unreachable",
			`{"BackendState":"Running","Self":{"Online":false,"InNetworkMap":true},"Health":["Unable to connect to the Tailscale coordination server to synchronize the state of your tailnet. Peer reachability may degrade."]}`,
			false,
		},
		{
			"running with unrelated health warning",
			`{"BackendState":"Running","Self":{"Online":false,"InNetworkMap":true},"Health":["Some peers are using an old client version."]}`,
			true,
		},
		// The stranded state a cloudbox deploy leaves behind: tailscaled still
		// reports Running from cached state, but the node is in no netmap it
		// polled (every /machine/map is a 404 "node not found"). A Running-only
		// check called this healthy and left the node off the pod network until
		// a human restarted it — the whole reason this field is now read.
		{"running but stranded (not in network map)", `{"BackendState":"Running","Self":{"InNetworkMap":false}}`, false},
		{"running but no self", `{"BackendState":"Running"}`, false},
		{"needs login", `{"BackendState":"NeedsLogin","Self":{"InNetworkMap":false}}`, false},
		{"stopped", `{"BackendState":"Stopped"}`, false},
		{"no state", `{"BackendState":"NoState"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &Refresher{Exec: func(ctx context.Context, args ...string) ([]byte, error) {
				return []byte(c.status), nil
			}}
			got, err := r.Healthy(context.Background())
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got != c.want {
				t.Errorf("%s -> healthy=%v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestRunRetriesUntilOverlayControlListenerReturns(t *testing.T) {
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetch := fetches.Add(1)
		_ = json.NewEncoder(w).Encode(Credentials{
			LoginServer: "https://cb/overlay/headscale",
			AuthKey:     fmt.Sprintf("tskey-%d", fetch),
			PodCIDR:     "10.42.1.0/24",
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ups []string
	statusCalls := 0
	r := &Refresher{
		Client:     testClient(srv.URL),
		Interval:   time.Millisecond,
		ControlURL: "https://127.0.0.1:8091",
		Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 1 && args[1] == "status" {
				statusCalls++
				if statusCalls >= 4 {
					cancel()
					return []byte(`{"BackendState":"Running","Self":{"Online":true,"InNetworkMap":true}}`), nil
				}
				return []byte(`{"BackendState":"Running","Self":{"Online":false,"InNetworkMap":true},"Health":["Unable to connect to the Tailscale coordination server to synchronize the state of your tailnet."]}`), nil
			}
			joined := strings.Join(args, " ")
			ups = append(ups, joined)
			if len(ups) == 1 {
				return []byte("dial tcp 127.0.0.1:8091: connect: connection refused"), errors.New("listener unavailable")
			}
			return nil, nil
		},
	}
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("authkey fetches = %d, want 2", got)
	}
	if len(ups) != 2 {
		t.Fatalf("tailscale up calls = %d, want 2: %v", len(ups), ups)
	}
	if !strings.Contains(ups[1], "--authkey=tskey-2") || !strings.Contains(ups[1], "--login-server=https://127.0.0.1:8091") {
		t.Fatalf("second heal did not use fresh key and loopback control URL: %q", ups[1])
	}
}

func TestRunDoesNotMintKeysWhenStatusIsUnknown(t *testing.T) {
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	r := &Refresher{
		Client:   testClient(srv.URL),
		Interval: time.Millisecond,
		Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			calls++
			if calls >= 3 {
				cancel()
			}
			return []byte("{not-json"), nil
		},
	}
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fetches.Load(); got != 0 {
		t.Fatalf("authkey fetches = %d, want 0 for unknown tailscale status", got)
	}
}

// TestHealthyErrorIsNotUnhealthy pins the distinction that keeps the
// refresher from acting on absence of evidence: if we cannot ASK (no
// container, no tailscale binary, overlay simply off), that is unknown —
// not "broken". Treating it as broken would mint keys and run `tailscale
// up` against hosts that never wanted an overlay.
func TestHealthyErrorIsNotUnhealthy(t *testing.T) {
	r := &Refresher{Exec: func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, errors.New("no such container")
	}}
	healthy, err := r.Healthy(context.Background())
	if err == nil {
		t.Fatal("exec failure must surface as an error, not a verdict")
	}
	if healthy {
		t.Error("healthy must be false alongside the error")
	}
}

// TestHealAdvertisesRoutes: a control-plane reset drops approved routes
// with everything else, so a node that rejoins without re-advertising is
// on the tailnet but carries no pod traffic — healthy-looking and useless.
func TestHealAdvertisesRoutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Credentials{
			LoginServer: "https://cb/overlay/headscale",
			AuthKey:     "tskey-xyz",
			PodCIDR:     "10.42.9.0/24",
		})
	}))
	defer srv.Close()

	var got []string
	r := &Refresher{
		Client: testClient(srv.URL),
		Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			// Heal verifies registration after `up`, so the status probe
			// has to answer too — see TestHealRejectsExitZeroWithoutRegistration.
			if len(args) > 1 && args[1] == "status" {
				return []byte(`{"BackendState":"Running","Self":{"InNetworkMap":true}}`), nil
			}
			got = args
			return nil, nil
		},
	}
	if err := r.Heal(context.Background()); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"tailscale up",
		"--authkey=tskey-xyz",
		"--login-server=https://cb/overlay/headscale",
		"--advertise-routes=10.42.9.0/24",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("tailscale up args %q missing %q", joined, want)
		}
	}
}

// TestHealFallsBackToConfiguredPodCIDR: if cloudbox omits the CIDR (an
// older build, say), the locally-known one must still be advertised rather
// than silently dropped.
func TestHealFallsBackToConfiguredPodCIDR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Credentials{LoginServer: "https://cb", AuthKey: "k"})
	}))
	defer srv.Close()

	var got []string
	r := &Refresher{
		Client:  testClient(srv.URL),
		PodCIDR: "10.42.3.0/24",
		Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 1 && args[1] == "status" {
				return []byte(`{"BackendState":"Running","Self":{"InNetworkMap":true}}`), nil
			}
			got = args
			return nil, nil
		},
	}
	if err := r.Heal(context.Background()); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if !strings.Contains(strings.Join(got, " "), "--advertise-routes=10.42.3.0/24") {
		t.Errorf("configured pod CIDR was not advertised: %v", got)
	}
}

// TestHealIgnoresCredentialPodCIDRForPeerFlannel pins the peer-flannel
// safety rule: IgnoreCredentialPodCIDR=true must drop a fetched
// Credentials.PodCIDR entirely, even though Heal just fetched one. Under
// peer-flannel the pod CIDR comes from the peer's k3s controller-manager
// via Node.spec.podCIDR — cloudbox's cluster carve has no authority there,
// and advertising it would reintroduce the competing allocator
// docs/adr-peer-dks-pod-network.md removed.
func TestHealIgnoresCredentialPodCIDRForPeerFlannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Credentials{
			LoginServer: "https://cb/overlay/headscale",
			AuthKey:     "tskey-peer",
			PodCIDR:     "10.99.0.0/24", // cloudbox's cloudbox-cluster carve
		})
	}))
	defer srv.Close()

	var got []string
	r := &Refresher{
		Client:                  testClient(srv.URL),
		IgnoreCredentialPodCIDR: true,
		Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 1 && args[1] == "status" {
				return []byte(`{"BackendState":"Running","Self":{"InNetworkMap":true}}`), nil
			}
			got = args
			return nil, nil
		},
	}
	if err := r.Heal(context.Background()); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--advertise-routes") {
		t.Errorf("peer-flannel heal must never advertise routes, got: %q", joined)
	}
	if strings.Contains(joined, "10.99.0.0/24") {
		t.Errorf("fetched cloudbox-cluster PodCIDR leaked into tailscale up args: %q", joined)
	}
}

// TestHealIgnoreCredentialPodCIDRStillHonorsConfiguredPodCIDR: the option
// only blocks the FETCHED credential's PodCIDR, not r.PodCIDR itself — a
// caller that explicitly configures one (today: never, for peer-flannel)
// must still see it advertised.
func TestHealIgnoreCredentialPodCIDRStillHonorsConfiguredPodCIDR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Credentials{
			LoginServer: "https://cb/overlay/headscale",
			AuthKey:     "tskey-peer",
			PodCIDR:     "10.99.0.0/24", // must be ignored
		})
	}))
	defer srv.Close()

	var got []string
	r := &Refresher{
		Client:                  testClient(srv.URL),
		PodCIDR:                 "10.42.5.0/24",
		IgnoreCredentialPodCIDR: true,
		Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 1 && args[1] == "status" {
				return []byte(`{"BackendState":"Running","Self":{"InNetworkMap":true}}`), nil
			}
			got = args
			return nil, nil
		},
	}
	if err := r.Heal(context.Background()); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--advertise-routes=10.42.5.0/24") {
		t.Errorf("configured PodCIDR was not advertised: %q", joined)
	}
	if strings.Contains(joined, "10.99.0.0/24") {
		t.Errorf("fetched cloudbox-cluster PodCIDR leaked into tailscale up args: %q", joined)
	}
}

// TestHealManagedModeRetainsCredentialPodCIDR is the byte-for-byte control:
// with IgnoreCredentialPodCIDR left at its zero value (false, the
// cloudbox-managed default), Heal must still prefer a fetched
// Credentials.PodCIDR exactly as before this option existed.
func TestHealManagedModeRetainsCredentialPodCIDR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Credentials{
			LoginServer: "https://cb/overlay/headscale",
			AuthKey:     "tskey-managed",
			PodCIDR:     "10.42.9.0/24",
		})
	}))
	defer srv.Close()

	var got []string
	r := &Refresher{
		Client:  testClient(srv.URL),
		PodCIDR: "10.42.1.0/24", // stale local value; the fetched one must win
		Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 1 && args[1] == "status" {
				return []byte(`{"BackendState":"Running","Self":{"InNetworkMap":true}}`), nil
			}
			got = args
			return nil, nil
		},
	}
	if err := r.Heal(context.Background()); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--advertise-routes=10.42.9.0/24") {
		t.Errorf("managed heal must retain the fetched PodCIDR, got: %q", joined)
	}
}

// TestHealRejectsExitZeroWithoutRegistration is the invariant that cost a
// live debugging session: `tailscale up` exits 0 while leaving the node
// logged out. Trusting that exit code makes the refresher report a
// successful re-registration on every tick while healing nothing —
// success asserted by the absence of a failure signal.
func TestHealRejectsExitZeroWithoutRegistration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Credentials{
			LoginServer: "https://cb/overlay/headscale",
			AuthKey:     "tskey-abc",
			PodCIDR:     "10.42.1.0/24",
		})
	}))
	defer srv.Close()

	r := &Refresher{
		Client: testClient(srv.URL),
		Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 1 && args[1] == "status" {
				// What a control plane that cannot complete registration
				// actually leaves behind.
				return []byte(`{"BackendState":"NeedsLogin"}`), nil
			}
			return nil, nil // `up` exits 0
		},
	}
	err := r.Heal(context.Background())
	if err == nil {
		t.Fatal("Heal reported success after `up` exited 0 without registering")
	}
	if !strings.Contains(err.Error(), "still not registered") {
		t.Errorf("error should name the real problem, got: %v", err)
	}
}
