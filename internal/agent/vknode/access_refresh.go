package vknode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AccessEndpointPath is the cloudbox endpoint that lists the namespaces
// permitted to schedule on a given outpost. Owner + per-(host, "podman")
// sharees, in the same hash format the outpost's NamespaceForEmail uses.
const AccessEndpointPath = "/api/v1/cluster/access"

// AccessResponse mirrors hub/internal/handlers/v1_cluster.go's response
// shape. AllowedNamespaces is the union of (owner_namespace, every
// sharee with HostShare(app="podman")) — the outpost replaces its
// Access set with this slice on every successful fetch.
type AccessResponse struct {
	NodeName          string   `json:"node_name"`
	OwnerNamespace    string   `json:"owner_namespace"`
	AllowedNamespaces []string `json:"allowed_namespaces"`
}

// FetchAccess does GET <cloudbox>/api/v1/cluster/access?node_name=<node>
// with Bearer <accessToken>. Returns the allow-list. Reuses FetchError +
// IsClusterDisabled from bootstrap.go for status-code classification so
// the refresher's backoff logic can treat 503 the same as a transient
// network error.
func FetchAccess(ctx context.Context, cloudboxBase, accessToken, nodeName string) (*AccessResponse, error) {
	if strings.TrimSpace(cloudboxBase) == "" {
		return nil, errors.New("vknode: empty cloudboxBase")
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("vknode: empty accessToken")
	}
	if strings.TrimSpace(nodeName) == "" {
		return nil, errors.New("vknode: empty nodeName")
	}
	q := url.Values{"node_name": []string{nodeName}}
	u := strings.TrimRight(cloudboxBase, "/") + AccessEndpointPath + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vknode: dial cloudbox access: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return nil, &FetchError{Status: resp.StatusCode, Message: msg}
	}
	var out AccessResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("vknode: decode access response: %w", err)
	}
	return &out, nil
}

// accessRefreshInterval is the steady-state cadence between successful
// refreshes. HostShares change at human pace (owner clicks share in the
// UI), so 60s is fast enough to feel instant from the operator's
// perspective without spamming cloudbox. Faster than peerhosts.Registry's
// 5-minute TTL because peerhosts is read-driven (lazy) while this is
// push-driven (the outpost polls on a fixed schedule).
const accessRefreshInterval = 60 * time.Second

// accessRefreshFailureBackoff is the cadence used after any fetch error.
// Slower than the success interval so a misconfigured token or briefly
// down cloudbox doesn't generate a flood of failed requests.
const accessRefreshFailureBackoff = 5 * time.Minute

// AccessRefreshDeps is the dependency bundle the AccessRefresher needs.
// Kept analogous to RefreshDeps so the two loops read the same way at
// call sites.
type AccessRefreshDeps struct {
	// CloudboxBase is the HTTPS base URL of cloudbox (no trailing
	// /api/v1/cluster/access), e.g. "https://ai.dhnt.io".
	CloudboxBase string

	// AccessToken is the outpost's matrix access_token. Used as Bearer
	// when calling cloudbox.
	AccessToken string

	// NodeName is the outpost identity to fetch the allow-list for —
	// always fc.AgentName in practice.
	NodeName string

	// Access is the live allow-set the gate consults on every CreatePod.
	// The refresher replaces its contents atomically via Access.Set on
	// each successful fetch. Must be non-nil.
	Access *Access
}

// AccessRefresher polls cloudbox at a fixed cadence and pushes the
// resulting allow-list into the live Access gate. Single instance per
// outpost process; constructed once and Run()-ed inside the vknode
// errgroup alongside the token Refresher.
type AccessRefresher struct {
	deps AccessRefreshDeps
}

// NewAccessRefresher captures deps and returns an AccessRefresher
// ready to Run.
func NewAccessRefresher(deps AccessRefreshDeps) *AccessRefresher {
	return &AccessRefresher{deps: deps}
}

// Run blocks until ctx is canceled. Loop:
//  1. FetchAccess from cloudbox.
//  2. On success: deps.Access.Set(allowed...); log the diff from prior.
//     Wait accessRefreshInterval.
//  3. On error: log + wait accessRefreshFailureBackoff. The existing
//     allow-set stays in place — a transient cloudbox blip never empties
//     the gate (which would reject all sharee pods until the next
//     successful fetch).
func (r *AccessRefresher) Run(ctx context.Context) error {
	for {
		resp, err := FetchAccess(ctx, r.deps.CloudboxBase, r.deps.AccessToken, r.deps.NodeName)
		wait := accessRefreshInterval
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			slog.Warn("vknode: access refresh failed",
				"err", err, "retry_in", accessRefreshFailureBackoff.String(),
				"node", r.deps.NodeName)
			wait = accessRefreshFailureBackoff
		} else {
			before := r.deps.Access.Snapshot()
			r.deps.Access.Set(resp.AllowedNamespaces...)
			if diff := namespaceDiff(before, resp.AllowedNamespaces); diff != "" {
				slog.Info("vknode: access refresh applied",
					"node", r.deps.NodeName,
					"namespaces", resp.AllowedNamespaces,
					"change", diff)
			} else {
				slog.Debug("vknode: access refresh unchanged",
					"node", r.deps.NodeName, "count", len(resp.AllowedNamespaces))
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// namespaceDiff returns a "+added,-removed" summary when the allow-set
// changed and "" otherwise. Useful so operators reading the log see
// share-row edits propagating without parsing the full namespace list.
func namespaceDiff(before, after []string) string {
	prev := make(map[string]struct{}, len(before))
	for _, n := range before {
		prev[n] = struct{}{}
	}
	next := make(map[string]struct{}, len(after))
	for _, n := range after {
		next[n] = struct{}{}
	}
	var added, removed []string
	for n := range next {
		if _, ok := prev[n]; !ok {
			added = append(added, n)
		}
	}
	for n := range prev {
		if _, ok := next[n]; !ok {
			removed = append(removed, n)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}
	sort.Strings(added)
	sort.Strings(removed)
	parts := make([]string, 0, 2)
	if len(added) > 0 {
		parts = append(parts, "+"+strings.Join(added, ","))
	}
	if len(removed) > 0 {
		parts = append(parts, "-"+strings.Join(removed, ","))
	}
	return strings.Join(parts, " ")
}
