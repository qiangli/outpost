// Package portal speaks to the cloud portal's pairing endpoint
// (POST /api/register/exchange). Both the `outpost register` CLI and the
// admin UI route their pairing through here so the request/response
// contract has exactly one definition.
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
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// exchangeMaxAttempts caps the number of POST tries before giving up.
// With the 1s/2s/4s backoff below this is ~7 s of total wait — enough
// to ride out a portal-replica restart on App Platform without
// blocking the CLI for too long.
const exchangeMaxAttempts = 4

// ExchangeRequest is the input to a pairing exchange.
//
// AuthURL, when non-empty, tells the portal that the agent will delegate
// /auth to an external endpoint — so the portal cannot derive a subtitle
// from an OS identity and Title is required.
//
// ClientOnly marks this registration as a credential-only pairing — the
// machine will use the resulting access_token to ssh OUT to other paired
// hosts but never accepts inbound traffic. Cloudbox skips the matrix-
// tunnel port allocation, the launcher hides the row from its tile
// grid, and `outpost start` becomes a no-op tunnel-less stub.
type ExchangeRequest struct {
	ServerURL  string
	Code       string
	Name       string
	Title      string
	AuthURL    string
	ClientOnly bool
}

// Exchange POSTs the pairing code to the portal and returns the FileConfig
// pinned by the portal's response. The caller is responsible for merging
// it with any locally-managed fields (Apps, built-in toggles) and saving
// to disk.
func Exchange(ctx context.Context, req ExchangeRequest) (*conf.FileConfig, error) {
	title := strings.TrimSpace(req.Title)
	authURL := strings.TrimSpace(req.AuthURL)
	// A custom auth URL means the app users have no OS identity, so the
	// OS-derived subtitle would be misleading. Require a human title.
	if authURL != "" && title == "" {
		return nil, errors.New("title is required when auth_url is set (no OS user to derive a subtitle from)")
	}

	// Report OS-side identity so the portal can render a disambiguating
	// subtitle and the elevate form can prefill the username. Best-effort.
	osUser, _ := hostauth.CurrentUser()
	osDisplay := hostauth.CurrentDisplayName()
	osHostname, _ := os.Hostname()

	payload := map[string]any{
		"code":            req.Code,
		"name":            req.Name,
		"title":           title,
		"os_user":         osUser,
		"os_display_name": osDisplay,
		"os_hostname":     osHostname,
		"has_auth_url":    authURL != "",
		"client_only":     req.ClientOnly,
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(req.ServerURL, "/") + "/api/register/exchange"

	// Retry the POST on transient failures (network errors + 5xx) so
	// pairing survives a cloud portal-replica restart. Cloudbox's split
	// deploy can return 503 with Retry-After during the brief window
	// when portal replicas are rolling — without retry, a single 503
	// kills `outpost register`. Stops on 2xx (success), 4xx (caller
	// error, won't fix itself), or attempt exhaustion.
	client := &http.Client{Timeout: 30 * time.Second}
	var respBody []byte
	var lastErr error
	for attempt := 1; attempt <= exchangeMaxAttempts; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("exchange: %w", err)
		} else {
			respBody, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("exchange failed (%d): %s", resp.StatusCode, bytes.TrimSpace(respBody))
			// 4xx — caller's problem (bad code, name collision, etc.).
			// Retrying won't help; surface the error immediately.
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return nil, lastErr
			}
			// 5xx — honor Retry-After if present, else exp backoff.
			if attempt < exchangeMaxAttempts {
				delay := time.Duration(1<<(attempt-1)) * time.Second // 1s, 2s, 4s
				if ra := resp.Header.Get("Retry-After"); ra != "" {
					if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
						delay = time.Duration(secs) * time.Second
					}
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
				continue
			}
		}
		// Network error path — same backoff, but no Retry-After to read.
		if attempt < exchangeMaxAttempts {
			delay := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}

	var ex struct {
		AgentName   string `json:"agent_name"`
		ServerAddr  string `json:"server_addr"`
		ServerPort  int    `json:"server_port"`
		Protocol    string `json:"protocol"`
		Token       string `json:"token"`
		RemotePort  int    `json:"remote_port"`
		AccessToken string `json:"access_token"`
		ClientOnly  bool   `json:"client_only"`

		// Cluster join data — populated only when cloudbox is running
		// in cluster mode AND has already materialized the node-token.
		// Outpost persists into ClusterConfig.{NodeToken,STCPSecret,
		// K8sAPIPort} so the daemon can spin up the k3s-agent path on
		// next boot (operator still has to flip --cluster-mode=agent).
		ClusterNodeToken   string `json:"cluster_node_token"`
		ClusterSTCPSecret  string `json:"cluster_stcp_secret"`
		ClusterAPIPort     int    `json:"cluster_api_port"`
		ClusterKubeletPort int    `json:"cluster_kubelet_port"`
		OverlayLoginServer string `json:"overlay_login_server"`
		OverlayAuthKey     string `json:"overlay_auth_key"`
		OverlayPodCIDR     string `json:"overlay_pod_cidr"`
	}
	if err := json.Unmarshal(respBody, &ex); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	fc := &conf.FileConfig{
		AgentName:   ex.AgentName,
		ServerAddr:  ex.ServerAddr,
		ServerPort:  ex.ServerPort,
		Protocol:    ex.Protocol,
		Token:       ex.Token,
		RemotePort:  ex.RemotePort,
		AuthURL:     authURL,
		AccessToken: ex.AccessToken,
		ClientOnly:  ex.ClientOnly,
	}
	// Carry the cluster-join bits onto the persisted ClusterConfig
	// when cloudbox returned them. Mode stays empty here — the
	// operator opts into Mode="agent" via the builtins toggle, which
	// preserves backward compat for outposts that flip --cluster=on
	// expecting vkpodman.
	if ex.ClusterNodeToken != "" || ex.ClusterSTCPSecret != "" ||
		ex.ClusterAPIPort != 0 || ex.ClusterKubeletPort != 0 ||
		ex.OverlayLoginServer != "" || ex.OverlayAuthKey != "" || ex.OverlayPodCIDR != "" {
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		fc.Cluster.NodeToken = ex.ClusterNodeToken
		fc.Cluster.STCPSecret = ex.ClusterSTCPSecret
		fc.Cluster.K8sAPIPort = ex.ClusterAPIPort
		fc.Cluster.KubeletProxyPort = ex.ClusterKubeletPort
		fc.Cluster.OverlayLoginServer = ex.OverlayLoginServer
		fc.Cluster.OverlayAuthKey = ex.OverlayAuthKey
		fc.Cluster.OverlayPodCIDR = ex.OverlayPodCIDR
	}
	return fc, nil
}
