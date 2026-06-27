package peerplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client speaks cloudbox's peer-plane signaling API (/api/v1/peer/*), the SOLE
// rendezvous for the fabric. Bearer-authed with the outpost access token
// (peer:signal scope).
type Client struct {
	BaseURL string
	Token   string
	HC      *http.Client
}

func (c *Client) hc() *http.Client {
	if c.HC != nil {
		return c.HC
	}
	return http.DefaultClient
}

// Announce publishes this host's reachability candidates.
func (c *Client) Announce(ctx context.Context, host, peerID string, candidates []string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/peer/announce", map[string]any{
		"host":       host,
		"peer_id":    peerID,
		"candidates": strings.Join(candidates, ","),
	}, nil)
}

// PeerTarget is the Connect response: the peer's candidates + observed external
// IP, plus the egress-IP same_lan hint (a guess; the probe is ground truth).
type PeerTarget struct {
	Peer struct {
		Host       string   `json:"host"`
		Owner      string   `json:"owner"`
		PeerID     string   `json:"peer_id"`
		Candidates []string `json:"candidates"`
		ExternalIP string   `json:"external_ip"`
	} `json:"peer"`
	SameLAN bool `json:"same_lan"`
}

// Connect requests a rendezvous from fromHost to toHost. Returns the peer's
// candidates and enqueues a notice for the peer to reciprocate.
func (c *Client) Connect(ctx context.Context, fromHost, toHost string) (*PeerTarget, error) {
	var out PeerTarget
	if err := c.do(ctx, http.MethodPost, "/api/v1/peer/connect", map[string]string{
		"from_host": fromHost, "to_host": toHost,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Rendezvous is one pending inbound connect notice.
type Rendezvous struct {
	FromHost       string   `json:"from_host"`
	FromOwner      string   `json:"from_owner"`
	FromPeerID     string   `json:"from_peer_id"`
	FromCandidates []string `json:"from_candidates"`
}

// Inbox returns + drains the pending rendezvous notices addressed to host.
func (c *Client) Inbox(ctx context.Context, host string) ([]Rendezvous, error) {
	var out struct {
		Rendezvous []Rendezvous `json:"rendezvous"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/peer/inbox?host="+url.QueryEscape(host), nil, &out); err != nil {
		return nil, err
	}
	return out.Rendezvous, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.hc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("peer signaling %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
