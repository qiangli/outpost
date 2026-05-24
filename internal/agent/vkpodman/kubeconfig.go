package vkpodman

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ParsedKubeconfig is the subset of a kubeconfig the cluster runner
// needs to dial: apiserver URL, bearer token, and optional CA bundle.
// Returned by ParseKubeconfig so the admin UI can persist it into
// conf.ClusterConfig and discard the original YAML.
type ParsedKubeconfig struct {
	APIURL string
	Token  string
	CA     []byte
}

// ParseKubeconfig pulls APIURL / Token / CA out of a kubeconfig (YAML
// bytes) — typically the file at /etc/rancher/k3s/k3s.yaml for dev /
// PoC, or the per-host kubeconfig cloudbox issues in production.
//
// We look at current-context's cluster + user only. AuthProvider /
// exec-plugin / client-cert auth are rejected with a clear error
// instead of silently producing a half-working credential — these are
// uncommon for cluster-join tokens, and adding them later is additive.
func ParseKubeconfig(raw []byte) (*ParsedKubeconfig, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("vkpodman: empty kubeconfig")
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("vkpodman: parse kubeconfig: %w", err)
	}
	ctxName := strings.TrimSpace(cfg.CurrentContext)
	if ctxName == "" {
		return nil, fmt.Errorf("vkpodman: kubeconfig has no current-context")
	}
	kctx, ok := cfg.Contexts[ctxName]
	if !ok {
		return nil, fmt.Errorf("vkpodman: kubeconfig current-context %q not defined", ctxName)
	}
	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return nil, fmt.Errorf("vkpodman: cluster %q referenced by context not defined", kctx.Cluster)
	}
	user, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return nil, fmt.Errorf("vkpodman: user %q referenced by context not defined", kctx.AuthInfo)
	}
	if strings.TrimSpace(cluster.Server) == "" {
		return nil, fmt.Errorf("vkpodman: cluster %q has empty server URL", kctx.Cluster)
	}
	if err := rejectUnsupportedAuth(user, kctx.AuthInfo); err != nil {
		return nil, err
	}
	token, err := tokenFromUser(user)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("vkpodman: kubeconfig user %q has no token", kctx.AuthInfo)
	}
	ca, err := caFromCluster(cluster)
	if err != nil {
		return nil, err
	}
	return &ParsedKubeconfig{
		APIURL: strings.TrimSpace(cluster.Server),
		Token:  token,
		CA:     ca,
	}, nil
}

func rejectUnsupportedAuth(user *clientcmdapi.AuthInfo, name string) error {
	if user.AuthProvider != nil {
		return fmt.Errorf("vkpodman: kubeconfig user %q uses auth-provider — only bearer-token credentials are supported in v1", name)
	}
	if user.Exec != nil {
		return fmt.Errorf("vkpodman: kubeconfig user %q uses exec-plugin auth — only bearer-token credentials are supported in v1", name)
	}
	if user.ClientCertificate != "" || len(user.ClientCertificateData) > 0 ||
		user.ClientKey != "" || len(user.ClientKeyData) > 0 {
		return fmt.Errorf("vkpodman: kubeconfig user %q uses client-certificate — only bearer-token credentials are supported in v1", name)
	}
	return nil
}

// tokenFromUser reads either the inline Token or the contents of TokenFile.
// Trimming surrounding whitespace handles both common k8s SA token shapes
// (one line, no trailing newline; or one line plus newline).
func tokenFromUser(user *clientcmdapi.AuthInfo) (string, error) {
	if t := strings.TrimSpace(user.Token); t != "" {
		return t, nil
	}
	if user.TokenFile != "" {
		data, err := os.ReadFile(user.TokenFile)
		if err != nil {
			return "", fmt.Errorf("vkpodman: read kubeconfig token file %s: %w", user.TokenFile, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return "", nil
}

// caFromCluster returns the CA bundle for the cluster — either inline
// (CertificateAuthorityData) or read from disk (CertificateAuthority).
// Empty result means "trust the system roots", which is the right
// behavior when cloudbox fronts the apiserver behind a real public cert.
func caFromCluster(cluster *clientcmdapi.Cluster) ([]byte, error) {
	if len(cluster.CertificateAuthorityData) > 0 {
		return append([]byte(nil), cluster.CertificateAuthorityData...), nil
	}
	if cluster.CertificateAuthority != "" {
		return os.ReadFile(cluster.CertificateAuthority)
	}
	return nil, nil
}

// EncodeCA renders a CA bundle as a base64 string. Useful for places
// that need a stringy form (e.g. surfacing the persisted CA length in
// the admin UI without sending the actual bytes).
func EncodeCA(ca []byte) string {
	if len(ca) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(ca)
}
