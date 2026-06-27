// Package mesh is the outpost's libp2p peer data plane — the node that
// carries authenticated, encrypted, NAT-traversing peer↔peer streams.
//
// It is the transport under shard-RPC (a loopback rpc-server forwarded over
// the mesh), peer-backup, and the broader resource fabric. cloudbox is the
// rendezvous/signaler; data goes peer-to-peer direct (hole-punched via
// DCUtR), with relay only as fallback. See docs/libp2p-mesh-transport.md.
package mesh

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// keyFileName is the persisted libp2p identity, a sibling of ssh_host_ed25519.
const keyFileName = "mesh_ed25519"

// LoadOrCreateKey returns outpost's persistent libp2p mesh identity. The first
// call generates an ed25519 keypair and writes the libp2p-marshalled private
// key to <ConfigDir>/mesh_ed25519 (mode 0600); later calls read it back.
//
// Like the SSH host key, it lives in its own file (not agent.json) so that
// re-pairing — which rewrites agent.json — does NOT change the peer ID. A
// stable peer ID is what lets cloudbox keep routing rendezvous to this host.
func LoadOrCreateKey() (crypto.PrivKey, error) {
	dir, err := conf.DefaultConfigDir()
	if err != nil {
		return nil, err
	}
	return loadOrCreateKeyAt(filepath.Join(dir, keyFileName))
}

// loadOrCreateKeyAt is the path-explicit body of LoadOrCreateKey, exposed for
// tests. Production callers should use LoadOrCreateKey.
func loadOrCreateKeyAt(path string) (crypto.PrivKey, error) {
	if b, rerr := os.ReadFile(path); rerr == nil {
		priv, perr := crypto.UnmarshalPrivateKey(b)
		if perr != nil {
			return nil, fmt.Errorf("parse mesh key %s: %w", path, perr)
		}
		return priv, nil
	} else if !os.IsNotExist(rerr) {
		return nil, rerr
	}

	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate mesh key: %w", err)
	}
	raw, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal mesh key: %w", err)
	}
	if err := writeKeyAtomic(path, raw); err != nil {
		return nil, err
	}
	return priv, nil
}

func writeKeyAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
