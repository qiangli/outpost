// Package federation is the host-side half of the Karmada federation
// (dhnt/docs/dks-federation-karmada.md): the kubeconfig materialiser, the CA
// discovery hash, the karmadactl runner and the host reconciler that drives
// install -> materialise -> identify -> self-register.
package federation

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
)

// CAHashFromKubeconfig computes the kubeadm/Karmada discovery hash of the
// kubeconfig's cluster CA: lowercase hex of sha256 over the CA certificate's
// SubjectPublicKeyInfo. The same value `karmadactl register` takes as
// --discovery-token-ca-cert-hash sha256:<hex>.
func CAHashFromKubeconfig(kubeconfig []byte) (string, error) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}
	for _, c := range cfg.Clusters {
		if len(c.CertificateAuthorityData) == 0 {
			continue
		}
		return CAHashFromPEM(c.CertificateAuthorityData)
	}
	return "", fmt.Errorf("kubeconfig has no certificate-authority-data")
}

// CAHashFromPEM hashes the first certificate in a PEM bundle.
func CAHashFromPEM(bundle []byte) (string, error) {
	for {
		var blk *pem.Block
		blk, bundle = pem.Decode(bundle)
		if blk == nil {
			return "", fmt.Errorf("no CERTIFICATE block in CA data")
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return "", fmt.Errorf("parse CA certificate: %w", err)
		}
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		return hex.EncodeToString(sum[:]), nil
	}
}

// NormalizeHash lowercases and strips an optional "sha256:" prefix; "" if the
// result is not a 64-char hex digest.
func NormalizeHash(h string) string {
	h = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), "sha256:")
	if len(h) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(h); err != nil {
		return ""
	}
	return h
}

// FederationID renders a hash as fed-<first 12 hex>, or "" if invalid.
func FederationID(h string) string {
	if n := NormalizeHash(h); n != "" {
		return "fed-" + n[:12]
	}
	return ""
}
