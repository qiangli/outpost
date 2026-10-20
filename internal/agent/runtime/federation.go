package runtime

import (
	"fmt"
	"strings"
)

// Env vars carrying the federation-member session (see Options.Federation*)
// into the container. Wire contract with image/entrypoint.sh's
// federation-visitor block — it activates on HOST and STCP_SECRET both being
// non-empty and defaults the rest.
const (
	federationHostEnv       = "OUTPOST_FEDERATION_HOST"
	federationServerPortEnv = "OUTPOST_FEDERATION_SERVER_PORT"
	federationProtocolEnv   = "OUTPOST_FEDERATION_PROTOCOL"
	federationTokenEnv      = "OUTPOST_FEDERATION_TOKEN"
	federationSecretEnv     = "OUTPOST_FEDERATION_STCP_SECRET"
	federationBindPortEnv   = "OUTPOST_FEDERATION_BIND_PORT"
)

// FederationActive reports whether these Options carry a usable federation
// visitor: the federation host's frps address and the STCP secret must both
// be present, matching the entrypoint's own activation test.
func (o Options) FederationActive() bool {
	return strings.TrimSpace(o.FederationHost) != "" &&
		strings.TrimSpace(o.FederationSecret) != ""
}

func (o Options) federationBindPort() int {
	if o.FederationBindPort > 0 {
		return o.FederationBindPort
	}
	return DefaultFederationAPIPort
}

// federationArgs renders the `-e` pairs and the host-loopback `-p` publish for
// the federation visitor. Returns NOTHING when inactive so an unfederated
// runtime's container command is byte-identical to the pre-federation one.
//
// The publish is pinned to 127.0.0.1: host-side `karmadactl register` and
// `bashy kubectl` reach the visitor there; it is never a LAN service.
func federationArgs(opts Options) []string {
	if !opts.FederationActive() {
		return nil
	}
	port := opts.federationBindPort()
	args := []string{
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"-e", federationHostEnv + "=" + strings.TrimSpace(opts.FederationHost),
		"-e", federationSecretEnv + "=" + strings.TrimSpace(opts.FederationSecret),
		"-e", fmt.Sprintf("%s=%d", federationBindPortEnv, port),
	}
	if opts.FederationPort != 0 {
		args = append(args, "-e", fmt.Sprintf("%s=%d", federationServerPortEnv, opts.FederationPort))
	}
	if p := strings.TrimSpace(opts.FederationProtocol); p != "" {
		args = append(args, "-e", federationProtocolEnv+"="+p)
	}
	// Stamped even when empty, like the overlay relay token.
	args = append(args, "-e", federationTokenEnv+"="+opts.FederationToken)
	return args
}
