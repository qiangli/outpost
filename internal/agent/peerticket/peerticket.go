// Package peerticket trades a host's elevation cookie at cloudbox for a
// short-lived, single-use peer ticket. It is shared by every direct
// (LAN or mesh) ssh dial: the CLI's ad-hoc ladder and the daemon's
// named-target chain, so both authorize a direct path the same way.
package peerticket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotDeployed reports that cloudbox has no peer-ticket endpoint (404):
// the direct path does not apply and the caller falls back to the tunnel.
var ErrNotDeployed = errors.New("peer-ticket endpoint not available")

// response is the wire shape of the cloudbox endpoint's
// success response. Kept tiny on purpose — the outpost only needs the
// ticket string itself.
type response struct {
	Ticket string `json:"ticket"`
}

// Exchange trades the local outpost's matrix_elev cookie at
// cloudbox's `/api/v1/ssh/peer-ticket` for a short-lived JWT scoped to
// `host` and `scope`. The cookie itself never leaves this exchange —
// only the derived ticket is presented to the peer.
//
// This is a CONTROL-PLANE call on the DATA-PLANE path, and that is the DESIGN,
// not a wart to engineer away. Tickets are 60s and single-use — the peer
// replay-protects the jti — so a captured one is worthless almost immediately
// and can never be presented twice. Minting per dial is what makes the
// authorization CURRENT: still paired, still authorized, sharing not revoked,
// elevation cookie still valid. A cached or long-lived ticket would answer
// those questions with stale facts.
//
// Peer mobility sharpens the point rather than weakening it. Laptops move
// between networks and the node layout changes underneath, so standing
// authority must not travel with the device. Accepting a caller purely on its
// libp2p peer identity — the tempting "remove the dependency" fix — would do
// exactly that: grant durable standing to a key that moves with the hardware,
// which cloudbox could no longer revoke. The short-lived, per-connection,
// centrally-issued ticket is the property worth keeping.
//
// The accepted cost is availability: a cloudbox outage denies the direct path,
// and since the relay fallback also needs cloudbox, both fail together. The
// retry below is the right scope of mitigation — survive a blip without
// weakening the check. Buying more than that belongs in cloudbox's own
// availability, never in a longer-lived or reusable credential.
//
// A 404 from cloudbox (endpoint not deployed yet) is treated as
// ErrNotDeployed so the caller can fall back to the tunnel.
func Exchange(ctx context.Context, cbBase, bearer, cookie, host, scope string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"host":  host,
		"scope": []string{scope},
	})
	u := strings.TrimRight(cbBase, "/") + "/api/v1/ssh/peer-ticket"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Cookie", "matrix_elev="+cookie)

	// A ticket is SINGLE-USE — the peer consumes its jti against a replay LRU
	// and rejects a second presentation — so it cannot be cached, and every
	// dial needs a fresh one. That makes this one call the point where a
	// momentary cloudbox blip denies a path that is otherwise fully
	// peer-to-peer. Observed live: a direct mesh link was up and idle while
	// `mesh-direct attempt failed ... cloudbox returned 504` sent the
	// connection to the relay.
	//
	// Retry the transient statuses briefly before giving up. This does not
	// remove the dependency (see the note on exchangePeerTicket), it only
	// stops a hiccup measured in hundreds of milliseconds from costing the
	// direct path. 4xx is a real answer and is never retried.
	client := &http.Client{Timeout: 15 * time.Second}
	var (
		resp     *http.Response
		respBody []byte
	)
	backoff := []time.Duration{200 * time.Millisecond, 600 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// Body was consumed by the previous attempt; rebuild the request.
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
			if err != nil {
				return "", err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("Cookie", "matrix_elev="+cookie)
		}
		resp, err = client.Do(req)
		if err == nil {
			respBody, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		retryable := err != nil || resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout
		if !retryable || attempt >= len(backoff) {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff[attempt]):
		}
	}
	if err != nil {
		return "", err
	}

	if resp.StatusCode == http.StatusNotFound {
		// Endpoint not deployed yet on cloudbox; tunnel-fallback.
		return "", ErrNotDeployed
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cloudbox returned %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	var out response
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode peer-ticket response: %w", err)
	}
	if strings.TrimSpace(out.Ticket) == "" {
		return "", errors.New("cloudbox returned empty ticket")
	}
	return out.Ticket, nil
}
