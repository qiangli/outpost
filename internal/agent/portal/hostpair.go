package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// The pair-once contract with the cloudbox master — yoke
// docs/cloudbox-members-attach-wire.md §3. Pairing there is an identity
// record only: it returns a host token and `tunnel: false`, never tunnel
// credentials.

// ErrNoHostPair reports that the server does not serve POST
// /api/v1/hosts/pair (a satellite or standalone server, which pairs over the
// legacy /api/register/exchange instead). The code was not consumed.
var ErrNoHostPair = errors.New("server does not serve /api/v1/hosts/pair")

// APIError is the contract's error envelope (§1). Code is the stable
// machine token callers branch on.
type APIError struct {
	Status int    `json:"-"`
	Code   string `json:"code"`
	Msg    string `json:"error"`
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("cloudbox: HTTP %d: %s", e.Status, e.Msg)
	}
	return fmt.Sprintf("cloudbox: %s (%d): %s", e.Code, e.Status, e.Msg)
}

func apiError(status int, body []byte) *APIError {
	e := &APIError{Status: status}
	if json.Unmarshal(body, e) != nil || (e.Code == "" && e.Msg == "") {
		e.Code, e.Msg = "", strings.TrimSpace(string(body))
	}
	return e
}

// HostPairRequest is the body of POST /api/v1/hosts/pair.
type HostPairRequest struct {
	ServerURL string `json:"-"`
	Code      string `json:"code"`
	Host      string `json:"host"`
	Hostname  string `json:"hostname,omitempty"`
	OS        string `json:"os,omitempty"`
	Arch      string `json:"arch,omitempty"`
}

// HostPairURL is the endpoint HostPair POSTs to for a server URL.
func HostPairURL(server string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/") + "/api/v1/hosts/pair"
}

// HostSelfURL is the endpoint HostUnpair DELETEs for a server URL.
func HostSelfURL(server string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/") + "/api/v1/hosts/self"
}

// HostPair redeems a pairing code at the cloudbox master and returns the
// identity record to persist as FileConfig.Cloudbox. ErrNoHostPair means the
// server is not the master (fall back to Exchange). A `tunnel: true` reply
// is a different contract and is refused.
//
// Retries on 429/5xx/network errors are safe: a replay with the same code
// and host returns the same record and token (§3 natural idempotency).
func HostPair(ctx context.Context, req HostPairRequest) (*conf.CloudboxPairing, error) {
	if strings.TrimSpace(req.Code) == "" || strings.TrimSpace(req.Host) == "" {
		return nil, errors.New("pair: code and host are required")
	}
	if req.Hostname == "" {
		req.Hostname, _ = os.Hostname()
	}
	if req.OS == "" {
		req.OS = runtime.GOOS
	}
	if req.Arch == "" {
		req.Arch = runtime.GOARCH
	}
	body, _ := json.Marshal(req)
	url := HostPairURL(req.ServerURL)

	client := &http.Client{Timeout: 30 * time.Second}
	var lastErr error
	for attempt := 1; attempt <= exchangeMaxAttempts; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(httpReq)
		var respBody []byte
		if err == nil {
			respBody, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		switch {
		case err != nil:
			lastErr = fmt.Errorf("pair: %w", err)
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
			return decodeHostPair(req.ServerURL, respBody)
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
			// Route absent: not the master. The contract never answers
			// this endpoint with 404 (an unknown code is 401 invalid_code).
			return nil, ErrNoHostPair
		case !retryableStatus(resp.StatusCode):
			return nil, apiError(resp.StatusCode, respBody)
		default:
			lastErr = apiError(resp.StatusCode, respBody)
		}
		if attempt < exchangeMaxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay(attempt, resp)):
			}
		}
	}
	return nil, lastErr
}

func decodeHostPair(server string, body []byte) (*conf.CloudboxPairing, error) {
	var out struct {
		Host struct {
			Host       string    `json:"host"`
			OwnerEmail string    `json:"owner_email"`
			PairedAt   time.Time `json:"paired_at"`
		} `json:"host"`
		Token  string   `json:"token"`
		Scopes []string `json:"scopes"`
		Tunnel *bool    `json:"tunnel"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Tunnel == nil {
		// A 2xx without the contract's shape (e.g. an SPA fallback page):
		// this is not the master's pair endpoint.
		return nil, ErrNoHostPair
	}
	if *out.Tunnel {
		return nil, errors.New("pair: server answered tunnel:true — not the identity-only pairing contract; refusing")
	}
	if out.Token == "" || out.Host.Host == "" {
		return nil, errors.New("pair: response is missing host or token")
	}
	return &conf.CloudboxPairing{
		PortalURL:  strings.TrimRight(strings.TrimSpace(server), "/"),
		Host:       out.Host.Host,
		Token:      out.Token,
		Scopes:     out.Scopes,
		OwnerEmail: out.Host.OwnerEmail,
		PairedAt:   out.Host.PairedAt,
	}, nil
}

// HostUnpair deletes this host's record at the master (DELETE
// /api/v1/hosts/self). 204 is success; 401 means the token is already gone,
// which the contract says to treat as done.
func HostUnpair(ctx context.Context, server, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, HostSelfURL(server), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unpair: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	return apiError(resp.StatusCode, b)
}
