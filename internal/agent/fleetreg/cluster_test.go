package fleetreg

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/fleet"
)

const testCA = `-----BEGIN CERTIFICATE-----
MIIBkTCB+wIJAKZ2hTestCAbytesForFingerprintingOnlyNotARealCert123456
-----END CERTIFICATE-----`

// THE PROPERTY THE WHOLE DESIGN TURNS ON. Two nodes on one peer-hosted cluster
// reach it through their own tunnels, so their endpoints differ by port. If the
// identity came from the URL they would report two clusters of one node each
// and the grouping the page exists for would be silently wrong.
func TestClusterID_SameClusterDifferentLoopbackPorts(t *testing.T) {
	a := &ClusterInfo{Endpoint: "https://127.0.0.1:21744", CA: []byte(testCA)}
	b := &ClusterInfo{Endpoint: "https://127.0.0.1:36443", CA: []byte(testCA)}
	if a.ClusterID() != b.ClusterID() {
		t.Fatalf("two members of one cluster got different ids: %s vs %s", a.ClusterID(), b.ClusterID())
	}
	if a.ClusterID() == "" {
		t.Fatal("no id derived from a CA")
	}
}

// Different clusters must not collide, or moving a node between planes would
// look like no change at all.
func TestClusterID_DifferentCAsDiffer(t *testing.T) {
	a := &ClusterInfo{CA: []byte(testCA)}
	b := &ClusterInfo{CA: []byte(strings.Replace(testCA, "MIIBkTCB", "MIIBkTCC", 1))}
	if a.ClusterID() == b.ClusterID() {
		t.Fatal("distinct CAs produced one id")
	}
}

// A CA that differs only in trailing whitespace is the same CA. Hosts acquire
// it by different paths (config write, kubeconfig parse) and must still agree.
func TestClusterID_WhitespaceInsensitive(t *testing.T) {
	a := &ClusterInfo{CA: []byte(testCA)}
	b := &ClusterInfo{CA: []byte("\n" + testCA + "\n\n")}
	if a.ClusterID() != b.ClusterID() {
		t.Fatalf("whitespace changed the id: %s vs %s", a.ClusterID(), b.ClusterID())
	}
}

// With cloudbox fronting a publicly-trusted cert there is no CA to fingerprint,
// but every member then agrees on the URL — so the URL is a valid identity in
// exactly that case.
func TestClusterID_FallsBackToURLHostWithoutCA(t *testing.T) {
	c := &ClusterInfo{Endpoint: "https://ai.example.io/api/cluster/agent"}
	if got := c.ClusterID(); got != "ai.example.io" {
		t.Fatalf("id = %q, want the URL host", got)
	}
}

func TestClassifyPlacement(t *testing.T) {
	const cb = "https://ai.example.io"
	cases := []struct {
		name         string
		apiURL       string
		controlPlane bool
		want         string
	}{
		// The control-plane host's own endpoint is loopback too, so this case
		// proves the flag is consulted BEFORE the loopback test.
		{"control plane on loopback", "https://127.0.0.1:6443", true, PlacementSelf},
		{"tunnelled peer plane", "https://127.0.0.1:36443", false, PlacementPeer},
		{"localhost by name", "https://localhost:6443", false, PlacementPeer},
		{"cloudbox hosted", "https://ai.example.io/api/cluster/agent", false, PlacementCloudbox},
		{"rented box", "https://k8s.elsewhere.net:6443", false, PlacementExternal},
		{"no endpoint", "", false, PlacementExternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPlacement(tc.apiURL, cb, tc.controlPlane); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A host in no cluster must produce NO row. A blank row on every non-cluster
// host reads as "misconfigured" rather than "not participating".
func TestClusterAsset_NothingToReport(t *testing.T) {
	if _, ok := clusterAsset(nil); ok {
		t.Error("nil cluster produced a row")
	}
	if _, ok := clusterAsset(&ClusterInfo{Placement: PlacementPeer}); ok {
		t.Error("a cluster with no derivable identity produced a row")
	}
}

func TestClusterAsset_ShapeAndNoCALeak(t *testing.T) {
	a, ok := clusterAsset(&ClusterInfo{
		Placement: PlacementPeer,
		Endpoint:  "https://127.0.0.1:36443",
		Nodes:     []string{"dog", "dog-vk-ollama"},
		Runtimes:  []string{"agent", "vk-ollama"},
		CA:        []byte(testCA),
	})
	if !ok {
		t.Fatal("expected a row")
	}
	if a.Kind != "cluster" {
		t.Errorf("kind = %q", a.Kind)
	}
	if a.Display != "peer-hosted" {
		t.Errorf("display = %q", a.Display)
	}
	// The CA is public, but it is read for identity only — shipping it would
	// put a certificate in a column nothing renders.
	if strings.Contains(a.Detail, "BEGIN CERTIFICATE") {
		t.Error("the CA leaked into the reported detail")
	}
	var back ClusterInfo
	if err := json.Unmarshal([]byte(a.Detail), &back); err != nil {
		t.Fatalf("detail is not valid JSON: %v", err)
	}
	if len(back.Nodes) != 2 || back.Placement != PlacementPeer {
		t.Errorf("detail round-trip lost data: %+v", back)
	}
}

// The row must change when the host moves planes — that IS the signal the
// cluster page is meant to show.
func TestSnapshot_ClusterMoveChangesContentHash(t *testing.T) {
	root := t.TempDir()
	mk := func(c *ClusterInfo) string {
		w, err := New(Config{
			CloudboxURL: "https://ai.example.io",
			AgentName:   "host-a",
			Catalog:     func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) },
			Skills:      func() []string { return nil },
			Cluster:     func() *ClusterInfo { return c },
		})
		if err != nil {
			t.Fatal(err)
		}
		return ContentHash(w.Snapshot())
	}
	cloud := mk(&ClusterInfo{Placement: PlacementCloudbox, Endpoint: "https://ai.example.io"})
	peer := mk(&ClusterInfo{Placement: PlacementPeer, Endpoint: "https://127.0.0.1:36443", CA: []byte(testCA)})
	none := mk(nil)

	if cloud == peer {
		t.Error("moving from cloudbox to a peer plane did not change the pushed inventory")
	}
	if peer == none || cloud == none {
		t.Error("joining a cluster did not change the pushed inventory")
	}
}
