// Package userkube owns the workflow for materializing a kubectl-
// ready kubeconfig from cloudbox onto this host's disk. Used by:
//
//   - the daemon at startup (when fc.Cluster.Enabled is on) so
//     kubectl / helm work without the operator running any extra
//     command;
//   - the admin UI's Cluster section "Refresh" button so the
//     operator can re-mint after the token rotates server-side;
//   - the `outpost cluster kubeconfig` CLI, which now defaults to
//     writing the file (was stdout).
//
// The package is intentionally tiny — fetch via vkpodman, render
// to YAML, write atomically. Path resolution + observability
// (LastStatus for the UI) live here so all three callers stay in
// sync.
package userkube

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent/vkpodman"
)

// DefaultFilename is what kubectl-readable kubeconfig gets written
// as inside the chosen directory.
const DefaultFilename = "outpost.yaml"

// Path returns the canonical place to write the kubectl-ready
// kubeconfig:
//
//  1. $OUTPOST_KUBECONFIG_PATH override (operator-set)
//  2. $HOME/.kube/<DefaultFilename>
//
// Returns "" only on systems with no resolvable HOME — caller
// should surface a useful error to the operator in that case.
func Path() string {
	if p := strings.TrimSpace(os.Getenv("OUTPOST_KUBECONFIG_PATH")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kube", DefaultFilename)
}

// Status carries the most recent FetchAndWrite outcome. The admin
// UI's Cluster section renders this to show "kubectl is ready" +
// a useful error when it isn't.
type Status struct {
	Path        string    `json:"path"`
	Exists      bool      `json:"exists"`
	LastRefresh time.Time `json:"last_refresh,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	NodeName    string    `json:"node_name,omitempty"`
	APIURL      string    `json:"api_url,omitempty"`
}

var (
	statusMu sync.RWMutex
	status   Status
)

// LastStatus returns a snapshot of the most recent FetchAndWrite
// state. Safe to call concurrently with refreshes.
func LastStatus() Status {
	statusMu.RLock()
	defer statusMu.RUnlock()
	out := status
	out.Path = Path()
	if out.Path != "" {
		if _, err := os.Stat(out.Path); err == nil {
			out.Exists = true
		}
	}
	return out
}

// FetchAndWrite reaches cloudbox for fresh kubeconfig credentials,
// renders the minimal kubectl-ready YAML, and writes to outPath
// atomically (write to .tmp + rename). When outPath is empty,
// resolves via Path(). Updates the package-level Status either way.
func FetchAndWrite(ctx context.Context, cloudboxBase, accessToken, nodeName, outPath string) (string, error) {
	if outPath == "" {
		outPath = Path()
	}
	if outPath == "" {
		err := errors.New("no kubeconfig path — set OUTPOST_KUBECONFIG_PATH or ensure HOME resolves")
		recordStatus(outPath, nodeName, "", err)
		return "", err
	}
	if strings.TrimSpace(cloudboxBase) == "" {
		err := errors.New("no cloudbox URL — host is not paired yet")
		recordStatus(outPath, nodeName, "", err)
		return outPath, err
	}
	if strings.TrimSpace(accessToken) == "" {
		err := errors.New("no access_token — run `outpost register` first")
		recordStatus(outPath, nodeName, "", err)
		return outPath, err
	}
	if strings.TrimSpace(nodeName) == "" {
		err := errors.New("no node name — pass --node or set fc.AgentName")
		recordStatus(outPath, nodeName, "", err)
		return outPath, err
	}

	parsed, err := vkpodman.FetchKubeconfig(ctx, cloudboxBase, accessToken, nodeName)
	if err != nil {
		recordStatus(outPath, nodeName, "", fmt.Errorf("fetch: %w", err))
		return outPath, fmt.Errorf("fetch kubeconfig: %w", err)
	}

	yaml := Render(nodeName, parsed)
	if err := writeAtomic(outPath, yaml); err != nil {
		recordStatus(outPath, nodeName, parsed.APIURL, err)
		return outPath, fmt.Errorf("write %s: %w", outPath, err)
	}
	recordStatus(outPath, nodeName, parsed.APIURL, nil)
	return outPath, nil
}

// Render returns the minimal kubeconfig YAML kubectl needs: one
// cluster, one user, one context, current-context set. CA inlined
// as certificate-authority-data when present; empty CA means trust
// the system roots, which is what cloudbox-fronted HTTPS through a
// real public cert wants.
//
// String built by hand to keep the surface tiny + the import set
// light — no sigs.k8s.io/yaml dep just to emit four stanzas.
func Render(contextName string, p *vkpodman.ParsedKubeconfig) string {
	clusterName := "outpost-cluster"
	userName := "outpost-" + contextName
	caField := ""
	if p != nil && len(p.CA) > 0 {
		caField = "    certificate-authority-data: " + base64.StdEncoding.EncodeToString(p.CA) + "\n"
	}
	apiURL := ""
	if p != nil {
		apiURL = p.APIURL
	}
	token := ""
	if p != nil {
		token = p.Token
	}
	return "apiVersion: v1\n" +
		"kind: Config\n" +
		"clusters:\n" +
		"- name: " + clusterName + "\n" +
		"  cluster:\n" +
		"    server: " + apiURL + "\n" +
		caField +
		"users:\n" +
		"- name: " + userName + "\n" +
		"  user:\n" +
		"    token: " + token + "\n" +
		"contexts:\n" +
		"- name: " + contextName + "\n" +
		"  context:\n" +
		"    cluster: " + clusterName + "\n" +
		"    user: " + userName + "\n" +
		"current-context: " + contextName + "\n"
}

// writeAtomic creates parent dirs, writes to <path>.tmp, then
// renames over <path>. 0600 — kubeconfig contains a bearer token.
func writeAtomic(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func recordStatus(path, node, apiURL string, err error) {
	statusMu.Lock()
	defer statusMu.Unlock()
	status.Path = path
	status.NodeName = node
	status.APIURL = apiURL
	status.LastRefresh = time.Now().UTC()
	if err != nil {
		status.LastError = err.Error()
	} else {
		status.LastError = ""
	}
}
