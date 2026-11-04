package main

import (
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/vknode"
)

// A credential rotation runs on a 12 h timer with the config the daemon
// loaded at boot. Saving that snapshot erased every app added since: on
// elephant the published `klatch` app vanished from agent.json at each
// rotation (Sprint 563 F1). The rotation must only change the cluster
// credential in the file as it is now.
func TestPersistClusterCredentialKeepsChangesSinceBoot(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	on := true
	boot := &conf.FileConfig{
		AgentName: "elephant",
		Apps:      []conf.AppConfig{{Name: "vlog", Scheme: "http", Host: "127.0.0.1", Port: 18090, Enabled: true}},
		Cluster:   &conf.ClusterConfig{Enabled: &on, Token: "old-token"},
	}
	if err := conf.SaveFile(cfgPath, boot); err != nil {
		t.Fatal(err)
	}

	// After boot, the operator publishes another app (`outpost apps add`).
	now, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	now.Apps = append(now.Apps, conf.AppConfig{Name: "klatch", Scheme: "http", Host: "127.0.0.1", Port: 7100, Enabled: true})
	if err := conf.SaveFile(cfgPath, now); err != nil {
		t.Fatal(err)
	}

	persistClusterCredential(boot, cfgPath, &vknode.ParsedKubeconfig{APIURL: "https://sat/api", Token: "new-token", CA: []byte("ca")})

	got, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, a := range got.Apps {
		names[a.Name] = true
	}
	if !names["klatch"] || !names["vlog"] {
		t.Fatalf("apps after rotation = %v, want vlog and klatch", names)
	}
	if got.Cluster == nil || got.Cluster.Token != "new-token" || got.Cluster.APIURL != "https://sat/api" {
		t.Fatalf("cluster credential not persisted: %+v", got.Cluster)
	}
	if boot.Cluster.Token != "new-token" {
		t.Fatalf("in-memory config not updated: token %q", boot.Cluster.Token)
	}
}

// First boot has no agent.json yet; the fetched credential is still saved.
func TestPersistClusterCredentialWritesWhenNoFile(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	fc := &conf.FileConfig{AgentName: "fresh"}
	persistClusterCredential(fc, cfgPath, &vknode.ParsedKubeconfig{APIURL: "https://sat/api", Token: "t1"})
	got, err := conf.LoadFile(cfgPath)
	if err != nil || got == nil || got.Cluster == nil || got.Cluster.Token != "t1" {
		t.Fatalf("first-boot save: cfg=%+v err=%v", got, err)
	}
}
