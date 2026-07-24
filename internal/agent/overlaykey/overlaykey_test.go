package overlaykey

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		state string
		want  bool
	}{
		{"Running", true},
		{"NeedsLogin", false},
		{"Stopped", false},
		{"NoState", false},
	} {
		r := &Refresher{Exec: func(ctx context.Context, args ...string) ([]byte, error) {
			return []byte(`{"BackendState":"` + c.state + `"}`), nil
		}}
		got, err := r.Healthy(context.Background())
		if err != nil {
			t.Fatalf("state %s: %v", c.state, err)
		}
		if got != c.want {
			t.Errorf("BackendState %q -> healthy=%v, want %v", c.state, got, c.want)
		}
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
