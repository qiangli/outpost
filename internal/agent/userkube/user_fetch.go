package userkube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// UserKubeconfigEndpoint is cloudbox's user-mode kubeconfig issuer.
// The route accepts the outpost's access_token as a Bearer (via
// middleware.DecodeUser, which trusts any cloudbox-signed JWT) and
// mints a per-user ServiceAccount token in the outpost-users
// namespace scoped to the calling account.
const UserKubeconfigEndpoint = "/api/cluster/userkubeconfig"

const maxKubeconfigBytes = 1 << 20

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
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("userkube: dial cloudbox: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKubeconfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("userkube: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		contentType := responseMediaType(resp.Header.Get("Content-Type"))
		if location := strings.TrimSpace(resp.Header.Get("Location")); location != "" {
			return nil, fmt.Errorf("userkube: endpoint %s returned HTTP %d redirect to %q (content-type %q); check cloudbox URL and login",
				UserKubeconfigEndpoint, resp.StatusCode, safeRedirectLocation(location), contentType)
		}
		return nil, fmt.Errorf("userkube: endpoint %s returned HTTP %d (content-type %q)",
			UserKubeconfigEndpoint, resp.StatusCode, contentType)
	}
	if len(body) > maxKubeconfigBytes {
		return nil, fmt.Errorf("userkube: endpoint %s response exceeds %d bytes", UserKubeconfigEndpoint, maxKubeconfigBytes)
	}
	contentType := responseMediaType(resp.Header.Get("Content-Type"))
	switch contentType {
	case "application/yaml", "application/x-yaml", "text/yaml":
	default:
		return nil, fmt.Errorf("userkube: endpoint %s returned unexpected content-type %q; expected Kubernetes YAML (possible login/SPA response)",
			UserKubeconfigEndpoint, contentType)
	}
	if _, err := ValidateKubeconfig(body); err != nil {
		return nil, fmt.Errorf("userkube: endpoint %s returned invalid kubeconfig (content-type %q): %w",
			UserKubeconfigEndpoint, contentType, err)
	}
	return body, nil
}

func safeRedirectLocation(location string) string {
	redirect, err := url.Parse(location)
	if err != nil {
		return "(redacted)"
	}
	redirect.RawQuery = ""
	redirect.ForceQuery = false
	redirect.Fragment = ""
	return redirect.String()
}

func responseMediaType(header string) string {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return strings.TrimSpace(header)
	}
	return strings.ToLower(mediaType)
}

// ValidateKubeconfig parses the response and requires a complete current
// kubeconfig identity. It intentionally rejects HTML/login bodies and every
// non-HTTPS API server before bytes may reach a cache file.
func ValidateKubeconfig(data []byte) (*clientcmdapi.Config, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, errors.New("empty document")
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return nil, errors.New("received HTML/login SPA instead of kubeconfig YAML")
	}
	var metadata struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("parse YAML metadata: %w", err)
	}
	if metadata.APIVersion != "v1" {
		return nil, fmt.Errorf("apiVersion: got %q, want %q", metadata.APIVersion, "v1")
	}
	if metadata.Kind != "Config" {
		return nil, fmt.Errorf("kind: got %q, want %q", metadata.Kind, "Config")
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, errors.New("clusters: must not be empty")
	}
	if len(cfg.AuthInfos) == 0 {
		return nil, errors.New("users: must not be empty")
	}
	if len(cfg.Contexts) == 0 {
		return nil, errors.New("contexts: must not be empty")
	}
	if strings.TrimSpace(cfg.CurrentContext) == "" {
		return nil, errors.New("current-context: must not be empty")
	}
	current, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || current == nil {
		return nil, fmt.Errorf("current-context %q is not defined", cfg.CurrentContext)
	}
	cluster, ok := cfg.Clusters[current.Cluster]
	if !ok || cluster == nil {
		return nil, fmt.Errorf("current-context cluster %q is not defined", current.Cluster)
	}
	user, ok := cfg.AuthInfos[current.AuthInfo]
	if !ok || user == nil || strings.TrimSpace(current.AuthInfo) == "" {
		return nil, fmt.Errorf("current-context user %q is not defined", current.AuthInfo)
	}
	server, err := url.Parse(strings.TrimSpace(cluster.Server))
	if err != nil || server.Scheme != "https" || server.Host == "" {
		return nil, fmt.Errorf("cluster server %q must be an absolute https URL", cluster.Server)
	}
	return cfg, nil
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
	newCfg, err := ValidateKubeconfig(newYAML)
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

	merged, err := clientcmd.Write(*existing)
	if err != nil {
		return path, fmt.Errorf("userkube: encode merged kubeconfig: %w", err)
	}
	if err := writeValidatedAtomic(path, merged); err != nil {
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
	return writeValidatedAtomic(path, yaml)
}

func writeValidatedAtomic(path string, data []byte) error {
	if _, err := ValidateKubeconfig(data); err != nil {
		return fmt.Errorf("userkube: refusing to write invalid kubeconfig: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}
