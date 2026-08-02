package vknode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/rest"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// PeerCredential is the peer-local credential set a virtual-kubelet node uses
// to reach a PEER-hosted (k3s) control plane, with no cloudbox involvement.
//
// It is the peer-plane counterpart of the cloudbox path (FetchKubeconfig +
// ConfigFromCluster): where that path mints a bearer token from cloudbox and
// trusts cloudbox's CA, this one carries the PEER's CA identity and a
// credential minted or held locally — either the k3s bearer Token or the
// client-certificate pair k3s issues. Exactly one credential form is used; the
// client certificate wins when both are present (the stronger statement, and
// what k3s writes).
type PeerCredential struct {
	// APIURL is the local visitor address the peer apiserver is bound on,
	// conventionally https://127.0.0.1:6443 (conf.ClusterConfig.LocalAPIURL).
	APIURL string
	// CA is the PEER plane's CA bundle (PEM). This is the peer CA identity the
	// node trusts, NOT cloudbox's. Empty falls back to the system roots, which
	// fails against a self-signed k3s apiserver — so a peer join should carry
	// it.
	CA []byte
	// Token is the k3s bearer credential (mutually exclusive with the cert).
	Token string
	// ClientCert / ClientKey are the client-certificate credential k3s issues.
	ClientCert []byte
	ClientKey  []byte
}

// CredentialKind names the effective credential form, for logs.
func (p PeerCredential) CredentialKind() string {
	switch {
	case len(p.ClientCert) > 0 && len(p.ClientKey) > 0:
		return "client-cert"
	case p.Token != "":
		return "token"
	default:
		return "none"
	}
}

// Validate rejects a credential that cannot dial an apiserver: no URL, or no
// usable credential at all. A half client-cert pair (cert without key, or vice
// versa) is an error rather than a silent fall-through to the token branch — a
// mismatch that would otherwise surface later as an opaque 401.
func (p PeerCredential) Validate() error {
	if p.APIURL == "" {
		return errors.New("vknode: peer credential: empty APIURL")
	}
	if (len(p.ClientCert) > 0) != (len(p.ClientKey) > 0) {
		return errors.New("vknode: peer credential: client certificate without its key (or vice versa)")
	}
	if p.CredentialKind() == "none" {
		return errors.New("vknode: peer credential: no token and no client certificate — a peer plane authenticates with one of them")
	}
	return nil
}

// PeerCredentialFiles are the on-disk materializations of a PeerCredential.
// Only the files for the present credential form are populated.
type PeerCredentialFiles struct {
	TokenFile string
	CertFile  string
	KeyFile   string
}

// Materialize writes the present credential to 0600 files under dir and
// returns their paths.
//
// The bearer token is written to a FILE (not baked into the rest.Config) so
// client-go's BearerTokenFile transport re-reads a rotated token on its own
// schedule without the controllers rebuilding — the same rotation mechanism the
// cloudbox path uses (see ConfigFromCluster). Client-certificate credentials
// are written as files too, but client-go caches them, so a cert rotation needs
// a restart (which the settings surfaces already schedule).
//
// Idempotent: rewriting the same bytes leaves the same files (atomic
// write-then-rename), so a restart re-materializes cleanly.
func (p PeerCredential) Materialize(dir string) (PeerCredentialFiles, error) {
	if dir == "" {
		return PeerCredentialFiles{}, errors.New("vknode: peer credential: empty dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return PeerCredentialFiles{}, fmt.Errorf("vknode: peer credential dir: %w", err)
	}
	var files PeerCredentialFiles
	if len(p.ClientCert) > 0 && len(p.ClientKey) > 0 {
		files.CertFile = filepath.Join(dir, "peer-client.crt")
		files.KeyFile = filepath.Join(dir, "peer-client.key")
		if err := writeFileAtomic(files.CertFile, p.ClientCert); err != nil {
			return PeerCredentialFiles{}, err
		}
		if err := writeFileAtomic(files.KeyFile, p.ClientKey); err != nil {
			return PeerCredentialFiles{}, err
		}
	}
	if p.Token != "" {
		files.TokenFile = filepath.Join(dir, "peer-token")
		if err := WriteTokenFile(files.TokenFile, p.Token); err != nil {
			return PeerCredentialFiles{}, err
		}
	}
	return files, nil
}

// RestConfig builds a *rest.Config for the peer apiserver from the materialized
// files. Client certificate wins over bearer token when both are present. The
// peer CA is pinned as CAData so the node trusts the PEER plane's identity, not
// the system roots or cloudbox's.
func (p PeerCredential) RestConfig(files PeerCredentialFiles) (*rest.Config, error) {
	if p.APIURL == "" {
		return nil, errors.New("vknode: peer credential: empty APIURL")
	}
	cfg := &rest.Config{Host: p.APIURL}
	if len(p.CA) > 0 {
		cfg.TLSClientConfig.CAData = append([]byte(nil), p.CA...)
	}
	switch {
	case files.CertFile != "" && files.KeyFile != "":
		cfg.TLSClientConfig.CertFile = files.CertFile
		cfg.TLSClientConfig.KeyFile = files.KeyFile
	case files.TokenFile != "":
		cfg.BearerTokenFile = files.TokenFile
	default:
		return nil, errors.New("vknode: peer credential: no materialized credential")
	}
	return cfg, nil
}

// DefaultPeerCredentialDir is where peer credential files are materialized —
// conf.DefaultCacheDir()/cluster-peer, alongside the rest of the agent's
// runtime state.
func DefaultPeerCredentialDir() (string, error) {
	base, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cluster-peer"), nil
}

// writeFileAtomic writes data to path (mode 0600) via a tmp file + rename, so a
// reader never sees a partial write.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("vknode: write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vknode: rename %s: %w", filepath.Base(path), err)
	}
	return nil
}

