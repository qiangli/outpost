package vknode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenK3sYAML is the shape k3s writes to /etc/rancher/k3s/k3s.yaml —
// inline CA + inline token, single context. The fake values don't have
// to round-trip TLS, just parse.
const goldenK3sYAML = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ZmFrZS1jYQ==
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    token: K3sFakeBearerToken
`

func TestParseKubeconfig_Golden(t *testing.T) {
	got, err := ParseKubeconfig([]byte(goldenK3sYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.APIURL != "https://127.0.0.1:6443" {
		t.Errorf("APIURL = %q", got.APIURL)
	}
	if got.Token != "K3sFakeBearerToken" {
		t.Errorf("Token = %q", got.Token)
	}
	if string(got.CA) != "fake-ca" {
		t.Errorf("CA = %q (decoded)", string(got.CA))
	}
}

func TestParseKubeconfig_TokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: x
current-context: x
users:
- name: u
  user:
    tokenFile: ` + tokenPath + `
`
	got, err := ParseKubeconfig([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "file-token" {
		t.Errorf("Token = %q (trailing whitespace should be trimmed)", got.Token)
	}
}

// SUPERSEDED: client-certificate auth is now SUPPORTED — it is what k3s
// writes, so a peer-hosted control plane presents it
// (dhnt/docs/dks-control-plane-on-sphere.md). This test asserted the v1
// limitation; the behaviour it guarded was removed on purpose, so it is
// inverted here rather than deleted, to keep the change visible in
// history. Acceptance is covered in kubeconfig_clientcert_test.go.
func TestParseKubeconfig_AcceptsClientCertAuth(t *testing.T) {
	yaml := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: x
current-context: x
users:
- name: u
  user:
    client-certificate-data: ZmFrZQ==
    client-key-data: ZmFrZQ==
`
	got, err := ParseKubeconfig([]byte(yaml))
	if err != nil {
		t.Fatalf("client-cert auth should now be accepted; got %v", err)
	}
	if len(got.ClientCert) == 0 || len(got.ClientKey) == 0 {
		t.Fatalf("cert/key not extracted: %+v", got)
	}
}

func TestParseKubeconfig_RejectsExecAuth(t *testing.T) {
	yaml := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: x
current-context: x
users:
- name: u
  user:
    exec:
      command: /usr/bin/aws
      apiVersion: client.authentication.k8s.io/v1
`
	_, err := ParseKubeconfig([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "exec-plugin") {
		t.Fatalf("expected rejection of exec auth; got %v", err)
	}
}

func TestParseKubeconfig_NoCurrentContext(t *testing.T) {
	yaml := `apiVersion: v1
kind: Config
clusters:
- cluster: {server: https://x}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: x
users:
- name: u
  user: {token: t}
`
	_, err := ParseKubeconfig([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "current-context") {
		t.Fatalf("expected no-current-context error; got %v", err)
	}
}

func TestParseKubeconfig_Empty(t *testing.T) {
	if _, err := ParseKubeconfig(nil); err == nil {
		t.Error("expected error for nil input")
	}
	if _, err := ParseKubeconfig([]byte("")); err == nil {
		t.Error("expected error for empty input")
	}
}
