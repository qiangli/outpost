package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKubeconfigFreshRejectsFreshMalformedCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster-kubeconfig.yaml")
	if err := os.WriteFile(path, []byte("<!doctype html><html>tessaro</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if kubeconfigFresh(path) {
		t.Fatal("fresh SPA HTML cache was accepted as kubeconfig")
	}
	valid := `apiVersion: v1
kind: Config
clusters:
- name: cloud
  cluster:
    server: https://cluster.example.test
users:
- name: owner
  user:
    token: test
contexts:
- name: owner@cloud
  context:
    cluster: cloud
    user: owner
current-context: owner@cloud
`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if !kubeconfigFresh(path) {
		t.Fatal("fresh valid kubeconfig was rejected")
	}
}
