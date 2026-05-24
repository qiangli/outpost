package vkpodman

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// refreshLeadTime is how far in advance of token expiry we re-fetch.
// 12h on a 24h cloudbox-issued token gives one full retry window if
// cloudbox is briefly unreachable — long enough that an overnight DO
// reboot doesn't strand the cluster, short enough that a misconfigured
// scope or revoked token is noticed during business hours.
const refreshLeadTime = 12 * time.Hour

// minRefreshInterval bounds how aggressively we retry on failure. We
// want to recover from a transient cloudbox outage within minutes, not
// hammer the endpoint on a permanent error (e.g. revoked access_token,
// cloudbox cluster mode turned off after we last joined).
const minRefreshInterval = 5 * time.Minute

// RefreshDeps is the dependency bundle the Refresher needs. Keeps the
// constructor signature small as we add behaviors (audit hooks, etc.)
// later; everything in here is captured at construction time.
type RefreshDeps struct {
	// CloudboxBase is the HTTPS base URL of cloudbox (without the
	// /api/cluster/kubeconfig suffix), e.g. "https://ai.dhnt.io".
	CloudboxBase string

	// AccessToken is the outpost's existing matrix access_token. Used
	// as the Bearer when calling cloudbox's kubeconfig endpoint.
	AccessToken string

	// NodeName is the outpost identity to mint the kubeconfig for —
	// always fc.AgentName in practice.
	NodeName string

	// TokenFilePath is where the current SA token is written; the
	// refresher overwrites it atomically before the old token expires.
	// client-go's BearerTokenFile transport picks up the new contents
	// on its next read.
	TokenFilePath string

	// OnRotation, when non-nil, is called after a successful refresh
	// with the new credential. The cmd/outpost wiring uses it to
	// persist the new APIURL/Token/CA into FileConfig — so a future
	// outpost restart starts from the refreshed state without having
	// to re-fetch.
	OnRotation func(*ParsedKubeconfig)
}

// Refresher runs a loop that re-fetches the SA token before it expires
// and writes the new value to TokenFilePath. Single instance per
// outpost process; constructed once and Run()-ed inside the vkpodman
// errgroup.
type Refresher struct {
	deps RefreshDeps
}

// NewRefresher captures deps and returns a Refresher ready to Run.
func NewRefresher(deps RefreshDeps) *Refresher { return &Refresher{deps: deps} }

// Run blocks until ctx is canceled. The loop:
//  1. Decode the current token's exp; sleep until refreshLeadTime
//     before it (or minRefreshInterval, whichever is longer).
//  2. Fetch a new kubeconfig from cloudbox.
//  3. Write the new token to the file.
//  4. Notify OnRotation so the caller can persist.
//  5. Repeat.
//
// On fetch error: log and sleep minRefreshInterval before retrying.
// On context cancel: return cleanly so the errgroup tears down without
// flagging an error.
func (r *Refresher) Run(ctx context.Context, currentToken string) error {
	for {
		wait := nextRefreshDelay(currentToken, time.Now())
		slog.Info("vkpodman: refresh scheduled",
			"in", wait.String(), "node", r.deps.NodeName)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}

		fetched, err := FetchKubeconfig(ctx, r.deps.CloudboxBase, r.deps.AccessToken, r.deps.NodeName)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			// On 503 (cluster mode disabled upstream) we don't want
			// to burn the loop tight; on 401/403 we want to be loud
			// so an operator notices. Both end up in the same backoff
			// path because there's nothing else useful to do from
			// here.
			slog.Warn("vkpodman: refresh failed",
				"err", err, "retry_in", minRefreshInterval.String())
			currentToken = "" // force the next iteration to retry quickly
			continue
		}
		if err := WriteTokenFile(r.deps.TokenFilePath, fetched.Token); err != nil {
			slog.Warn("vkpodman: refresh wrote new token but file update failed",
				"err", err, "path", r.deps.TokenFilePath)
			// Keep going — client-go is still using the old in-memory
			// token until it re-reads the file; we'll try again next
			// cycle. Losing one write isn't worth crashing the agent.
			continue
		}
		if r.deps.OnRotation != nil {
			r.deps.OnRotation(fetched)
		}
		currentToken = fetched.Token
		slog.Info("vkpodman: refresh ok",
			"node", r.deps.NodeName, "next_exp", TokenExpiry(fetched.Token).Format(time.RFC3339))
	}
}

// nextRefreshDelay computes how long to wait before the next refresh.
// Empty token (initial fetch failure, etc.) → retry on minRefreshInterval.
// Token with no parseable exp → retry on minRefreshInterval (we don't
// know when it expires, so be conservative). Token with future exp →
// sleep until refreshLeadTime before, but never less than
// minRefreshInterval so we don't tight-loop on a token cloudbox is
// re-minting with a very short TTL.
func nextRefreshDelay(token string, now time.Time) time.Duration {
	exp := TokenExpiry(token)
	if exp.IsZero() {
		return minRefreshInterval
	}
	target := exp.Add(-refreshLeadTime)
	wait := target.Sub(now)
	if wait < minRefreshInterval {
		return minRefreshInterval
	}
	return wait
}
