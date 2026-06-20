// Layer-4 out-of-band recovery code persistence + recover client.
//
// The recovery code is the credential of last resort when BOTH the
// agent.json AccessToken (Reattach) AND the matrix tunnel are
// unrecoverable. Cloudbox returns the plaintext once at first
// Exchange; we stash it at ~/.config/matrix/recovery_code.txt (mode
// 0600) so the operator can also copy it to a password manager /
// paper / wherever.

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
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// recoveryCodePath returns the canonical on-disk location for the
// stashed recovery code. Sits next to agent.json (same XDG-aware
// directory), 0600 perms, name fixed.
func recoveryCodePath() string {
	if cfg, err := conf.DefaultConfigPath(); err == nil && cfg != "" {
		return filepath.Join(filepath.Dir(cfg), "recovery_code.txt")
	}
	// Fallback when DefaultConfigPath fails (shouldn't, but the
	// stash is best-effort — Exchange itself doesn't fail on us).
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "matrix", "recovery_code.txt")
	}
	return "/tmp/recovery_code.txt"
}

// stashRecoveryCode writes the plaintext code to a 0600 file. The
// file is opened with O_TRUNC so a re-pair (which mints a new code)
// overwrites the previous stash atomically.
func stashRecoveryCode(code string) error {
	p := recoveryCodePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(p), err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	body := fmt.Sprintf(
		"# outpost recovery code — Layer-4 out-of-band re-pair credential.\n"+
			"# Cloudbox does not re-emit this. Keep it in your password\n"+
			"# manager (or print it). Used by:\n"+
			"#   outpost register --recovery-code <code> --name <name>\n"+
			"# Generated %s\n\n%s\n",
		time.Now().UTC().Format(time.RFC3339), code)
	if _, err := f.WriteString(body); err != nil {
		return err
	}
	return nil
}

// LoadStashedRecoveryCode reads the previously-stashed code, if any.
// Strips the # comment lines so the caller gets the bare code. Empty
// string with no error means "no stash on disk."
func LoadStashedRecoveryCode() (string, error) {
	p := recoveryCodePath()
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, nil
	}
	return "", nil
}

// RecoverRequest is the input to the out-of-band re-pair flow.
type RecoverRequest struct {
	ServerURL    string
	Name         string
	RecoveryCode string
	Title        string
	AuthURL      string
	ClientOnly   bool
}

// Recover POSTs the recovery code to /api/register/recover. Returns
// a FileConfig pinned to the response just like Exchange, including
// a fresh AccessToken (the previous one is revoked server-side as
// part of the recovery — the whole point is that the operator is
// declaring the previous bearer gone). A NEW recovery code is also
// returned in the response and re-stashed locally.
func Recover(ctx context.Context, req RecoverRequest) (*conf.FileConfig, error) {
	if strings.TrimSpace(req.RecoveryCode) == "" {
		return nil, errors.New("recover: recovery_code required")
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("recover: name required")
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
		"recovery_code":   req.RecoveryCode,
		"title":           title,
		"os_user":         osUser,
		"os_display_name": osDisplay,
		"os_hostname":     osHostname,
		"has_auth_url":    authURL != "",
		"client_only":     req.ClientOnly,
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(req.ServerURL, "/") + "/api/register/recover"

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
			lastErr = fmt.Errorf("recover: %w", err)
		} else {
			respBody, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("recover failed (%d): %s", resp.StatusCode, bytes.TrimSpace(respBody))
			// 429 is retryable backpressure (admission control); other
			// 4xx are terminal.
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && !retryableStatus(resp.StatusCode) {
				return nil, lastErr
			}
			if attempt < exchangeMaxAttempts {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(retryDelay(attempt, resp)):
				}
				continue
			}
		}
		if attempt < exchangeMaxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay(attempt, nil)):
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}

	var ex struct {
		AgentName    string `json:"agent_name"`
		ServerAddr   string `json:"server_addr"`
		ServerPort   int    `json:"server_port"`
		Protocol     string `json:"protocol"`
		Token        string `json:"token"`
		RemotePort   int    `json:"remote_port"`
		AccessToken  string `json:"access_token"`
		ClientOnly   bool   `json:"client_only"`
		RecoveryCode string `json:"recovery_code"`
	}
	if err := json.Unmarshal(respBody, &ex); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Rotate the on-disk stash to the new code cloudbox issued.
	if ex.RecoveryCode != "" {
		if err := stashRecoveryCode(ex.RecoveryCode); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: failed to re-stash recovery code: %v\n", err)
		}
		fmt.Fprintf(os.Stderr,
			"NEW recovery code issued (rotated):\n  %s\nStashed at %s.\n",
			ex.RecoveryCode, recoveryCodePath())
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
		ClientOnly:  ex.ClientOnly,
	}, nil
}
