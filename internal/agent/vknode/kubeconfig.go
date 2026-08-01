package vknode

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

	// ClientCert / ClientKey carry client-certificate credentials, the
	// form k3s writes at /etc/rancher/k3s/k3s.yaml. Exactly one of
	// (Token) or (ClientCert+ClientKey) is populated.
	//
	// Both shapes exist because the control plane's LOCATION is a user
	// choice (dhnt/docs/dks-control-plane-on-sphere.md): a cloudbox- or
	// droplet-hosted plane hands out a minted bearer token, while a
	// peer-hosted one is just k3s, which authenticates with certs. One
	// parser handles both rather than a second credential path existing
	// per placement.
	ClientCert []byte
	ClientKey  []byte
}

// HasCredential reports whether either supported credential is present.
func (p *ParsedKubeconfig) HasCredential() bool {
	return p.Token != "" || (len(p.ClientCert) > 0 && len(p.ClientKey) > 0)
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
		return nil, fmt.Errorf("vknode: empty kubeconfig")
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("vknode: parse kubeconfig: %w", err)
	}
	ctxName := strings.TrimSpace(cfg.CurrentContext)
	if ctxName == "" {
		return nil, fmt.Errorf("vknode: kubeconfig has no current-context")
	}
	kctx, ok := cfg.Contexts[ctxName]
	if !ok {
		return nil, fmt.Errorf("vknode: kubeconfig current-context %q not defined", ctxName)
	}
	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return nil, fmt.Errorf("vknode: cluster %q referenced by context not defined", kctx.Cluster)
	}
	user, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return nil, fmt.Errorf("vknode: user %q referenced by context not defined", kctx.AuthInfo)
	}
	if strings.TrimSpace(cluster.Server) == "" {
		return nil, fmt.Errorf("vknode: cluster %q has empty server URL", kctx.Cluster)
	}
	if err := rejectUnsupportedAuth(user, kctx.AuthInfo); err != nil {
		return nil, err
	}
	ca, err := caFromCluster(cluster)
	if err != nil {
		return nil, err
	}
	out := &ParsedKubeconfig{APIURL: strings.TrimSpace(cluster.Server), CA: ca}

	// Client certs first: a kubeconfig carrying both is unusual, and the
	// cert is the stronger statement of intent.
	cert, key, err := clientCertFromUser(user)
	if err != nil {
		return nil, err
	}
	if len(cert) > 0 && len(key) > 0 {
		out.ClientCert, out.ClientKey = cert, key
		return out, nil
	}

	token, err := tokenFromUser(user)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("vknode: kubeconfig user %q has no usable credential (no token, no client certificate)", kctx.AuthInfo)
	}
	out.Token = token
	return out, nil
}

// clientCertFromUser reads inline or file-referenced client cert/key.
// A half-pair is an error rather than a silent fall-through to the token
// branch: a kubeconfig naming a cert but not a key is malformed, and
// treating it as "no cert" would surface later as an opaque 401.
func clientCertFromUser(user *clientcmdapi.AuthInfo) (cert, key []byte, err error) {
	cert = append([]byte(nil), user.ClientCertificateData...)
	if len(cert) == 0 && strings.TrimSpace(user.ClientCertificate) != "" {
		if cert, err = os.ReadFile(user.ClientCertificate); err != nil {
			return nil, nil, fmt.Errorf("vknode: read client certificate: %w", err)
		}
	}
	key = append([]byte(nil), user.ClientKeyData...)
	if len(key) == 0 && strings.TrimSpace(user.ClientKey) != "" {
		if key, err = os.ReadFile(user.ClientKey); err != nil {
			return nil, nil, fmt.Errorf("vknode: read client key: %w", err)
		}
	}
	if (len(cert) > 0) != (len(key) > 0) {
		return nil, nil, fmt.Errorf("vknode: kubeconfig has a client certificate without its key (or vice versa)")
	}
	return cert, key, nil
}

func rejectUnsupportedAuth(user *clientcmdapi.AuthInfo, name string) error {
	if user.AuthProvider != nil {
		return fmt.Errorf("vknode: kubeconfig user %q uses auth-provider — only bearer-token credentials are supported in v1", name)
	}
	if user.Exec != nil {
		return fmt.Errorf("vknode: kubeconfig user %q uses exec-plugin auth — only bearer-token credentials are supported in v1", name)
	}
	// client-certificate auth is SUPPORTED (see clientCertFromUser) —
	// it is what k3s writes, so a peer-hosted control plane produces it.
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
			return "", fmt.Errorf("vknode: read kubeconfig token file %s: %w", user.TokenFile, err)
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
