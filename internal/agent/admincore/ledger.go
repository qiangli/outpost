// Reachability-ledger plumbing for the daemon-side dial path. Wave
// 3B.1 records-only — every successful sshclient.Dial in
// dialSSHChain appends one ReachabilityEdge to the JSONL ledger.
// Wave 3B.2 wires this into the Memberlist gossip layer so peers
// learn about each other's recent contacts.
//
// Self-PeerID is derived lazily from the SSH host key at the first
// call and cached for the daemon's lifetime; subsequent appends are
// just a file write.
package admincore

import (
	"log/slog"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
)

var (
	selfIDOnce sync.Once
	selfID     discovery.PeerID
)

// resolveSelfID loads the persistent ed25519 host key on first call
// and computes the fingerprint. Subsequent calls return the cached
// value. Returns "" if the host key isn't available (e.g. ssh is off
// and discovery is off — both extremely unlikely on a paired host).
func resolveSelfID() discovery.PeerID {
	selfIDOnce.Do(func() {
		signer, err := agent.LoadOrCreateHostKey()
		if err != nil || signer == nil {
			return
		}
		selfID = discovery.PeerID(ssh.FingerprintSHA256(signer.PublicKey()))
	})
	return selfID
}

// recordEdge appends a successful-dial observation to the
// reachability ledger. Best-effort: failure is logged at debug level
// and never propagated. Mirrors cmd/outpost/ssh_dial.go's helper of
// the same shape.
func recordEdge(t conf.SSHTarget, started time.Time) {
	latency := time.Since(started).Milliseconds()
	port := t.Port
	if port <= 0 {
		port = conf.DefaultSSHPort
	}
	kind := discovery.EndpointCloudboxSSH
	transport := "cloudbox-ssh"
	if t.Direct {
		kind = discovery.EndpointLANSSH
		transport = "lan-direct-ssh"
	}
	edge := discovery.ReachabilityEdge{
		Self:      resolveSelfID(),
		PeerName:  t.Name,
		Endpoint:  discovery.Endpoint{Kind: kind, Host: t.Host, Port: port},
		Transport: transport,
		LatencyMs: latency,
		At:        time.Now(),
	}
	if _, err := discovery.AppendLedgerEntry(edge); err != nil {
		slog.Debug("reachability ledger: append failed", "err", err, "peer_name", t.Name)
	}
}
