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
//
// Ring is an optional deployment-ring tag (e.g. "dev", "test", "stage",
// "prod") that seeds the cloudbox-side host.ring column at first pairing.
// Cloudbox is authoritative after that — admins can re-assign rings from
// the portal SPA, and a subsequent re-pair without --ring will not
// overwrite the admin's value. Used by `POST /api/v1/fleet/upgrade` to
// scope fleet-upgrade fan-out to one cohort (dev → test → stage → prod).
type ExchangeRequest struct {
	ServerURL  string
	Code       string
	Name       string
	Title      string
	AuthURL    string
	ClientOnly bool
	Ring       string
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
		"ring":            strings.TrimSpace(req.Ring),
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

		// Observability fleet-aggregation endpoints, supplied by
		// cloudbox when the operator has installed the cluster
		// observability bundle (VictoriaMetrics / VictoriaLogs / Jaeger).
		// Resolve via cluster Service DNS on the tailscale overlay; empty
		// when the AppStore bundle hasn't been installed.
		MetricsRemoteURL string `json:"metrics_remote_url"`
		LogsRemoteURL    string `json:"logs_remote_url"`
		TracesRemoteURL  string `json:"traces_remote_url"`

		// RecoveryCode is the one-time out-of-band re-pair credential
		// (Layer-4 defense). Returned ONCE at first Exchange. The
		// outpost prints it to stdout + writes 0600 to
		// ~/.config/matrix/recovery_code.txt so the operator can
		// stash it.
		RecoveryCode string `json:"recovery_code"`
	}
	if err := json.Unmarshal(respBody, &ex); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Stash the recovery code OUT-OF-BAND on first appearance.
	// Cloudbox never re-emits it (the SPA can't display it again
	// either), so this side-effect at first Exchange is the only
	// time the plaintext is reachable.
	if ex.RecoveryCode != "" {
		if err := stashRecoveryCode(ex.RecoveryCode); err != nil {
			// Don't fail Exchange — operator can re-pair if the
			// stash fails. But shout so it shows in scrollback.
			fmt.Fprintf(os.Stderr, "WARN: failed to stash recovery code: %v\n", err)
		}
		fmt.Fprintf(os.Stderr,
			"\n==============================================================\n"+
				" Recovery code (Layer-4 out-of-band re-pair credential):\n"+
				"\n   %s\n\n"+
				" Stashed at %s (0600).\n"+
				" Save this in your password manager or print it. It is the\n"+
				" only way to re-pair if BOTH the agent.json AccessToken AND\n"+
				" the matrix tunnel are unrecoverable. Cloudbox will not\n"+
				" re-display it.\n"+
				"==============================================================\n\n",
			ex.RecoveryCode, recoveryCodePath())
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
		ex.OverlayLoginServer != "" || ex.OverlayAuthKey != "" || ex.OverlayPodCIDR != "" ||
		ex.MetricsRemoteURL != "" || ex.LogsRemoteURL != "" || ex.TracesRemoteURL != "" {
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
		fc.Cluster.MetricsRemoteURL = ex.MetricsRemoteURL
		fc.Cluster.LogsRemoteURL = ex.LogsRemoteURL
		fc.Cluster.TracesRemoteURL = ex.TracesRemoteURL
	}
	return fc, nil
}

// ReattachRequest is the input to a bearer-authed re-pair.
//
// Distinct from ExchangeRequest in two ways:
//
//  1. No Code — the bearer (AccessToken from a previous Exchange) is
//     the credential, so the outpost can recover state when the
//     human invite codes are unavailable (cloudbox host row deleted,
//     DB restored from a stale backup, etc.).
//  2. AccessToken is required input, not output — Reattach NEVER
//     re-mints the per-pair JWT (the outpost already has it).
//
// Sent to POST <server>/api/register/reattach, which is the Layer-1
// defense surface added alongside this client.
type ReattachRequest struct {
	ServerURL   string
	AccessToken string
	Name        string
	Title       string
	AuthURL     string
	ClientOnly  bool
}

