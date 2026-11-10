package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Satellite attach — yoke docs/cloudbox-members-attach-wire.md §4. The host
// asks the master for an attach token with its host token, then redeems it
// at the satellite for a satellite host token. The satellite's attach
// response also carries `registration` (the /api/register/exchange tunnel
// fields), which is what lets the daemon open its tunnel there.

// Satellite is one row of the host-token view of GET /api/v1/satellites.
type Satellite struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	State    string `json:"state,omitempty"`
	Attached bool   `json:"attached"`
}

// AttachGrant is the master's reply to POST /api/v1/satellites/:id/attach.
type AttachGrant struct {
	AttachToken string    `json:"attach_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	Satellite   Satellite `json:"satellite"`
	Host        string    `json:"host"`
}

// SatellitesURL is the master's satellite list endpoint.
func SatellitesURL(master string) string {
	return strings.TrimRight(strings.TrimSpace(master), "/") + "/api/v1/satellites"
}

// SatelliteAttachURL is the master's attach/detach endpoint for one satellite.
func SatelliteAttachURL(master, id string) string {
	return SatellitesURL(master) + "/" + url.PathEscape(id) + "/attach"
}

// HostAttachURL is the satellite's redeem endpoint.
func HostAttachURL(endpoint string) string {
	return strings.TrimRight(strings.TrimSpace(endpoint), "/") + "/api/v1/hosts/attach"
}

var satelliteClient = &http.Client{Timeout: 30 * time.Second}

// do sends one bearer request and returns the body of a 2xx reply, or the
// contract error envelope as *APIError.
func do(ctx context.Context, method, u, bearer string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := satelliteClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, b, apiError(resp.StatusCode, b)
	}
	return resp.StatusCode, b, nil
}

// ListSatellites returns the satellites this host may attach to, each with
// its `attached` flag (GET /api/v1/satellites, host token).
func ListSatellites(ctx context.Context, master, hostToken string) ([]Satellite, error) {
	_, b, err := do(ctx, http.MethodGet, SatellitesURL(master), hostToken, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Satellites []Satellite `json:"satellites"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("satellites: bad response: %w", err)
	}
	if out.Satellites == nil {
		out.Satellites = []Satellite{}
	}
	return out.Satellites, nil
}

// RequestAttach asks the master for a single-use attach token for one
// satellite (POST /api/v1/satellites/:id/attach, host token).
func RequestAttach(ctx context.Context, master, hostToken, satelliteID string) (*AttachGrant, error) {
	_, b, err := do(ctx, http.MethodPost, SatelliteAttachURL(master, satelliteID), hostToken, nil)
	if err != nil {
		return nil, err
	}
	var g AttachGrant
	if err := json.Unmarshal(b, &g); err != nil || g.AttachToken == "" || g.Satellite.Endpoint == "" {
		return nil, errors.New("attach: master response is missing attach_token or satellite endpoint")
	}
	if g.Satellite.ID == "" {
		g.Satellite.ID = satelliteID
	}
	return &g, nil
}

// RedeemAttach presents the attach token at the satellite (POST
// /api/v1/hosts/attach) and returns the attachment to persist. A replay of
// the same token and body is idempotent (§4), so transient failures are
// safe for the caller to retry.
func RedeemAttach(ctx context.Context, g *AttachGrant, host string) (*conf.SatelliteAttachment, error) {
	hostname, _ := os.Hostname()
	body := map[string]string{"host": host, "hostname": hostname, "os": runtime.GOOS, "arch": runtime.GOARCH}
	_, b, err := do(ctx, http.MethodPost, HostAttachURL(g.Satellite.Endpoint), g.AttachToken, body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Host struct {
			Host       string    `json:"host"`
			AttachedAt time.Time `json:"attached_at"`
		} `json:"host"`
		Token        string                 `json:"token"`
		Scopes       []string               `json:"scopes"`
		SatelliteID  string                 `json:"satellite_id"`
		Registration *conf.AttachmentTunnel `json:"registration"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Token == "" {
		return nil, errors.New("attach: satellite response is missing the satellite host token")
	}
	id := out.SatelliteID
	if id == "" {
		id = g.Satellite.ID
	}
	return &conf.SatelliteAttachment{
		SatelliteID: id,
		Name:        g.Satellite.Name,
		Endpoint:    strings.TrimRight(strings.TrimSpace(g.Satellite.Endpoint), "/"),
		Host:        out.Host.Host,
		Token:       out.Token,
		Scopes:      out.Scopes,
		AttachedAt:  out.Host.AttachedAt,
		Tunnel:      out.Registration,
	}, nil
}

// Detach removes the assignment at the master (DELETE
// /api/v1/satellites/:id/attach, host token). Idempotent: 204 every time.
func Detach(ctx context.Context, master, hostToken, satelliteID string) error {
	_, _, err := do(ctx, http.MethodDelete, SatelliteAttachURL(master, satelliteID), hostToken, nil)
	return err
}

// LeaveSatellite deletes this host's record at the satellite (DELETE
// /api/v1/hosts/self, satellite host token). 401 means the token is
// already gone, which the contract says to treat as done.
func LeaveSatellite(ctx context.Context, endpoint, satelliteToken string) error {
	status, _, err := do(ctx, http.MethodDelete, HostSelfURL(endpoint), satelliteToken, nil)
	if status == http.StatusUnauthorized {
		return nil
	}
	return err
}
