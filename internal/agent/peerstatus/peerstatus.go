// Package peerstatus is a thin client for cloudbox's
// GET /api/v1/peers — the peer status board. It returns, for each
// paired host the caller can see (its owned hosts + hosts shared with
// it), online status, a same-LAN/remote location hint, and the
// build/OS/arch details the host last reported. Consumed by
// `outpost peers status` (CLI) and the `outpost_peers_status` MCP tool.
package peerstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Peer mirrors one row of cloudbox's /api/v1/peers response. Field tags
// match the cloudbox handler's V1PeerEntry exactly.
type Peer struct {
	Host       string `json:"host"`
	Alias      string `json:"alias,omitempty"`
	Owned      bool   `json:"owned"`
	Shared     bool   `json:"shared,omitempty"`
	Online     bool   `json:"online"`
	Location   string `json:"location"` // "same_lan" | "remote" | "unknown"
	Version    string `json:"version,omitempty"`
	Commit     string `json:"commit,omitempty"`
	OS         string `json:"os,omitempty"`
	Arch       string `json:"arch,omitempty"`
	OSVersion  string `json:"os_version,omitempty"`
	UpdateMode string `json:"update_mode,omitempty"`
}

const fetchTimeout = 15 * time.Second

// Fetch GETs the peer status board from cloudbox. base is the cloudbox
// origin (scheme+host); token is the per-outpost access_token. A nil
// client uses http.DefaultClient.
func Fetch(ctx context.Context, base, token string, client *http.Client) ([]Peer, error) {
	if strings.TrimSpace(base) == "" {
		return nil, fmt.Errorf("cloudbox base URL is empty (host not paired?)")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("no access token (host not paired?)")
	}
	url := strings.TrimRight(base, "/") + "/api/v1/peers"
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("cloudbox returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Peers []Peer `json:"peers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode peers: %w", err)
	}
	return out.Peers, nil
}
