package conf

import (
	"encoding/json"
	"testing"
)

func TestClusterConfig_LocalAPIURL(t *testing.T) {
	// Default port when unset.
	def := (&ClusterConfig{}).LocalAPIURL()
	if def != "https://127.0.0.1:6443" {
		t.Fatalf("default LocalAPIURL = %q, want https://127.0.0.1:6443", def)
	}
	// Honors an override.
	over := (&ClusterConfig{K8sAPIPort: 7443}).LocalAPIURL()
	if over != "https://127.0.0.1:7443" {
		t.Fatalf("override LocalAPIURL = %q, want https://127.0.0.1:7443", over)
	}
	// The peer apiserver URL is loopback — never a cloudbox public URL. This is
	// the assumption the peer vk path relies on to stay cloudbox-independent.
	if got := (&ClusterConfig{}).K8sAPIPortOrDefault(); got != DefaultK8sAPIPort {
		t.Fatalf("K8sAPIPortOrDefault = %d, want %d", got, DefaultK8sAPIPort)
	}
}

func TestClusterConfig_HasClientCert(t *testing.T) {
	if (&ClusterConfig{}).HasClientCert() {
		t.Fatalf("empty cluster should not have a client cert")
	}
	if (&ClusterConfig{ClientCert: []byte("c")}).HasClientCert() {
		t.Fatalf("half a client-cert pair must not count as having one")
	}
	if !(&ClusterConfig{ClientCert: []byte("c"), ClientKey: []byte("k")}).HasClientCert() {
		t.Fatalf("full client-cert pair should count")
	}
}

// TestClusterConfig_PeerFieldsRoundTrip proves the peer credential + policy
// fields survive a save/load cycle (JSON) so a peer-joined worker's identity
// persists across restarts.
func TestClusterConfig_PeerFieldsRoundTrip(t *testing.T) {
	in := &ClusterConfig{
		JoinEndpoint:      "peer-host:7000",
		JoinToken:         "tunnel-tok",
		CA:                []byte("PEER-CA"),
		ClientCert:        []byte("CERT"),
		ClientKey:         []byte("KEY"),
		AllowedNamespaces: []string{"user-abc", "team-x"},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ClusterConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.JoinsPeerPlane() {
		t.Fatalf("expected JoinsPeerPlane after round trip")
	}
	if !out.HasClientCert() {
		t.Fatalf("client cert lost in round trip")
	}
	if len(out.AllowedNamespaces) != 2 || out.AllowedNamespaces[0] != "user-abc" {
		t.Fatalf("allowed_namespaces lost: %+v", out.AllowedNamespaces)
	}
}

// TestClusterConfig_MixedRuntimeOnPeerPlane proves a peer-joined host can select
// the real agent AND virtual backends together — the "run real agent plus
// selected virtual backends on the same peer plane" requirement — and that the
// selection validates.
func TestClusterConfig_MixedRuntimeOnPeerPlane(t *testing.T) {
	virtual, err := NormalizeVirtualRuntimes([]string{"vk-podman", "vk-native", "vk-ollama"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	cc := &ClusterConfig{
		JoinEndpoint: "peer-host:7000",
		JoinToken:    "tunnel-tok",
		Runtimes: ClusterRuntimes{
			Agent:   true,
			Virtual: virtual,
		},
	}
	if !cc.JoinsPeerPlane() {
		t.Fatalf("should join a peer plane")
	}
	if !cc.HasAgentRuntime() {
		t.Fatalf("agent runtime should be selected")
	}
	if got := cc.VirtualRuntimes(); len(got) != 3 {
		t.Fatalf("expected 3 virtual runtimes, got %v", got)
	}
	if err := cc.ValidateRuntimes(); err != nil {
		t.Fatalf("mixed agent+virtual on a peer plane must validate: %v", err)
	}

	// Deselecting everything is refused (matches SetBuiltins / JoinPeerPlane).
	none := &ClusterConfig{JoinEndpoint: "peer:7000"}
	if err := none.ValidateRuntimes(); err == nil {
		t.Fatalf("a peer join with no runtime selected must fail validation")
	}
}
