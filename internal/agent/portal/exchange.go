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
type ExchangeRequest struct {
	ServerURL string
	Code      string
	Name      string
	Title     string
	AuthURL   string
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
	}
	if err := json.Unmarshal(respBody, &ex); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &conf.FileConfig{
		AgentName:   ex.AgentName,
		ServerAddr:  ex.ServerAddr,
		ServerPort:  ex.ServerPort,
		Protocol:    ex.Protocol,
		Token:       ex.Token,
		RemotePort:  ex.RemotePort,
		AuthURL:     authURL,
		AccessToken: ex.AccessToken,
	}, nil
}
