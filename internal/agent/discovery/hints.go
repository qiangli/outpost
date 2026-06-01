// Roadmap item #12 — outpost-side NAT-locality hint poller.
//
// Polls cloudbox `GET /api/v1/peer-hints` every 5 min. Each hint
// names another outpost owned by the same cloudbox account that
// shares the local outpost's external IP — i.e., almost certainly
// reachable on the same LAN even when mDNS can't cross network
// segments. Each hint Upserts into the discovery Cache with
// SourceCloudboxHint, so the rest of the discovery surface
// (outpost peers list, ssh add --from-peer, route-to) sees them.
//
// We deliberately do NOT auto-dial hints — they're advisory. The
// operator-feedback signal on hint quality is still pending (see
// the Phase 2 deferral in docs/outpost-roadmap.md); the hint
// pipeline only surfaces, doesn't act.

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// HintsClient is the polling worker. One per outpost.
type HintsClient struct {
	cloudboxBase string
	accessToken  string
	cache        *Cache
	interval     time.Duration
	client       *http.Client
}

// HintsConfig is the input shape for NewHintsClient.
type HintsConfig struct {
	CloudboxBase string
	AccessToken  string
	Cache        *Cache
	Interval     time.Duration // default 5m
	HTTPClient   *http.Client  // optional; default 30s timeout
}

func NewHintsClient(cfg HintsConfig) *HintsClient {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HintsClient{
		cloudboxBase: cfg.CloudboxBase,
		accessToken:  cfg.AccessToken,
		cache:        cfg.Cache,
		interval:     interval,
		client:       client,
	}
}

// peerHintsResponse mirrors cloudbox handlers.PeerHintsResponse.
type peerHintsResponse struct {
	CallerExternalIP string         `json:"caller_external_ip"`
	Hints            []peerHintWire `json:"hints"`
}

type peerHintWire struct {
	Name             string     `json:"name"`
	AssignedHostname string     `json:"assigned_hostname,omitempty"`
	ExternalIP       string     `json:"external_ip"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
}

// Run drives the poll loop. Blocks until ctx.Done(). Configurations
// missing required pieces (cloudbox URL, access token, cache) cause
// Run to log + sleep on ctx without polling — useful so caller can
// always Go(client.Run) without thinking about a half-configured
// outpost.
func (h *HintsClient) Run(ctx context.Context) error {
	if h.cloudboxBase == "" || h.accessToken == "" || h.cache == nil {
		slog.Info("hints: disabled (missing cloudbox URL, access token, or cache)")
		<-ctx.Done()
		return nil
	}
	// Boot poll after short jitter; then on interval.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if err := h.pollOnce(ctx); err != nil {
			slog.Warn("hints: poll failed", "err", err)
		}
		timer.Reset(h.interval)
	}
}

// pollOnce hits /api/v1/peer-hints and merges the response into
// the cache.
func (h *HintsClient) pollOnce(ctx context.Context) error {
	url := strings.TrimRight(h.cloudboxBase, "/") + "/api/v1/peer-hints"
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.accessToken)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hints: cloudbox returned %d: %s", resp.StatusCode, body)
	}
	var hr peerHintsResponse
	if err := json.Unmarshal(body, &hr); err != nil {
		return fmt.Errorf("decode hints: %w", err)
	}
	for _, hint := range hr.Hints {
		// Synthesize a Peer with SourceCloudboxHint. We don't know
		// the peer's fingerprint, real endpoints, or paired-ness
		// from the hint alone — those resolve at probe time. The
		// AssignedHostname is the strongest LAN-direct candidate
		// (try <hostname>.local).
		peer := Peer{
			ID:               PeerID("cb-hint:" + hint.Name),
			AgentName:        hint.Name,
			AssignedHostname: hint.AssignedHostname,
			Sources:          []Source{SourceCloudboxHint},
			Trust:            TrustUnverified,
			LastSeenAt:       time.Now(),
		}
		if hint.AssignedHostname != "" {
			// The hint suggests this peer is on our LAN; surface
			// an LAN-SSH endpoint candidate at <hostname>.local
			// so the dial path can try mDNS resolution + TCP
			// probe. Port left empty — peer's actual LAN-SSH
			// port comes from the /hello round-trip, not the hint.
			peer.Endpoints = []Endpoint{{
				Kind: EndpointLANSSH,
				Host: hint.AssignedHostname + ".local",
			}}
		}
		h.cache.Upsert(peer)
	}
	slog.Debug("hints: poll merged", "count", len(hr.Hints), "caller_ip", hr.CallerExternalIP)
	return nil
}
