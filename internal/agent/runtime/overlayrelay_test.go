package runtime

import (
	"strings"
	"testing"
)

// The relay renders NOTHING unless both endpoint and secret are present —
// the cloudbox-hosted plane and the single-node fallback must keep
// producing a byte-identical container command to the pre-relay one, and a
// half-configured relay must surface in the daemon's fail-closed check
// rather than as an frp auth error inside the container.
func TestOverlayRelayEnvArgs(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want []string
	}{
		{
			name: "inactive on the cloudbox plane",
			opts: Options{PodCIDR: "10.42.7.0/24", OverlayLoginServer: "https://cb", OverlayAuthKey: "k"},
			want: nil,
		},
		{
			name: "host without secret stays inactive",
			opts: Options{OverlayRelayHost: "ai.example.io"},
			want: nil,
		},
		{
			name: "secret without host stays inactive",
			opts: Options{OverlayRelaySecret: "s3cret"},
			want: nil,
		},
		{
			name: "active relay renders the full env set",
			opts: Options{
				OverlayRelayHost:     "ai.example.io",
				OverlayRelayPort:     443,
				OverlayRelayProtocol: "wss",
				OverlayRelayToken:    "matrix-token",
				OverlayRelaySecret:   "s3cret",
				OverlayRelayUser:     "cloudbox",
			},
			want: []string{
				"-e", "OUTPOST_OVERLAY_RELAY_HOST=ai.example.io",
				"-e", "OUTPOST_OVERLAY_RELAY_SECRET=s3cret",
				"-e", "OUTPOST_OVERLAY_RELAY_PORT=443",
				"-e", "OUTPOST_OVERLAY_RELAY_PROTOCOL=wss",
				"-e", "OUTPOST_OVERLAY_RELAY_TOKEN=matrix-token",
				"-e", "OUTPOST_OVERLAY_RELAY_USER=cloudbox",
			},
		},
		{
			// Prod cloudbox often runs with an empty MATRIX_TOKEN; the token
			// env is still stamped (empty) so the entrypoint renders the same
			// [auth] shape the main config uses.
			name: "empty token is stamped, optional fields defaulted in-container",
			opts: Options{
				OverlayRelayHost:   "ai.example.io",
				OverlayRelaySecret: "s3cret",
			},
			want: []string{
				"-e", "OUTPOST_OVERLAY_RELAY_HOST=ai.example.io",
				"-e", "OUTPOST_OVERLAY_RELAY_SECRET=s3cret",
				"-e", "OUTPOST_OVERLAY_RELAY_TOKEN=",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := overlayRelayEnvArgs(tt.opts)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("overlayRelayEnvArgs = %q, want %q", got, tt.want)
			}
		})
	}
}

// A rotated relay secret (or a moved cloudbox) must recreate the
// container: the old frpc session would keep dialing with dead
// credentials while the container reads as healthy.
func TestRuntimeFingerprintCoversOverlayRelay(t *testing.T) {
	base := Options{AgentName: "n", NodeToken: "t", PodmanBin: "definitely-not-a-binary"}
	fp := func(o Options) string {
		s, err := runtimeFingerprint(t.Context(), "false", "img", o)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return s
	}
	plain := fp(base)
	withRelay := base
	withRelay.OverlayRelayHost = "ai.example.io"
	withRelay.OverlayRelaySecret = "s3cret"
	if fp(withRelay) == plain {
		t.Error("adding the relay did not change the fingerprint")
	}
	rotated := withRelay
	rotated.OverlayRelaySecret = "rotated"
	if fp(rotated) == fp(withRelay) {
		t.Error("rotating the relay secret did not change the fingerprint")
	}
}

