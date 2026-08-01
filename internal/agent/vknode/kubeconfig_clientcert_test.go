package vknode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// k3sStyleKubeconfig is the shape k3s writes to
// /etc/rancher/k3s/k3s.yaml — client-certificate auth, which is what a
// PEER-HOSTED control plane presents. It was rejected outright before
// client-cert support, which is exactly what blocked vk on a
// self-hosted plane.
const k3sStyleKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Y2EtcGVt
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
users:
- name: default
  user:
    client-certificate-data: Y2xpZW50LWNlcnQ=
    client-key-data: Y2xpZW50LWtleQ==
`

func TestParseKubeconfig_AcceptsK3sClientCert(t *testing.T) {
	got, err := ParseKubeconfig([]byte(k3sStyleKubeconfig))
	if err != nil {
		t.Fatalf("k3s-style kubeconfig rejected: %v", err)
	}
	if got.APIURL != "https://127.0.0.1:6443" {
		t.Errorf("APIURL = %q", got.APIURL)
	}
	if string(got.ClientCert) != "client-cert" || string(got.ClientKey) != "client-key" {
		t.Errorf("cert/key = %q / %q", got.ClientCert, got.ClientKey)
	}
	if string(got.CA) != "ca-pem" {
		t.Errorf("CA = %q", got.CA)
	}
	if got.Token != "" {
		t.Errorf("Token should be empty for cert auth, got %q", got.Token)
	}
	if !got.HasCredential() {
		t.Error("HasCredential() false for a valid cert pair")
	}
}

// The cloudbox-minted shape must keep working unchanged — one parser,
// both placements.
func TestParseKubeconfig_TokenStillWorks(t *testing.T) {
	const tokenCfg = `apiVersion: v1
clusters:
- cluster: {server: https://ai.example/api/cluster/agent}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: c
current-context: c
kind: Config
users:
- name: u
  user: {token: sa-token-value}
`
	got, err := ParseKubeconfig([]byte(tokenCfg))
	if err != nil {
		t.Fatalf("token kubeconfig rejected: %v", err)
	}
	if got.Token != "sa-token-value" {
		t.Errorf("Token = %q", got.Token)
	}
	if len(got.ClientCert) != 0 {
		t.Error("cert fields should be empty for token auth")
	}
}

// A cert without its key is malformed. Falling through to the token
// branch would surface later as an opaque 401 against a live apiserver,
// so it must fail here where the message can name the cause.
func TestParseKubeconfig_HalfCertPairIsAnError(t *testing.T) {
	half := strings.Replace(k3sStyleKubeconfig, "    client-key-data: Y2xpZW50LWtleQ==\n", "", 1)
	_, err := ParseKubeconfig([]byte(half))
	if err == nil {
		t.Fatal("expected an error for a certificate with no key")
	}
	if !strings.Contains(err.Error(), "without its key") {
		t.Errorf("error should name the half-pair, got: %v", err)
	}
}

// File-referenced certs are as valid as inline ones; k3s inlines, but
// hand-written kubeconfigs commonly point at files.
func TestParseKubeconfig_CertFromFileRefs(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certPath, []byte("file-cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("file-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := strings.NewReplacer(
		"    client-certificate-data: Y2xpZW50LWNlcnQ=", "    client-certificate: "+certPath,
		"    client-key-data: Y2xpZW50LWtleQ==", "    client-key: "+keyPath,
	).Replace(k3sStyleKubeconfig)

	got, err := ParseKubeconfig([]byte(cfg))
	if err != nil {
		t.Fatalf("file-referenced certs rejected: %v", err)
	}
	if string(got.ClientCert) != "file-cert" || string(got.ClientKey) != "file-key" {
		t.Errorf("cert/key = %q / %q", got.ClientCert, got.ClientKey)
	}
}

func TestConfigFromClientCert(t *testing.T) {
	cfg, err := ConfigFromClientCert("https://127.0.0.1:6443", []byte("c"), []byte("k"), []byte("ca"))
	if err != nil {
		t.Fatalf("ConfigFromClientCert: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q", cfg.Host)
	}
	if string(cfg.TLSClientConfig.CertData) != "c" || string(cfg.TLSClientConfig.KeyData) != "k" {
		t.Error("cert/key not threaded into TLSClientConfig")
	}
	if cfg.BearerToken != "" || cfg.BearerTokenFile != "" {
		t.Error("cert config must not also carry a bearer credential")
	}
	if _, err := ConfigFromClientCert("https://x", []byte("c"), nil, nil); err == nil {
		t.Error("expected an error when the key is missing")
	}
}
