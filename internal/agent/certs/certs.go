// Package certs is the outpost-side counterpart to cloudbox's
// internal/ca package (roadmap item #11).
//
// At first boot after pairing, the outpost POSTs its SSH host
// pubkey to /api/v1/ca/sign-host-cert. The response gives:
//
//   - host_cert    — an OpenSSH host certificate signed by cloudbox
//                    that binds this outpost's identity facts (the
//                    owner email, OS user, host-key fingerprint)
//   - ca_pubkey    — the cloudbox CA pubkey, pinned locally so peer
//                    probes can verify each other's certs without
//                    a roundtrip to cloudbox on every handshake
//   - lifetime     — human-readable hint ("30d")
//
// We persist both into FileConfig.Cluster (HostCert + CAPubkey
// fields). A refresh ticker re-signs before expiry; for now
// "before expiry" is a static "every 7 days" cadence since the
// cloudbox cert lifetime is fixed at 30 days.
//
// Failure modes are non-fatal — when the CA endpoint is unreachable
// the outpost falls back to TOFU-only trust on peer probes, same
// as before this code shipped.

package certs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Default refresh cadence. Cloudbox's CertLifetime is 30 days; we
// refresh weekly so an offline stretch can't expire the cert.
const DefaultRefreshInterval = 7 * 24 * time.Hour

// Config is what NewManager takes.
type Config struct {
	CloudboxBase string
	AccessToken  string
	Principal    string // assigned_hostname / agent_name
	HostKey      ssh.Signer

	// Refresh interval. Zero = DefaultRefreshInterval.
	RefreshInterval time.Duration

	// OnRefresh is called when a fresh cert lands. Typically wired
	// to write the new cert + ca pubkey into FileConfig.Cluster
	// and SaveFile.
	OnRefresh func(cert, caPubkey string) error

	// HTTPClient is optional; default 30s timeout.
	HTTPClient *http.Client
}

// Manager is the long-running cert refresh worker.
type Manager struct {
	cfg Config
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.CloudboxBase == "" {
		return nil, errors.New("certs: empty CloudboxBase")
	}
	if cfg.AccessToken == "" {
		return nil, errors.New("certs: empty AccessToken")
	}
	if cfg.Principal == "" {
		return nil, errors.New("certs: empty Principal")
	}
	if cfg.HostKey == nil {
		return nil, errors.New("certs: nil HostKey")
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = DefaultRefreshInterval
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Manager{cfg: cfg}, nil
}

// Run drives the boot fetch + periodic refresh. Blocks until
// ctx.Done(). Failures during the boot fetch are logged but the
// loop continues — we DON'T want a transient cloudbox blip to
// disable peer trust forever.
func (m *Manager) Run(ctx context.Context) error {
	// Boot fetch after short jitter so we don't fight with the
	// /apps poller for the same matrix-tunnel bandwidth on first
	// boot.
	time.Sleep(10 * time.Second)
	if err := m.refreshOnce(ctx); err != nil {
		slog.Warn("certs: initial fetch failed (will retry)", "err", err)
	}
	t := time.NewTicker(m.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := m.refreshOnce(ctx); err != nil {
				slog.Warn("certs: refresh failed", "err", err)
			}
		}
	}
}

type signHostCertReq struct {
	HostPubkey string `json:"host_pubkey"`
	Principal  string `json:"principal"`
}

type signHostCertResp struct {
	Cert     string `json:"cert"`
	CAPubkey string `json:"ca_pubkey"`
	Lifetime string `json:"lifetime"`
}

func (m *Manager) refreshOnce(ctx context.Context) error {
	pub := string(ssh.MarshalAuthorizedKey(m.cfg.HostKey.PublicKey()))
	body, _ := json.Marshal(signHostCertReq{
		HostPubkey: pub,
		Principal:  m.cfg.Principal,
	})
	url := strings.TrimRight(m.cfg.CloudboxBase, "/") + "/api/v1/ca/sign-host-cert"

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("certs: cloudbox returned %d: %s", resp.StatusCode, respBody)
	}
	var r signHostCertResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return err
	}
	if m.cfg.OnRefresh != nil {
		if err := m.cfg.OnRefresh(r.Cert, r.CAPubkey); err != nil {
			return fmt.Errorf("certs: OnRefresh: %w", err)
		}
	}
	slog.Info("certs: refreshed", "principal", m.cfg.Principal, "lifetime", r.Lifetime)
	return nil
}
