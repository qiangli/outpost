package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/overlaykey"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

// The auth key used throughout. Distinctive so a leak into any rendered
// string is unambiguous.
const testAuthKey = "tskey-auth-LEAKCANARY-doNotLog"

func TestPeerOverlayWanted(t *testing.T) {
	peer := &conf.ClusterConfig{JoinEndpoint: "peer.local:7000"}
	cloud := &conf.ClusterConfig{}

	tests := []struct {
		name  string
		cc    *conf.ClusterConfig
		token string
		want  bool
	}{
		{"peer plane and paired fetches", peer, "cb-token", true},
		// Cloudbox is the only party that can mint a key, and an
		// unpaired host has nothing to ask with.
		{"peer plane but unpaired does not", peer, "", false},
		{"peer plane with a blank token does not", peer, "   ", false},
		// The cloudbox-hosted plane already carries its overlay trio
		// through pairing; re-fetching would burn a key it does not need.
		{"cloudbox plane never fetches here", cloud, "cb-token", false},
		{"no cluster config", nil, "cb-token", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peerOverlayWanted(tt.cc, tt.token); got != tt.want {
				t.Fatalf("peerOverlayWanted = %t, want %t", got, tt.want)
			}
		})
	}
}

func overlayServer(t *testing.T, status int, body string) *overlaykey.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/overlay/authkey" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cb-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &overlaykey.Client{BaseURL: srv.URL, AccessToken: "cb-token", AgentName: "node-a"}
}

// The load-bearing assertion of deliverable (a): LoginServer and AuthKey
// are taken, PodCIDR is DROPPED. Honoring cloudbox's carve on a peer plane
// would hand the entrypoint a CIDR to template a conflist from and
// reintroduce the allocator the ADR removed.
func TestApplyPeerOverlayCredentialsIgnoresPodCIDR(t *testing.T) {
	cl := overlayServer(t, http.StatusOK, `{
	  "overlay_login_server": "https://hs.example",
	  "overlay_auth_key": "`+testAuthKey+`",
	  "overlay_pod_cidr": "10.42.9.0/24",
	  "expires_in_seconds": 300
	}`)

	// Seeded with a stale cloudbox carve, as a host that switched planes
	// mid-life would be.
	opts := runtime.Options{PeerFlannel: true, PodCIDR: "10.42.1.0/24"}
	if err := applyPeerOverlayCredentials(context.Background(), cl, &opts); err != nil {
		t.Fatalf("applyPeerOverlayCredentials: %v", err)
	}
	if opts.OverlayLoginServer != "https://hs.example" {
		t.Errorf("OverlayLoginServer = %q", opts.OverlayLoginServer)
	}
	if opts.OverlayAuthKey != testAuthKey {
		t.Errorf("OverlayAuthKey not applied")
	}
	if opts.PodCIDR != "" {
		t.Errorf("PodCIDR = %q, want empty — cloudbox's carve has no authority over a peer plane", opts.PodCIDR)
	}
	// And the classification stays peer-flannel, so no conflist is written.
	if got := opts.PodNetwork().Mode; got != runtime.PodNetworkPeerFlannel {
		t.Errorf("mode = %q, want %q", got, runtime.PodNetworkPeerFlannel)
	}
}

// A failure must be distinguishable: "the feature is off" and "you asked
// too soon" call for different operator actions, and a caller that cannot
// tell them apart retries forever against something that will never answer.
func TestApplyPeerOverlayCredentialsErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"overlay disabled", http.StatusServiceUnavailable, `{}`, overlaykey.ErrOverlayDisabled},
		{"throttled", http.StatusTooManyRequests, `{}`, overlaykey.ErrThrottled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := overlayServer(t, tt.status, tt.body)
			opts := runtime.Options{PeerFlannel: true, PodCIDR: "10.42.1.0/24"}
			err := applyPeerOverlayCredentials(context.Background(), cl, &opts)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			// Nothing half-applied: a failed fetch must not leave the
			// options in a state that reads like a partial success.
			if opts.OverlayAuthKey != "" || opts.OverlayLoginServer != "" {
				t.Errorf("credentials half-applied on error")
			}
			if opts.PodCIDR != "10.42.1.0/24" {
				t.Errorf("PodCIDR mutated on error: %q", opts.PodCIDR)
			}
		})
	}
}

