package admincore

import (
	"encoding/json"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// `outpost status` on a control-plane host reported cluster.control_plane and
// cluster.control_plane_kubeconfig as null — the first because an omitempty
// bool renders false and "the flag got dropped by a config-write path"
// identically as a missing key, the second because the field simply did not
// exist on the view. Between them, the two questions an operator asks when the
// control-plane reconcilers are not running were both unanswerable from the
// status surface.
func TestClusterView_ReportsControlPlaneHostingFacts(t *testing.T) {
	on := true
	fc := &conf.FileConfig{
		AgentName: "dragon",
		Cluster: &conf.ClusterConfig{
			ControlPlane:           &on,
			ControlPlaneKubeconfig: "/home/op/.kube/outpost-control-plane/k3s.yaml",
		},
	}

	v := toClusterView(fc)
	if !v.ControlPlane {
		t.Errorf("ControlPlane = false, want true")
	}
	if v.ControlPlaneKubeconfig != fc.Cluster.ControlPlaneKubeconfig {
		t.Errorf("ControlPlaneKubeconfig = %q, want %q",
			v.ControlPlaneKubeconfig, fc.Cluster.ControlPlaneKubeconfig)
	}

	// The path is reportable; it is a filename, not a credential. Nothing from
	// inside the kubeconfig may ride along.
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(blob); len(got) == 0 {
		t.Fatal("empty view")
	}
}

// The false case is the one that matters: a host that is SUPPOSED to host the
// plane but whose flag was dropped must render an explicit control_plane:false
// rather than vanishing from the JSON and reading as null.
func TestClusterView_ControlPlaneFalseIsExplicitInJSON(t *testing.T) {
	fc := &conf.FileConfig{AgentName: "dragon", Cluster: &conf.ClusterConfig{}}

	blob, err := json.Marshal(toClusterView(fc))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, present := m["control_plane"]
	if !present {
		t.Fatalf("control_plane is absent from the status JSON — false and "+
			"\"never reported\" must not look alike; got %s", blob)
	}
	if got != false {
		t.Errorf("control_plane = %v, want false", got)
	}
}
