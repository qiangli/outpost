package admincore

import (
	"context"
	"net"
	"strconv"
	"strings"

	"github.com/qiangli/outpost/internal/agent/vkcred"
)

// vk-credential minting — the HOSTING side of a peer join's virtual runtimes.
//
// The four values `outpost cluster join` provisions (endpoint, tunnel token,
// STCP secret, k3s node token) authenticate the tunnel and the k3s AGENT — none
// of them is an apiserver bearer credential, so a worker that selected a
// virtual runtime still booted into "no usable credentials" until someone
// hand-edited agent.json. The hand edit people reach for is the admin
// kubeconfig, which must never travel. This surface replaces it: the hosting
// machine (the only place the admin kubeconfig legitimately lives) mints a
// least-privilege ServiceAccount bundle a worker passes as --vk-bundle.
//
// Like the node token, it is deliberately NOT part of ControlPlaneResult, and
// there is no REST route: reading it touches the live plane and returns a
// credential, so it exists only as this explicitly-named operation (MCP + CLI),
// the same treatment ControlPlaneNodeToken gets.

// mintVKCredential is a seam for tests — the real implementation talks to the
// hosted plane's apiserver, which unit tests must not do.
var mintVKCredential = vkcred.Mint

// DefaultVKNamespace is the allow-list a mint falls back to when the operator
// names none. `default` always exists, so the zero-flag path yields a policy
// that both admits something and stays explicit about WHAT (fail-closed
// everywhere else).
const DefaultVKNamespace = "default"

// VKCredentialResult carries the encoded bundle. It is a CREDENTIAL: returned
// only from this explicitly-named operation, never from a status read.
type VKCredentialResult struct {
	OK bool `json:"ok"`
	// Bundle is the opaque one-line value a worker passes to
	// `outpost cluster join --vk-bundle`.
	Bundle string `json:"bundle"`
	// Namespaces is the allow-list embedded in the bundle — the namespace
	// policy the worker will enforce fail-closed.
	Namespaces []string `json:"namespaces"`
	// Endpoint is the tunnel address a worker pairs the bundle with, so the
	// caller can render the whole join line without a second round-trip.
	Endpoint string `json:"endpoint,omitempty"`
}

// ControlPlaneVKCredential mints (or re-reads — the operation is idempotent)
// the least-privilege virtual-kubelet credential of the control plane this
// host hosts, provisioning the named workload namespaces as it goes.
//
// It refuses on a host that is not hosting a plane: the credential describes
// THIS machine's plane, and minting one anywhere else would be a category
// error dressed as success.
func (s *Server) ControlPlaneVKCredential(ctx context.Context, namespaces []string) (VKCredentialResult, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return VKCredentialResult{}, err
	}
	if !fc.Cluster.ControlPlaneOn() {
		return VKCredentialResult{}, badRequest(
			"this host does not host a control plane — the vk credential is minted on the hosting machine " +
				"(run `outpost cluster control-plane on` here, or mint it there)")
	}
	kubeconfig := strings.TrimSpace(fc.Cluster.ControlPlaneKubeconfig)
	if kubeconfig == "" {
		return VKCredentialResult{}, unavailable(
			"the hosted plane's kubeconfig is not recorded yet (cluster.control_plane_kubeconfig) — " +
				"the control-plane container writes it on first boot; start the plane and retry")
	}

	cleaned := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if ns = strings.TrimSpace(ns); ns != "" {
			cleaned = append(cleaned, ns)
		}
	}
	if len(cleaned) == 0 {
		cleaned = []string{DefaultVKNamespace}
	}

	bundle, err := mintVKCredential(ctx, vkcred.MintOptions{
		KubeconfigPath: kubeconfig,
		Namespaces:     cleaned,
	})
	if err != nil {
		// Unavailable, not internal: the plane is a dependency that may simply
		// not be up yet, and the caller's remedy is to start it or wait.
		return VKCredentialResult{}, unavailable("%s", err.Error())
	}
	encoded, err := bundle.Encode()
	if err != nil {
		return VKCredentialResult{}, internalErr("%s", err.Error())
	}
	addr, port := fc.Cluster.TunnelBind()
	return VKCredentialResult{
		OK:         true,
		Bundle:     encoded,
		Namespaces: bundle.Namespaces,
		Endpoint:   net.JoinHostPort(addr, strconv.Itoa(port)),
	}, nil
}