// The gate that keeps a dark node off the cluster: a failed credential
// fetch must stop the caller BEFORE runtime.Up, not merely log on the way
// past it. peer-flannel binds --flannel-iface=tailscale0, so a node started
// without a tailnet identity joins, goes Ready, and blackholes every pod
// packet — invisible from the cluster's side.
//
// preparePeerOverlay is the whole decision, so asserting on it is what makes
// a regression (log-and-continue) a test failure rather than a field report.
func TestPreparePeerOverlayGatesRuntimeStart(t *testing.T) {
	// Keep the ERROR lines out of the test output; the log CONTENT is
	// covered by TestPeerOverlayKeyNeverRendered.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Run("fetch success proceeds with the key applied", func(t *testing.T) {
		cl := overlayServer(t, http.StatusOK, `{
		  "overlay_login_server": "https://hs.example",
		  "overlay_auth_key": "`+testAuthKey+`",
		  "expires_in_seconds": 300
		}`)
		opts := runtime.Options{PeerFlannel: true}
		if !preparePeerOverlay(context.Background(), cl, &opts, "node-a") {
			t.Fatal("proceed = false on a successful fetch")
		}
		if opts.OverlayAuthKey != testAuthKey {
			t.Error("auth key not applied to the runtime options")
		}
		if opts.OverlayLoginServer != "https://hs.example" {
			t.Errorf("OverlayLoginServer = %q", opts.OverlayLoginServer)
		}
	})

	// Every failure shape stops the start. ErrOverlayDisabled is the one
	// that could argue for continuing (cloudbox runs no overlay, so there
	// is no key to wait for) — decided explicitly against, because the node
	// still cannot satisfy the peer-flannel config it is holding, and a
	// present-but-broken node is harder to diagnose than an absent one.
	stops := []struct {
		name   string
		status int
	}{
		{"overlay disabled", http.StatusServiceUnavailable},
		{"throttled", http.StatusTooManyRequests},
		{"cloudbox 500", http.StatusInternalServerError},
	}
	for _, tt := range stops {
		t.Run(tt.name+" does not proceed", func(t *testing.T) {
			cl := overlayServer(t, tt.status, `{}`)
			opts := runtime.Options{PeerFlannel: true}
			if preparePeerOverlay(context.Background(), cl, &opts, "node-a") {
				t.Fatal("proceed = true after a failed fetch — the daemon would start a peer-flannel node with no tailnet identity")
			}
			if opts.OverlayAuthKey != "" {
				t.Error("auth key set despite a failed fetch")
			}
		})
	}

	// A transient outage must cost one boot, not wedge the node: the
	// decision keeps no state, so the next call re-fetches and proceeds.
	t.Run("recovers on a later attempt", func(t *testing.T) {
		down := overlayServer(t, http.StatusInternalServerError, `{}`)
		opts := runtime.Options{PeerFlannel: true}
		if preparePeerOverlay(context.Background(), down, &opts, "node-a") {
			t.Fatal("proceed = true while cloudbox was down")
		}
		up := overlayServer(t, http.StatusOK,
			`{"overlay_login_server":"https://hs.example","overlay_auth_key":"`+testAuthKey+`"}`)
		if !preparePeerOverlay(context.Background(), up, &opts, "node-a") {
			t.Fatal("proceed = false after cloudbox recovered")
		}
	})
}

// The key is a standing credential to join the tailnet, which is what
// reaches the pod network. It must never reach a log file, a status view,
// or an error string.
func TestPeerOverlayKeyNeverRendered(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Success path logs presence only.
	logPeerOverlayFetch("node-a", true, nil)
	if !strings.Contains(buf.String(), "has_auth_key=true") {
		t.Errorf("success log does not report key presence: %s", buf.String())
	}

	// Every failure path.
	for _, err := range []error{
		overlaykey.ErrOverlayDisabled,
		overlaykey.ErrThrottled,
		errors.New("overlaykey: cloudbox returned 500"),
	} {
		logPeerOverlayFetch("node-a", false, err)
	}
	if strings.Contains(buf.String(), testAuthKey) {
		t.Fatalf("auth key leaked into the log stream: %s", buf.String())
	}

	// The error a real fetch produces must not carry the key either —
	// a 200-with-a-key that fails to decode is the closest case.
	cl := overlayServer(t, http.StatusOK, `{"overlay_auth_key": "`+testAuthKey+`"`) // truncated JSON
	opts := runtime.Options{PeerFlannel: true}
	err := applyPeerOverlayCredentials(context.Background(), cl, &opts)
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if strings.Contains(err.Error(), testAuthKey) {
		t.Fatalf("auth key leaked into an error string: %v", err)
	}
}
