// `outpost scan` and `outpost discover probe <url>` — Wave 3A.1 CLI
// surfaces for LAN peer discovery.
//
// scan       — broadcasts an mDNS query for `_outpost._tcp.local`,
//                prints a table of responding peers (Tier 1).
// discover   — performs a full hello → probe exchange against a known
//   probe       URL and prints the result with the trust state.
//
// Both commands work without the daemon being paired and without any
// MCP roundtrip — they're operator tooling that runs in the
// terminal. The MCP equivalents (outpost_scan_peers, outpost_probe_peer)
// live in tools_discovery.go for agentic callers.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
)

func scanCmd() *cobra.Command {
	var (
		jsonOut bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Discover outposts on the local network via mDNS",
		Long: `outpost scan broadcasts an mDNS query for _outpost._tcp.local
and prints the responses as a table (or --json for machine-readable
output). Tier 1 only — no cryptographic verification. To verify a
peer's identity, use 'outpost discover probe <url>'.

The local outpost daemon does NOT need to be running for this
command to work; mDNS is host-level. However, only outposts that
have opted into discovery (FileConfig.discovery_enabled=true) appear
in the results.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runScan(cmd.Context(), jsonOut, timeout)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "How long to wait for mDNS responses")
	return cmd
}

func runScan(ctx context.Context, jsonOut bool, timeout time.Duration) error {
	// Suppress our own announcement if it matches a known FileConfig.
	selfID := selfPeerIDFromConfig()

	peers, err := discovery.Browse(ctx, discovery.BrowseOptions{
		Timeout:    timeout,
		SelfPeerID: selfID,
	})
	if err != nil && len(peers) == 0 {
		return fmt.Errorf("mdns browse: %w", err)
	}

	if jsonOut {
		b, _ := json.MarshalIndent(peers, "", "  ")
		fmt.Println(string(b))
		return nil
	}

	if len(peers) == 0 {
		fmt.Println("No outpost peers found on the LAN within the timeout.")
		fmt.Println("If you expect peers, confirm 'discovery_enabled' is set on the other outposts and that mDNS isn't blocked by the network.")
		return nil
	}
	fmt.Printf("%-22s  %-18s  %-26s  %-8s  %-22s  %s\n",
		"AGENT", "ASSIGNED.local", "PEER-ID", "PAIRED", "OAUTH2-EMAIL", "ENDPOINTS")
	for _, p := range peers {
		paired := "no"
		if p.Paired {
			paired = "yes"
		}
		endpoints := summarizeEndpoints(p.Endpoints)
		fmt.Printf("%-22s  %-18s  %-26s  %-8s  %-22s  %s\n",
			truncStr(p.AgentName, 22),
			truncStr(p.AssignedHostname, 18),
			truncStr(string(p.ID), 26),
			paired,
			truncStr(p.OAuth2Email, 22),
			endpoints,
		)
	}
	return nil
}

func discoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "LAN discovery: probe a known outpost URL or list discovered peers",
	}
	cmd.AddCommand(discoverProbeCmd())
	return cmd
}

func discoverProbeCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "probe <url>",
		Short: "Probe an outpost's HTTP /discover surface (hello → probe roundtrip)",
		Long: `probe contacts the given URL's /api/v1/discover/{hello,probe}
endpoints, exchanges signed nonces for mutual fingerprint verification,
and prints the discovered peer record.

The local outpost must be runnable so we have an ed25519 host key to
sign the server's challenge with. (We don't need cloudbox connectivity
for this — the host key lives entirely locally.)

Example:
  outpost discover probe http://192.168.1.42:17778`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiscoverProbe(cmd.Context(), args[0], jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the full ProbeResult envelope as JSON")
	return cmd
}

func runDiscoverProbe(ctx context.Context, baseURL string, jsonOut bool) error {
	signer, err := loadLocalHostKey()
	if err != nil {
		return fmt.Errorf("load host key: %w", err)
	}
	peerID := discovery.PeerID(fingerprintSHA256OfSigner(signer))

	selfHello := discovery.PeerHello{
		PeerID:    peerID,
		AgentName: localAgentName(),
		Version:   localVersion(),
	}

	client := discovery.NewClient()
	result, err := client.Probe(ctx, baseURL, signer, selfHello)
	if err != nil {
		return fmt.Errorf("probe %s: %w", baseURL, err)
	}

	if jsonOut {
		b, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(b))
		return nil
	}

	p := result.Peer
	fmt.Printf("Peer found at %s:\n", baseURL)
	fmt.Printf("  agent_name:        %s\n", p.AgentName)
	fmt.Printf("  assigned_hostname: %s\n", p.AssignedHostname)
	fmt.Printf("  peer_id:           %s\n", p.ID)
	fmt.Printf("  oauth2_email:      %s\n", p.OAuth2Email)
	fmt.Printf("  paired:            %t\n", p.Paired)
	fmt.Printf("  cloudbox:          %s\n", p.CloudboxBase)
	fmt.Printf("  version:           %s\n", p.Version)
	fmt.Printf("  trust:             %s\n", p.Trust)
	fmt.Printf("  endpoints:         %s\n", summarizeEndpoints(p.Endpoints))
	if !result.ServerVerified {
		fmt.Fprintln(os.Stderr, "\nNote: the peer did NOT pass mutual fingerprint verification — Tier-2 ops (ssh exec, repair, etc.) will refuse this peer.")
	}
	return nil
}

// --- helpers ---

// selfPeerIDFromConfig returns the local outpost's PeerID when the
// host key file is readable, empty otherwise. Used by `outpost scan`
// to filter ourselves out of the announce stream.
func selfPeerIDFromConfig() discovery.PeerID {
	signer, err := loadLocalHostKey()
	if err != nil {
		return ""
	}
	return discovery.PeerID(fingerprintSHA256OfSigner(signer))
}

// localAgentName returns the AgentName from the local FileConfig
// when readable, otherwise the OS hostname.
func localAgentName() string {
	cfgPath, _ := conf.DefaultConfigPath()
	if cfgPath != "" {
		if fc, err := conf.LoadFile(cfgPath); err == nil && fc != nil && fc.AgentName != "" {
			return fc.AgentName
		}
	}
	h, _ := os.Hostname()
	if h == "" {
		return "outpost"
	}
	return h
}

// localVersion returns the running binary's commit short.
func localVersion() string {
	return agent.ReadBuildInfo().Short()
}

// loadLocalHostKey loads the persistent ed25519 host key from disk.
// Same key the SSH server uses; we need it locally to sign discovery
// probe nonces in `outpost discover probe`.
func loadLocalHostKey() (ssh.Signer, error) {
	return agent.LoadOrCreateHostKey()
}

// fingerprintSHA256OfSigner returns the SHA256 fingerprint of the
// signer's public key in the same shape ssh-keygen / ssh-keyscan
// output: "SHA256:<base64>".
func fingerprintSHA256OfSigner(s ssh.Signer) string {
	if s == nil {
		return ""
	}
	return ssh.FingerprintSHA256(s.PublicKey())
}

func summarizeEndpoints(eps []discovery.Endpoint) string {
	if len(eps) == 0 {
		return "-"
	}
	out := ""
	for i, e := range eps {
		if i > 0 {
			out += ", "
		}
		if e.Port > 0 {
			out += fmt.Sprintf("%s=%s:%d", e.Kind, e.Host, e.Port)
		} else {
			out += fmt.Sprintf("%s=%s", e.Kind, e.Host)
		}
	}
	return out
}

func truncStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
