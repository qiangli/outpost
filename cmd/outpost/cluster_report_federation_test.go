package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/fleetreg"
)

// The federation rides the ONE kind:cluster row as an attribute. These tests
// pin the two halves of the W4 contract on the clusterReport side: with a CA
// hash the row carries federation{id,role,cluster_name,sync_mode} and the
// cluster identity is untouched; without one the row is exactly the
// unfederated row.
func TestClusterReport_FederationFilledIffCAHashKnown(t *testing.T) {
	const clusterHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const fedHash = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	fc := &conf.FileConfig{
		AgentName: "worker",
		Cluster: &conf.ClusterConfig{
			Enabled:      boolPtr(true),
			Runtimes:     conf.ClusterRuntimes{Agent: true},
			JoinEndpoint: "10.0.0.5:7000",
			NodeToken:    "K10" + clusterHash + "::node:x",
		},
	}

	// Default: no federation config wired, no federation reported.
	plain := clusterReport(fc, "https://ai.example.io")
	if plain == nil {
		t.Fatal("no report")
	}
	if plain.Federation != nil {
		t.Fatalf("unfederated host reported a federation: %+v", plain.Federation)
	}

	// Facts present but no CA hash yet (a member waiting on its discovery
	// hash): still no federation — never a nameless one.
	withFacts := func(hash string) *fleetreg.ClusterInfo {
		prev := federationFacts
		defer func() { federationFacts = prev }()
		federationFacts = func(cc *conf.ClusterConfig) (string, string, string, string) {
			if cc != fc.Cluster {
				t.Fatal("federationFacts was not handed the row's own cluster config")
			}
			return hash, fleetreg.FederationRoleMember, "worker", fleetreg.FederationSyncPull
		}
		return clusterReport(fc, "https://ai.example.io")
	}
	if got := withFacts(""); got.Federation != nil {
		t.Fatalf("no CA hash but a federation was reported: %+v", got.Federation)
	}

	fed := withFacts("sha256:" + fedHash)
	if fed.Federation == nil {
		t.Fatal("CA hash known but no federation reported")
	}
	if fed.Federation.ID != "fed-"+fedHash[:12] || fed.Federation.Role != "member" ||
		fed.Federation.ClusterName != "worker" || fed.Federation.SyncMode != "Pull" {
		t.Errorf("federation = %+v", fed.Federation)
	}
	// ClusterID() is the CLUSTER's identity, with or without a federation.
	if fed.ClusterID() != plain.ClusterID() || fed.ClusterID() != "k8s-"+clusterHash[:12] {
		t.Errorf("federation changed the cluster id: %q vs %q", fed.ClusterID(), plain.ClusterID())
	}

	// Everything else in the row is identical — the federation is additive.
	fed.Federation = nil
	a, _ := json.Marshal(fed)
	b, _ := json.Marshal(plain)
	if string(a) != string(b) {
		t.Errorf("federation changed the rest of the row:\n%s\n%s", a, b)
	}
	if strings.Contains(string(b), "federation") {
		t.Errorf("unfederated row carries a federation key: %s", b)
	}
}
