// Package overlay supervises a `tailscaled` subprocess that connects
// to the cloudbox-embedded Headscale coordination plane for Phase 3
// cross-outpost pod networking.
//
// Linux-only (Tailscale's userspace + kernel WireGuard story is best
// on Linux; macOS/Windows builds return ErrUnsupported and the outpost
// keeps running without the overlay — vkpodman or single-node
// k3s-agent paths still work).
//
// Subprocess for MVP. Embedded `tsnet` is the official library form
// but is userspace-only and can't host a CNI plugin's TUN device.
// Embedding the full tailscaled internals (wgengine + ipn + ipnlocal
// + controlclient) is weeks of glue against a not-stable surface;
// running the upstream tailscaled binary is the pragmatic compromise.
// Post-MVP cleanup: replace with a proper embed.
package overlay

import "errors"

// ErrUnsupported is returned by Run on non-Linux platforms.
var ErrUnsupported = errors.New("overlay: tailscaled supervisor requires Linux")

// Options is the supervisor's input. All fields except ExtraArgs are
// required when LoginServer/AuthKey are non-empty.
type Options struct {
	// Binary is the path to `tailscaled`. Empty looks up "tailscaled"
	// on PATH. We require the upstream binary on the host — installed
	// via the standard installer (e.g. `curl -fsSL
	// https://tailscale.com/install.sh | sh`).
	Binary string

	// TailscaleBinary is the path to the `tailscale` CLI (separate
	// from tailscaled). Used for `tailscale up --login-server=... `
	// to authenticate the node after the daemon starts. Same
	// PATH-fallback as Binary.
	TailscaleBinary string

	// SocketPath is where tailscaled listens for the `tailscale` CLI
	// (--socket). Default /var/run/tailscale/tailscaled.sock. We pin
	// a per-outpost path under StateDir so multiple instances on the
	// same host (rare, but possible during testing) don't collide.
	SocketPath string

	// StateDir is where tailscaled persists its node state (machine
	// key, profile, current peer set). Defaults to
	// /var/lib/cloudbox/tailscale.
	StateDir string

	// LoginServer is the Headscale URL (cloudbox-mounted) tailscaled
	// authenticates against. Empty disables the overlay (Run returns
	// nil immediately).
	LoginServer string

	// AuthKey is the one-shot pre-auth key cloudbox minted at
	// Exchange time. Required when LoginServer is set; without it
	// tailscaled would block on interactive auth.
	AuthKey string

	// AdvertiseRoutes is the list of CIDRs this node advertises over
	// the overlay so other nodes can route to them. For Phase 3 this
	// is one entry: the per-outpost pod CIDR. Empty is OK (the node
	// joins but advertises nothing).
	AdvertiseRoutes []string

	// AcceptRoutes makes tailscaled install routes for CIDRs other
	// nodes have advertised. Required for cross-outpost pod-to-pod.
	// Default true (callers shouldn't have to think about it).
	// Pointer-bool so explicit false can be plumbed if needed.
	AcceptRoutes *bool

	// ExtraDaemonArgs is appended to the `tailscaled` command line.
	// ExtraUpArgs is appended to `tailscale up`. Both for escape-hatch
	// use during development.
	ExtraDaemonArgs []string
	ExtraUpArgs     []string
}

// DefaultStateDir is where tailscaled stores its state when StateDir
// is empty.
const DefaultStateDir = "/var/lib/cloudbox/tailscale"

// DefaultSocketPath mirrors the upstream Tailscale default, scoped
// under the cloudbox StateDir to avoid colliding with a system-wide
// tailscaled an operator might have installed for other purposes.
const DefaultSocketPath = "/var/lib/cloudbox/tailscale/tailscaled.sock"
