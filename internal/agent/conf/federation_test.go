package conf

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFederationConfigDefaultsAndRoundTrip(t *testing.T) {
	var nilf *FederationConfig
	if nilf.On() || nilf.HostOn() || nilf.MemberOn() || nilf.Port() != DefaultFederationAPIPort {
		t.Fatal("nil federation must be off with the default port")
	}
	f := &FederationConfig{Enabled: true, Host: true}
	if !f.HostOn() || f.MemberOn() {
		t.Fatal("role predicates")
	}
	if !strings.HasSuffix(filepath.ToSlash(f.KubeconfigPath()), DefaultFederationKubeconfigRel) {
		t.Fatalf("default kubeconfig = %s", f.KubeconfigPath())
	}
	f.Kubeconfig = "/x/k.yaml"
	if f.KubeconfigPath() != "/x/k.yaml" {
		t.Fatal("override must win")
	}
	t.Setenv("DKS_FEDERATION_KUBECONFIG", "/env/k.yaml")
	f.Kubeconfig = ""
	if f.KubeconfigPath() != "/env/k.yaml" {
		t.Fatalf("env = %s", f.KubeconfigPath())
	}

	path := filepath.Join(t.TempDir(), "agent.json")
	fc := &FileConfig{Cluster: &ClusterConfig{Federation: &FederationConfig{Enabled: true, Host: true, CAHash: "ab", APIPort: 25443}}}
	if err := SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(path)
	if err != nil || got.Cluster.Federation == nil || got.Cluster.Federation.Port() != 25443 || !got.Cluster.FederationOn() {
		t.Fatalf("round trip: %+v err=%v", got.Cluster, err)
	}
}
