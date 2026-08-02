package runtime

import (
	"fmt"
	"strings"
)

// Env vars carrying the overlay-relay session (see Options.OverlayRelay*)
// into the container. Wire contract with image/entrypoint.sh — the
// entrypoint activates the relay on HOST and SECRET both being non-empty
// and defaults the rest.
const (
	overlayRelayHostEnv     = "OUTPOST_OVERLAY_RELAY_HOST"
	overlayRelayPortEnv     = "OUTPOST_OVERLAY_RELAY_PORT"
	overlayRelayProtocolEnv = "OUTPOST_OVERLAY_RELAY_PROTOCOL"
	overlayRelayTokenEnv    = "OUTPOST_OVERLAY_RELAY_TOKEN"
	overlayRelaySecretEnv   = "OUTPOST_OVERLAY_RELAY_SECRET"
	overlayRelayUserEnv     = "OUTPOST_OVERLAY_RELAY_USER"
)

// OverlayRelayActive reports whether these Options carry a usable relay:
// both the endpoint and the visitor secret must be present, matching the
// entrypoint's own activation test. A half-configured relay (host with no
// secret, or vice versa) is treated as absent so the failure surfaces in
// the daemon's fail-closed check rather than as an frp auth error inside
// the container.
func (o Options) OverlayRelayActive() bool {
	return strings.TrimSpace(o.OverlayRelayHost) != "" &&
		strings.TrimSpace(o.OverlayRelaySecret) != ""
}

// overlayRelayEnvArgs renders the `-e` pairs for the relay session.
//
// Returns NOTHING when the relay is inactive, on purpose: the
// cloudbox-hosted plane and the single-node fallback must keep rendering a
// byte-identical container command to the one that shipped before the
// relay existed.
func overlayRelayEnvArgs(opts Options) []string {
	if !opts.OverlayRelayActive() {
		return nil
	}
	args := []string{
		"-e", overlayRelayHostEnv + "=" + strings.TrimSpace(opts.OverlayRelayHost),
		"-e", overlayRelaySecretEnv + "=" + strings.TrimSpace(opts.OverlayRelaySecret),
	}
	if opts.OverlayRelayPort != 0 {
		args = append(args, "-e", fmt.Sprintf("%s=%d", overlayRelayPortEnv, opts.OverlayRelayPort))
	}
	if p := strings.TrimSpace(opts.OverlayRelayProtocol); p != "" {
		args = append(args, "-e", overlayRelayProtocolEnv+"="+p)
	}
	// Token may legitimately be empty (prod cloudbox often runs with an
	// empty MATRIX_TOKEN); the env var is still stamped so the entrypoint
	// renders the same [auth] shape the main config uses.
	args = append(args, "-e", overlayRelayTokenEnv+"="+opts.OverlayRelayToken)
	if u := strings.TrimSpace(opts.OverlayRelayUser); u != "" {
		args = append(args, "-e", overlayRelayUserEnv+"="+u)
	}
	return args
}
