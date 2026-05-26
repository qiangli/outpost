package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// LoadOrCreateHostKey returns outpost's persistent SSH host identity.
// On first call it generates an ed25519 keypair and writes it to
// <UserConfigDir>/matrix/ssh_host_ed25519 with mode 0600. Subsequent
// calls read the same file back.
//
// The key lives in its own file (not in agent.json) so that re-pairing
// — which rewrites agent.json — does NOT regenerate the host identity.
// Clients that have cached our host key in known_hosts would otherwise
// see a REMOTE HOST IDENTIFICATION HAS CHANGED warning on every
// re-pair.
func LoadOrCreateHostKey() (ssh.Signer, error) {
	path, err := hostKeyPath()
	if err != nil {
		return nil, err
	}
	return loadOrCreateHostKeyAt(path)
}

// loadOrCreateHostKeyAt is the path-explicit body of LoadOrCreateHostKey.
// Exposed for tests; production callers should use LoadOrCreateHostKey.
func loadOrCreateHostKeyAt(path string) (ssh.Signer, error) {
	if b, rerr := os.ReadFile(path); rerr == nil {
		signer, perr := ssh.ParsePrivateKey(b)
		if perr != nil {
			return nil, fmt.Errorf("parse ssh host key %s: %w", path, perr)
		}
		return signer, nil
	} else if !os.IsNotExist(rerr) {
		return nil, rerr
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ssh host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "outpost@"+hostnameForKey())
	if err != nil {
		return nil, fmt.Errorf("marshal ssh host key: %w", err)
	}
	if err := writeHostKeyAtomic(path, pem.EncodeToMemory(block)); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

func hostKeyPath() (string, error) {
	dir, err := conf.DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ssh_host_ed25519"), nil
}

func writeHostKeyAtomic(path string, data []byte) error {
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

func hostnameForKey() string {
	h, _ := os.Hostname()
	if h == "" {
		return "host"
	}
	return h
}
