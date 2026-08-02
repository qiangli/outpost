package vknode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"

	"k8s.io/client-go/rest"
)

// ClusterIdentityFromCA derives the stable cluster-identity value stamped
// into ClusterLabel from the cluster's CA bundle. The CA is the cluster's
// cryptographic identity: it survives API-URL changes (LAN IP today, mesh
// forward tomorrow) and cannot be spoofed by a workload, which is why the
// identity is a CA fingerprint and deliberately NOT a hash of the API URL.
//
// The hash covers the DER bytes of every CERTIFICATE block in the bundle,
// in order, so cosmetic PEM differences (re-wrapping, comments, trailing
// whitespace) between two copies of the same bundle produce the same
// identity. A bundle with no parseable CERTIFICATE block falls back to
// hashing the trimmed raw bytes — a stable, if opaque, identity.
//
// Empty input returns "" — the caller runs unscoped (legacy semantics,
// see podmanBackend ownership rules). That happens when the apiserver
// sits behind a public cert and the kubeconfig trusts system roots; there
// is no cluster-owned key material to fingerprint in that case.
func ClusterIdentityFromCA(caPEM []byte) string {
	trimmed := bytes.TrimSpace(caPEM)
	if len(trimmed) == 0 {
		return ""
	}
	h := sha256.New()
	sawCert := false
	for rest := trimmed; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		sawCert = true
		h.Write(block.Bytes)
	}
	if !sawCert {
		h.Reset()
		h.Write(trimmed)
	}
	// 16 hex chars = 64 bits — ample for "distinct across the handful of
	// clusters one podman socket will ever serve", short enough to read
	// in `podman ps --format '{{.Labels}}'`.
	return "ca256-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// ClusterIdentityFromRestConfig derives the cluster identity from an
// already-built kube REST config — the one seam both the supervised
// daemon (ConfigFromCluster / ConfigFromClientCert, inline CAData) and
// the standalone outpost-vk runner (clientcmd kubeconfig, CAData or
// CAFile) flow through. Reading the CAFile here is fail-closed: if the
// file is unreadable we error rather than silently running unscoped,
// since client-go would fail to build the transport from it anyway.
func ClusterIdentityFromRestConfig(cfg *rest.Config) (string, error) {
	if cfg == nil {
		return "", nil
	}
	ca := cfg.TLSClientConfig.CAData
	if len(ca) == 0 && cfg.TLSClientConfig.CAFile != "" {
		b, err := os.ReadFile(cfg.TLSClientConfig.CAFile)
		if err != nil {
			return "", fmt.Errorf("vknode: read CA file for cluster identity: %w", err)
		}
		ca = b
	}
	return ClusterIdentityFromCA(ca), nil
}
