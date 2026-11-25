package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HostShareRule is the audience for one cloudbox host app grant.
type HostShareRule struct {
	App     string   `json:"app"`
	Circles []string `json:"circles"`
	Groups  []string `json:"groups"`
	Emails  []string `json:"emails"`
}

// AppShare is one stored host app grant rule.
type AppShare struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	HostShareRule
	Created time.Time `json:"created"`
}

func HostSharesURL(endpoint, host string) string {
	return strings.TrimRight(strings.TrimSpace(endpoint), "/") + "/api/v1/hosts/" + url.PathEscape(host) + "/shares"
}

func ListHostShares(ctx context.Context, endpoint, token, host string) ([]AppShare, error) {
	_, body, err := do(ctx, http.MethodGet, HostSharesURL(endpoint, host), token, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Shares []AppShare `json:"shares"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("shares: bad response: %w", err)
	}
	if out.Shares == nil {
		out.Shares = []AppShare{}
	}
	return out.Shares, nil
}

// CreateHostShare posts one grant rule and returns the stored row and HTTP status.
func CreateHostShare(ctx context.Context, endpoint, token, host string, rule HostShareRule) (AppShare, int, error) {
	// The wire represents unused audiences as empty arrays.
	rule.Circles = append([]string{}, rule.Circles...)
	rule.Groups = append([]string{}, rule.Groups...)
	rule.Emails = append([]string{}, rule.Emails...)
	status, body, err := do(ctx, http.MethodPost, HostSharesURL(endpoint, host), token, rule)
	if err != nil {
		return AppShare{}, status, err
	}
	var out struct {
		Share AppShare `json:"share"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Share.ID == "" {
		return AppShare{}, status, fmt.Errorf("shares: bad create response")
	}
	return out.Share, status, nil
}

func DeleteHostShare(ctx context.Context, endpoint, token, host, id string) error {
	_, _, err := do(ctx, http.MethodDelete, HostSharesURL(endpoint, host)+"/"+url.PathEscape(id), token, nil)
	return err
}