// peerCredentialRefreshInterval is the steady-state cadence between peer
// credential re-reads. An operator rotation is a human-paced event, so 60s is
// responsive without churning the disk.
const peerCredentialRefreshInterval = 60 * time.Second

// PeerCredentialReloader returns the current persisted peer credential — a
// re-read of FileConfig.Cluster — so the refresher picks up an operator
// rotation (a new cluster.token saved via any settings surface) without a
// restart.
type PeerCredentialReloader func() (PeerCredential, error)

// PeerCredentialRefresher re-materializes the peer bearer-token file whenever
// the persisted token changes. It is the peer-plane, cloudbox-free counterpart
// of Refresher: no minting call, just a local re-read + rewrite, which suffices
// because client-go re-reads the BearerTokenFile on its own schedule.
//
// Client-certificate rotation is deliberately NOT handled live (client-go
// caches the cert); it takes effect on the restart the settings surfaces
// schedule.
type PeerCredentialRefresher struct {
	reload   PeerCredentialReloader
	files    PeerCredentialFiles
	interval time.Duration
	last     string // last-materialized bearer token, to skip no-op writes
}

// NewPeerCredentialRefresher captures the reloader, the materialized file paths,
// and the credential currently in effect (so the first change is detected
// against it).
func NewPeerCredentialRefresher(reload PeerCredentialReloader, files PeerCredentialFiles, current PeerCredential) *PeerCredentialRefresher {
	return &PeerCredentialRefresher{
		reload:   reload,
		files:    files,
		interval: peerCredentialRefreshInterval,
		last:     current.Token,
	}
}

// Run blocks until ctx is canceled, re-reading the persisted credential on each
// tick and rewriting the token file when it rotated.
func (r *PeerCredentialRefresher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.interval):
		}
		if _, err := r.refreshOnce(); err != nil {
			slog.Warn("vknode: peer credential refresh failed", "err", err)
		}
	}
}

// refreshOnce reloads the persisted credential and, when its bearer token
// changed, rewrites the token file so client-go picks up the rotation. Returns
// whether a rotation was applied. Exported behavior is exercised directly by
// the deterministic rotation test.
func (r *PeerCredentialRefresher) refreshOnce() (bool, error) {
	cred, err := r.reload()
	if err != nil {
		return false, err
	}
	if cred.Token == "" || cred.Token == r.last {
		return false, nil
	}
	if r.files.TokenFile == "" {
		// The node authenticates with a client cert, not a token; there is no
		// token file to rewrite. Record the value so we don't re-log.
		r.last = cred.Token
		return false, nil
	}
	if err := WriteTokenFile(r.files.TokenFile, cred.Token); err != nil {
		return false, err
	}
	slog.Info("vknode: peer bearer credential rotated (live)", "token_file", r.files.TokenFile)
	r.last = cred.Token
	return true, nil
}
