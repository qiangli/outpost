package vknode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/localsock"
)

// apiPrefix is the versioned path libpod expects. Earlier we used the
// unversioned `/libpod/*` form, but the real podman socket alias only
// kicks in under the official `podman system service` entry point;
// wrapper sockets (e.g. ycode's curated libpod surface, which serves
// requests through podman-system-service-equivalent code but doesn't
// implement the unversioned alias) reject `/libpod/...` with 404 and
// require the explicit version. Pinning here keeps us compatible with
// both shapes and decouples us from server-side version negotiation.
const apiPrefix = "/v5.0.0"

// Client is a thin HTTP client for the local libpod REST API. It speaks
// the versioned `/v5.0.0/libpod/*` path tree — compatible with any
// podman 5.x/6.x daemon (the path major is stable across releases).
type Client struct {
	// http is the underlying transport. Its DialContext is wired to the
	// configured socket; Host in the URL is a synthetic placeholder
	// ("podman") that the daemon ignores.
	http *http.Client
}

// NewClient returns a Client that dials the libpod REST API at the given
// local socket. The socket must already exist and be reachable; callers
// typically obtain its path from agent.DetectPodman().
//
// The transport is chosen from the path shape by localsock, so this
// works against a unix socket (Linux, macOS — and Windows, where podman
// also publishes %TEMP%\podman\<machine>-api.sock) and against a Windows
// NAMED PIPE (`\\.\pipe\podman-<machine>`). Hardcoding a unix dial here
// is what previously made `--cluster-mode=vk-podman` unreachable on
// Windows: podman's default endpoint there is the pipe.
func NewClient(socket string) (*Client, error) {
	if strings.TrimSpace(socket) == "" {
		return nil, errors.New("vknode: empty podman socket path")
	}
	tr := &http.Transport{
		DialContext: localsock.DialFunc(socket),
		// libpod logs/wait/attach streams are long-lived. Disable the
		// per-request timeout entirely — individual calls set their own
		// context deadlines.
		IdleConnTimeout: 90 * time.Second,
	}
	return &Client{http: &http.Client{Transport: tr}}, nil
}

// Ping checks that the daemon is responsive. Used by NodeProvider.Ping
// as the node heartbeat. We hit /libpod/_ping rather than /info because
// _ping returns "OK\n" and is the cheapest endpoint libpod exposes.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, apiPrefix+"/libpod/_ping", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("ping", resp)
	}
	return nil
}

// do issues a request against the libpod REST API. body, when non-nil,
// is JSON-encoded; query, when non-nil, is appended as URL-encoded.
// The returned response's Body must be closed by the caller — do does
// not drain on success because some endpoints (logs, wait, exec/start)
// stream.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	u := "http://podman" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("vknode: marshal %s body: %w", path, err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}

// statusErr builds a wrapped error from a non-2xx libpod response.
// Libpod returns JSON errors of the form {"cause":"...", "message":"...",
// "response":<code>}; surface the message when available, fall back to
// the raw body otherwise.
func statusErr(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var libErr struct {
		Cause    string `json:"cause"`
		Message  string `json:"message"`
		Response int    `json:"response"`
	}
	if json.Unmarshal(body, &libErr) == nil && libErr.Message != "" {
		return &APIError{Op: op, Status: resp.StatusCode, Message: libErr.Message}
	}
	return &APIError{Op: op, Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
}

// APIError is returned when the libpod daemon answers a non-2xx status.
// Status carries the HTTP code so callers can distinguish 404 (NotFound)
// from 409 (Conflict) etc. — both are common shapes during reconnect
// reconciliation where "already exists" / "no such container" are
// expected rather than fatal.
type APIError struct {
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil APIError>"
	}
	return fmt.Sprintf("vknode: %s: HTTP %d: %s", e.Op, e.Status, e.Message)
}

// IsNotFound reports whether err describes a libpod 404 (container or
// image not found). Useful for idempotent delete/stop paths that should
// treat "already gone" as success.
func IsNotFound(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusNotFound
	}
	return false
}

// IsConflict reports whether err describes a libpod 409 — typically
// "container already in the requested state" (e.g. start on a running
// container, stop on a stopped one). Useful for the same idempotent
// patterns as IsNotFound.
func IsConflict(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusConflict
	}
	return false
}
