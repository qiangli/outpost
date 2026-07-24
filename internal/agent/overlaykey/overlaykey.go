// Package overlaykey fetches a fresh single-use overlay (Tailscale/
// Headscale) pre-auth key from cloudbox when this host's tailnet
// registration is no longer valid.
//
// Why this exists: overlay credentials used to arrive only at pairing and
// during reattach, and reattach runs at daemon BOOT only. So anything that
// invalidated the registration — most commonly a cloudbox deploy, which
// resets the embedded Headscale — left the node unable to rejoin until a
// human restarted the daemon. The credentials were always re-issuable; the
// node simply had no way to ask.
//
// The key we ask with is the cloudbox access token. It is signed with
// cloudbox's JWT secret and has nothing to do with the tailnet, so it keeps
// working across exactly the failures that break overlay auth. Every fetch
// is a fresh SINGLE-USE key: nothing replayable is stored on disk, and the
// alternative (a long-lived reusable key held forever) would be a standing
// credential to join the tailnet, which is what reaches the pod network.
package overlaykey

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Credentials is one issuance from cloudbox.
type Credentials struct {
	LoginServer      string `json:"overlay_login_server"`
	AuthKey          string `json:"overlay_auth_key"`
	PodCIDR          string `json:"overlay_pod_cidr"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

// ErrOverlayDisabled means cloudbox is not running the overlay at all.
//
// Distinct from a transient failure on purpose: a caller that cannot tell
// "the feature is off" from "the mint failed" will retry forever against
// something that is never going to answer.
var ErrOverlayDisabled = fmt.Errorf("overlaykey: overlay not enabled on cloudbox")

// ErrThrottled means cloudbox refused because we asked too recently.
var ErrThrottled = fmt.Errorf("overlaykey: throttled by cloudbox")

// Client fetches overlay credentials for one paired host.
type Client struct {
	// BaseURL is the cloudbox base (e.g. https://ai.dhnt.io).
	BaseURL string
	// AccessToken is this host's cloudbox bearer token.
	AccessToken string
	// AgentName is this host's registered name.
	AgentName string
	// HTTP is optional; a 30s-timeout client is used when nil.
	HTTP *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Fetch requests a fresh pre-auth key.
func (c *Client) Fetch(ctx context.Context) (*Credentials, error) {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("overlaykey: BaseURL required")
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return nil, fmt.Errorf("overlaykey: AccessToken required (host not paired?)")
	}
	if strings.TrimSpace(c.AgentName) == "" {
		return nil, fmt.Errorf("overlaykey: AgentName required")
	}

	body, err := json.Marshal(map[string]string{"agent_name": c.AgentName})
	if err != nil {
		return nil, fmt.Errorf("overlaykey: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api/v1/overlay/authkey", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("overlaykey: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.AccessToken))

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("overlaykey: post: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		return nil, ErrOverlayDisabled
	case http.StatusTooManyRequests:
		return nil, ErrThrottled
	default:
		return nil, fmt.Errorf("overlaykey: cloudbox returned %d", resp.StatusCode)
	}

	var creds Credentials
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		return nil, fmt.Errorf("overlaykey: decode response: %w", err)
	}
	// A 200 carrying no key is not success. Returning it would have the
	// caller "re-register" with an empty credential and report progress
	// it did not make.
	if strings.TrimSpace(creds.AuthKey) == "" {
		return nil, fmt.Errorf("overlaykey: cloudbox returned 200 with an empty auth key")
	}
	return &creds, nil
}