// The entrypoint's relay half, pinned property by property. Each is a
// silent failure if it regresses: a visitor appended to the MAIN config on
// a peer plane binds its port against a server that publishes no
// overlay-control (every ts2021 connect then dies quietly), and a missing
// second supervisor means one cloudbox blip permanently strands the relay.
func TestEntrypointOverlayRelayBranch(t *testing.T) {
	script := embeddedEntrypoint(t)

	// Activation matches runtime.Options.OverlayRelayActive: host AND secret.
	if !strings.Contains(script, `if [ -n "${OUTPOST_OVERLAY_RELAY_HOST}" ] && [ -n "${OUTPOST_OVERLAY_RELAY_SECRET}" ]; then`) {
		t.Fatal("entrypoint does not gate the relay on HOST and SECRET both present")
	}

	relayAt := strings.Index(script, `cat > /tmp/frpc-overlay.toml`)
	if relayAt < 0 {
		t.Fatal("entrypoint writes no /tmp/frpc-overlay.toml relay config")
	}
	elifAt := strings.Index(script[relayAt:], `elif [ -n "${OUTPOST_OVERLAY_AUTHKEY}" ]; then`)
	if elifAt < 0 {
		t.Fatal("the relay branch and the main-config visitor branch are not mutually exclusive; " +
			"a peer plane would also append the visitor to the main session, which dials a server " +
			"that publishes no overlay-control")
	}
	relay := script[relayAt : relayAt+elifAt]

	for _, want := range []string{
		`serverAddr = "${OUTPOST_OVERLAY_RELAY_HOST}"`,
		`serverPort = ${OUTPOST_OVERLAY_RELAY_PORT}`,
		`protocol = "${OUTPOST_OVERLAY_RELAY_PROTOCOL}"`,
		`token = "${OUTPOST_OVERLAY_RELAY_TOKEN}"`,
		`serverUser = "${OUTPOST_OVERLAY_RELAY_USER}"`,
		`serverName = "overlay-control"`,
		`secretKey = "${OUTPOST_OVERLAY_RELAY_SECRET}"`,
		`bindPort = ${OVERLAY_CONTROL_BACKEND_PORT}`,
		// A cloudbox deploy is exactly when every outpost reconnects.
		`loginFailExit = false`,
	} {
		if !strings.Contains(relay, want) {
			t.Errorf("relay config lost %q", want)
		}
	}

	// Defaults reproduce the cloudbox pairing shape.
	for _, want := range []string{
		`: "${OUTPOST_OVERLAY_RELAY_PORT:=443}"`,
		`: "${OUTPOST_OVERLAY_RELAY_PROTOCOL:=wss}"`,
		`: "${OUTPOST_OVERLAY_RELAY_USER:=cloudbox}"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("relay default lost: %q", want)
		}
	}

	// The relay session gets its own supervisor, probing ONLY the backend
	// port, and the MAIN session's required ports include that port ONLY
	// when the visitor actually rides the main config — otherwise a
	// cloudbox outage restarts a healthy apiserver tunnel (and vice versa).
	if !strings.Contains(script, `FRPC_REQUIRED_PORTS="${OVERLAY_CONTROL_BACKEND_PORT}" \
    FRPC_CONFIG=/tmp/frpc-overlay.toml \`) {
		t.Error("no dedicated supervisor for the relay session")
	}
	if !strings.Contains(script, `if [ -n "${OUTPOST_OVERLAY_AUTHKEY}" ] && [ "${OVERLAY_RELAY_ACTIVE}" != "1" ]; then`) {
		t.Error("main FRPC_REQUIRED_PORTS is not gated on the visitor riding the main session")
	}
}

// The cloudbox-hosted plane's overlay path is untouched: with no relay env
// set, the visitor still rides the MAIN config exactly as it shipped.
func TestEntrypointCloudboxOverlayPathUnchangedByRelay(t *testing.T) {
	script := embeddedEntrypoint(t)
	for _, want := range []string{
		`serverUser = "${OUTPOST_FRP_SERVER_USER}"`,
		"cat >> /tmp/frpc.toml",
		`secretKey = "${OUTPOST_STCP_SECRET}"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("cloudbox overlay path lost %q", want)
		}
	}
	// The socat TLS terminator and the tailscale-up block are shared by
	// both paths; both dial the same loopback backend port.
	if !strings.Contains(script, `LOGIN_SERVER="https://127.0.0.1:${OVERLAY_CONTROL_PORT}"`) {
		t.Error("the tailscale login server no longer rides the loopback TLS terminator")
	}
}
