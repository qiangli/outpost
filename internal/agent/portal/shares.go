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

// AppShare is one cloudbox app grant for a member.
type AppShare struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	App       string    `json:"app"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
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

// CreateHostShare returns the share and the HTTP status (201 for new, 200
// for an existing identical grant).
func CreateHostShare(ctx context.Context, endpoint, token, host, app, email string) (AppShare, int, error) {
	status, body, err := do(ctx, http.MethodPost, HostSharesURL(endpoint, host), token,
		map[string]string{"app": app, "email": email})
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
