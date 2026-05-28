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
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(req.ServerURL, "/")+"/api/register/exchange",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("exchange: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange failed (%d): %s", resp.StatusCode, bytes.TrimSpace(respBody))
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
		ClusterNodeToken  string `json:"cluster_node_token"`
		ClusterSTCPSecret string `json:"cluster_stcp_secret"`
		ClusterAPIPort    int    `json:"cluster_api_port"`
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
	if ex.ClusterNodeToken != "" || ex.ClusterSTCPSecret != "" || ex.ClusterAPIPort != 0 {
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		fc.Cluster.NodeToken = ex.ClusterNodeToken
		fc.Cluster.STCPSecret = ex.ClusterSTCPSecret
		fc.Cluster.K8sAPIPort = ex.ClusterAPIPort
	}
	return fc, nil
}
