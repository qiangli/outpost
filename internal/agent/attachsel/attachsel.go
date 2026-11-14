// Package attachsel picks, for a client command that targets a host, the
// satellite attachment whose satellite actually knows that host (Sprint 630
// S15b). A pair-once host attached to several satellites used to overlay
// attachments[0] for every client call, so `outpost connect` / `ssh` to a
// host that lives only on the second satellite answered "unknown host".
package attachsel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// HTTPClient is the client used to ask each satellite; tests swap it.
var HTTPClient = &http.Client{Timeout: 8 * time.Second}

// LoadFor loads agent.json for a client command targeting host. A legacy
// pairing, a single attachment, or an empty host keep the LoadClientFile
// behaviour. With several attachments each satellite's /api/v1/ssh/hosts is
// asked in order and the first one listing host is overlaid; when none
// does (or every call fails) the first attachment is the fallback, exactly
// as before, so the caller's error stays "unknown host" at the satellite.
func LoadFor(ctx context.Context, cfgPath, host string) (*conf.FileConfig, error) {
	fc, err := conf.LoadFile(cfgPath)
	if err != nil || fc == nil {
		return fc, err
	}
	host = strings.TrimSpace(host)
	if fc.LegacyPaired() || len(fc.Attachments) <= 1 || host == "" {
		fc.ApplyAttachmentTunnel()
		return fc, nil
	}
	for i := range fc.Attachments {
		cand, lerr := conf.LoadFile(cfgPath)
		if lerr != nil || cand == nil || !cand.ApplyAttachmentTunnelAt(i) {
			continue
		}
		bearer := strings.TrimSpace(cand.AccessToken)
		if bearer == "" {
			bearer = strings.TrimSpace(cand.Token)
		}
		if bearer == "" {
			continue
		}
		if knows, kerr := satelliteKnowsHost(ctx, cand.ServerAddr, cand.ServerPort, cand.Protocol, bearer, host); kerr == nil && knows {
			return cand, nil
		}
	}
	fc.ApplyAttachmentTunnel()
	return fc, nil
}

// satelliteKnowsHost asks one satellite whether host is among the hosts
// this account may reach there (GET /api/v1/ssh/hosts).
func satelliteKnowsHost(ctx context.Context, server string, port int, protocol, token, host string) (bool, error) {
	s := strings.TrimSpace(server)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(strings.TrimSpace(protocol), "wss") || strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	if u.Port() == "" && port > 0 {
		u.Host = u.Hostname() + ":" + strconv.Itoa(port)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/ssh/hosts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Hosts []struct {
			Host string `json:"host"`
		} `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	for _, h := range out.Hosts {
		if strings.EqualFold(strings.TrimSpace(h.Host), host) {
			return true, nil
		}
	}
	return false, nil
}