// Reattach POSTs the bearer to the portal and returns a refreshed
// FileConfig pinned by the portal's response. The caller is responsible
// for merging it with locally-managed fields and saving to disk.
//
// On success, the returned FileConfig carries the SAME AccessToken
// that was passed in (the portal returns "" and we copy it through)
// so callers can `SaveFile` the result without losing identity.
//
// 401/403 is fatal: the bearer is invalid, revoked, or doesn't own
// this name. The caller should fall back to fresh `outpost register`
// with an invite code. 4xx other than auth (e.g. 400 bad name) is
// also fatal. 5xx + network errors retry with the same backoff
// schedule as Exchange.
func Reattach(ctx context.Context, req ReattachRequest) (*conf.FileConfig, error) {
	if strings.TrimSpace(req.AccessToken) == "" {
		return nil, errors.New("reattach: access_token required")
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("reattach: name required")
	}
	title := strings.TrimSpace(req.Title)
	authURL := strings.TrimSpace(req.AuthURL)
	if authURL != "" && title == "" {
		return nil, errors.New("title is required when auth_url is set")
	}

	osUser, _ := hostauth.CurrentUser()
	osDisplay := hostauth.CurrentDisplayName()
	osHostname, _ := os.Hostname()

	payload := map[string]any{
		"name":            req.Name,
		"title":           title,
		"os_user":         osUser,
		"os_display_name": osDisplay,
		"os_hostname":     osHostname,
		"has_auth_url":    authURL != "",
		"client_only":     req.ClientOnly,
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(req.ServerURL, "/") + "/api/register/reattach"

	client := &http.Client{Timeout: 30 * time.Second}
	var respBody []byte
	var lastErr error
	for attempt := 1; attempt <= exchangeMaxAttempts; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+req.AccessToken)

		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("reattach: %w", err)
		} else {
			respBody, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("reattach failed (%d): %s", resp.StatusCode, bytes.TrimSpace(respBody))
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return nil, lastErr
			}
			if attempt < exchangeMaxAttempts {
				delay := time.Duration(1<<(attempt-1)) * time.Second
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
		AccessToken string `json:"access_token"` // always "" on reattach
		ClientOnly  bool   `json:"client_only"`

		ClusterNodeToken   string `json:"cluster_node_token"`
		ClusterSTCPSecret  string `json:"cluster_stcp_secret"`
		ClusterAPIPort     int    `json:"cluster_api_port"`
		ClusterKubeletPort int    `json:"cluster_kubelet_port"`
		OverlayLoginServer string `json:"overlay_login_server"`
		OverlayAuthKey     string `json:"overlay_auth_key"`
		OverlayPodCIDR     string `json:"overlay_pod_cidr"`

		MetricsRemoteURL string `json:"metrics_remote_url"`
		LogsRemoteURL    string `json:"logs_remote_url"`
		TracesRemoteURL  string `json:"traces_remote_url"`
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
		AccessToken: req.AccessToken, // carry the existing bearer through
		ClientOnly:  ex.ClientOnly,
	}
	if ex.ClusterNodeToken != "" || ex.ClusterSTCPSecret != "" ||
		ex.ClusterAPIPort != 0 || ex.ClusterKubeletPort != 0 ||
		ex.OverlayLoginServer != "" || ex.OverlayAuthKey != "" || ex.OverlayPodCIDR != "" ||
		ex.MetricsRemoteURL != "" || ex.LogsRemoteURL != "" || ex.TracesRemoteURL != "" {
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
		fc.Cluster.MetricsRemoteURL = ex.MetricsRemoteURL
		fc.Cluster.LogsRemoteURL = ex.LogsRemoteURL
		fc.Cluster.TracesRemoteURL = ex.TracesRemoteURL
	}
	return fc, nil
}
