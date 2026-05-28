// Package k3sagent supervises a real `k3s agent` subprocess that joins
// the cloudbox-embedded apiserver as a normal Kubernetes kubelet (no
// virtual-kubelet shim). This is the runtime half of "real shared k8s"
// Phase 1 — the other half is the matrix-tunnel STCP visitor that
// terminates the apiserver address locally on this outpost (so the
// agent can dial https://127.0.0.1:<port> as if the apiserver were
// next to it).
//
// Linux-only by design: kubelet has no Darwin / Windows build. On
// other OSes Run returns ErrUnsupported and the caller is expected
// to fall back to vkpodman (the v1 virtual-kubelet path) or surface
// the error to the operator.
package k3sagent

import (
	"errors"
)

// ErrUnsupported is returned by Run on non-Linux platforms.
// Callers (cmd/outpost/main.go startClusterRunner) surface this as a
// "k3s agent mode not supported on <GOOS>; use vkpodman or run k3s in
// a Linux VM" log message; outpost stays up.
var ErrUnsupported = errors.New("k3sagent: real-kubelet mode requires Linux")

// Options is the supervisor's input. All fields are required unless
// noted; missing values cause Run to return early with a clear error.
type Options struct {
	// Binary is the path to a `k3s` executable (single static binary
	// that bundles both server and agent modes). When empty, the
	// supervisor looks up "k3s" on PATH. Pin a known version per the
	// cloudbox-embedded k3s for predictable behavior.
	Binary string

	// Server is the URL the agent dials to register and watch.
	// In Phase 1 this is always "https://127.0.0.1:<K8sAPIPort>" —
	// the STCP visitor on the outpost's loopback that bridges to
	// cloudbox's embedded apiserver. Plumbed as a field so future
	// modes (direct dial when cloudbox exposes the apiserver
	// publicly) can reuse this package.
	Server string

	// Token is the k3s join token (`K10<ca-hash>::node:<secret>`)
	// cloudbox handed out at register time. Passed as `--token` to
	// the subprocess.
	Token string

	// NodeName is what `kubectl get nodes` will show. Defaults to the
	// agent's hostname when empty; we always set it from the outpost's
	// configured AgentName so the cluster identity matches the portal
	// identity.
	NodeName string

	// DataDir is the agent's local state (image cache, kubelet
	// credentials, containerd root). Defaults to /var/lib/cloudbox/k3s-agent.
	// Operators with constrained disk can override.
	DataDir string

	// ExtraArgs are appended verbatim to the `k3s agent ...` command
	// line. Useful for `--kubelet-arg=...`, `--node-label=...`, or
	// pointing at a non-default container runtime endpoint during dev.
	ExtraArgs []string
}

// DefaultDataDir is where k3s agent writes its local state when DataDir
// is empty. Distinct from the cloudbox-side k3s server data dir so the
// two never collide if someone runs both on the same host (dev only).
const DefaultDataDir = "/var/lib/cloudbox/k3s-agent"
