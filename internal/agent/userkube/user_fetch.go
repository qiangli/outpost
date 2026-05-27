package userkube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// UserKubeconfigEndpoint is cloudbox's user-mode kubeconfig issuer.
// The route accepts the outpost's access_token as a Bearer (via
// middleware.DecodeUser, which trusts any cloudbox-signed JWT) and
// mints a per-user ServiceAccount token in the outpost-users
// namespace scoped to the calling account.
const UserKubeconfigEndpoint = "/api/cluster/userkubeconfig"

// FetchUserKubeconfigYAML POSTs to cloudbox's user-kubeconfig issuer
// and returns the rendered YAML body verbatim. Cloudbox renders the
// four-stanza kubeconfig server-side (handlers.renderKubeconfigYAML)
// so the outpost never reconstructs it.
//
// 1 MiB response cap is intentionally generous — a real kubeconfig is
// ~1500 bytes; anything materially larger means a misconfigured server
// or a non-YAML body the operator should see truncated rather than
// have eaten by an unbounded reader.
func FetchUserKubeconfigYAML(ctx context.Context, cloudboxBase, accessToken string) ([]byte, error) {
	if strings.TrimSpace(cloudboxBase) == "" {
		return nil, errors.New("userkube: empty cloudboxBase")
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("userkube: empty accessToken")
	}
	url := strings.TrimRight(cloudboxBase, "/") + UserKubeconfigEndpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/yaml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("userkube: dial cloudbox: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("userkube: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userkube: cloudbox HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// DefaultKubectlPath returns where kubectl looks for its config by
// default: the first writable entry in $KUBECONFIG (colon-separated)
// or $HOME/.kube/config. Matches kubectl's own resolution so a merge
// targets the same file kubectl will read from on the next call.
func DefaultKubectlPath() string {
	if env := strings.TrimSpace(os.Getenv("KUBECONFIG")); env != "" {
		for _, part := range strings.Split(env, string(os.PathListSeparator)) {
			if p := strings.TrimSpace(part); p != "" {
				return p
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

// MergeIntoKubectl splices the clusters/users/contexts from newYAML
// into the kubeconfig at path (treating a missing file as an empty
// config) and re-points current-context to newYAML's. Existing
// entries with names that don't collide are preserved — running this
// repeatedly only churns the cloudbox user/cluster/context entries
// (stable names) and refreshes the SA bearer.
//
// path is resolved via DefaultKubectlPath when empty. The write is
// atomic (.tmp + rename) at mode 0600.
func MergeIntoKubectl(newYAML []byte, path string) (string, error) {
	if path == "" {
		path = DefaultKubectlPath()
	}
	if path == "" {
		return "", errors.New("userkube: no kubeconfig path — set KUBECONFIG or ensure HOME resolves")
	}
	newCfg, err := clientcmd.Load(newYAML)
	if err != nil {
		return path, fmt.Errorf("userkube: parse cloudbox kubeconfig: %w", err)
	}

	existing := clientcmdapi.NewConfig()
	raw, err := os.ReadFile(path)
	switch {
	case err == nil && len(raw) > 0:
		loaded, lerr := clientcmd.Load(raw)
		if lerr != nil {
			return path, fmt.Errorf("userkube: parse existing %s: %w", path, lerr)
		}
		if loaded != nil {
			existing = loaded
		}
	case err == nil:
		// Empty file — keep the fresh NewConfig().
	case errors.Is(err, os.ErrNotExist):
		// First-time setup — keep the fresh NewConfig().
	default:
		return path, fmt.Errorf("userkube: read %s: %w", path, err)
	}

	if existing.Clusters == nil {
		existing.Clusters = map[string]*clientcmdapi.Cluster{}
	}
	if existing.AuthInfos == nil {
		existing.AuthInfos = map[string]*clientcmdapi.AuthInfo{}
	}
	if existing.Contexts == nil {
		existing.Contexts = map[string]*clientcmdapi.Context{}
	}
	for name, c := range newCfg.Clusters {
		existing.Clusters[name] = c
	}
	for name, u := range newCfg.AuthInfos {
		existing.AuthInfos[name] = u
	}
	for name, c := range newCfg.Contexts {
		existing.Contexts[name] = c
	}
	if newCfg.CurrentContext != "" {
		existing.CurrentContext = newCfg.CurrentContext
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, err
	}
	tmp := path + ".tmp"
	if err := clientcmd.WriteToFile(*existing, tmp); err != nil {
		return path, fmt.Errorf("userkube: write %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return path, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return path, err
	}
	return path, nil
}

// WriteStandalone writes the kubeconfig YAML to path verbatim, with
// 0600 perms and atomic rename — useful when the caller wants the
// cloudbox kubeconfig in a separate file (e.g. for KUBECONFIG-list
// merging in tools that prefer that over an in-place rewrite).
func WriteStandalone(yaml []byte, path string) error {
	if path == "" {
		return errors.New("userkube: empty output path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, yaml, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
