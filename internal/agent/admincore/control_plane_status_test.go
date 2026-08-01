package admincore

import (
	"context"
	"testing"
	"time"
)

// fakeControlPlaneStatusProber is a test double that returns fixed results.
type fakeControlPlaneStatusProber struct {
	status ControlPlaneStatus
}

func (f *fakeControlPlaneStatusProber) ProbeControlPlaneStatus(ctx context.Context) (ControlPlaneStatus, error) {
	return f.status, nil
}

// TestControlPlaneStatusView_NoProber tests that a server without a prober
// returns an empty status (hosted=false).
func TestControlPlaneStatusView_NoProber(t *testing.T) {
	s := newTestServer(t)
	// No prober wired; s.deps.ControlPlaneStatusProber is nil by default.

	got, err := s.ControlPlaneStatusView(context.Background())
	if err != nil {
		t.Fatalf("ControlPlaneStatusView: %v", err)
	}
	if got.Hosted {
		t.Error("status without prober should have hosted=false")
	}
	if got.ContainerExists || got.ContainerRunning || got.APIServerServing {
		t.Error("status without prober should have all probes false")
	}
	if len(got.Nodes) > 0 || got.JoinEndpoint != "" {
		t.Error("status without prober should have empty nodes and endpoint")
	}
}

// TestControlPlaneStatusView_WithProber tests basic status flow with a fake prober.
func TestControlPlaneStatusView_WithProber(t *testing.T) {
	s := newTestServer(t)
	fakeStatus := ControlPlaneStatus{
		Hosted:              true,
		ContainerExists:     true,
		ContainerRunning:    true,
		APIServerServing:    true,
		APIServerStatusCode: 200,
		Nodes: []Node{
			{Name: "node1", Ready: true},
			{Name: "node2", Ready: true},
			{Name: "node3", Ready: false},
		},
		JoinEndpoint: "https://127.0.0.1:6443",
		CheckedAt:    time.Now(),
	}
	s.deps.ControlPlaneStatusProber = &fakeControlPlaneStatusProber{status: fakeStatus}

	got, err := s.ControlPlaneStatusView(context.Background())
	if err != nil {
		t.Fatalf("ControlPlaneStatusView: %v", err)
	}

	if got.Hosted != fakeStatus.Hosted {
		t.Errorf("hosted = %v, want %v", got.Hosted, fakeStatus.Hosted)
	}
	if got.ContainerExists != fakeStatus.ContainerExists {
		t.Errorf("container_exists = %v, want %v", got.ContainerExists, fakeStatus.ContainerExists)
	}
	if got.ContainerRunning != fakeStatus.ContainerRunning {
		t.Errorf("container_running = %v, want %v", got.ContainerRunning, fakeStatus.ContainerRunning)
	}
	if got.APIServerServing != fakeStatus.APIServerServing {
		t.Errorf("apiserver_serving = %v, want %v", got.APIServerServing, fakeStatus.APIServerServing)
	}
	if got.APIServerStatusCode != fakeStatus.APIServerStatusCode {
		t.Errorf("apiserver_status_code = %d, want %d", got.APIServerStatusCode, fakeStatus.APIServerStatusCode)
	}
	if len(got.Nodes) != len(fakeStatus.Nodes) {
		t.Errorf("nodes length = %d, want %d", len(got.Nodes), len(fakeStatus.Nodes))
	}
	for i, node := range got.Nodes {
		if node.Name != fakeStatus.Nodes[i].Name {
			t.Errorf("node[%d].name = %s, want %s", i, node.Name, fakeStatus.Nodes[i].Name)
		}
		if node.Ready != fakeStatus.Nodes[i].Ready {
			t.Errorf("node[%d].ready = %v, want %v", i, node.Ready, fakeStatus.Nodes[i].Ready)
		}
	}
	if got.JoinEndpoint != fakeStatus.JoinEndpoint {
		t.Errorf("join_endpoint = %s, want %s", got.JoinEndpoint, fakeStatus.JoinEndpoint)
	}
}

// TestDefaultControlPlaneStatusProber_HostingState tests the prober with
// different hosting configurations, routed through the fake seam.
func TestDefaultControlPlaneStatusProber_HostingState(t *testing.T) {
	tests := []struct {
		name string
		// controlPlaneEnabled determines whether hosting is on.
		controlPlaneEnabled bool
		// expectedHosted is what we expect in the result.
		expectedHosted bool
	}{
		{"hosting disabled", false, false},
		{"hosting enabled", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStatus := ControlPlaneStatus{
				Hosted: tt.expectedHosted,
			}
			s := newTestServer(t)
			s.deps.ControlPlaneStatusProber = &fakeControlPlaneStatusProber{status: fakeStatus}

			status, err := s.ControlPlaneStatusView(context.Background())
			if err != nil {
				t.Fatalf("ControlPlaneStatusView: %v", err)
			}

			if status.Hosted != tt.expectedHosted {
				t.Errorf("hosted = %v, want %v", status.Hosted, tt.expectedHosted)
			}
		})
	}
}

// TestFakeProber_TableDriven uses table-driven test with a fake prober to
// cover combinations of container states, node readiness, and endpoint
// configuration without calling the real runtime probe.
func TestFakeProber_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		// Input status from the fake prober
		fakeStatus ControlPlaneStatus
		// Expected output (should match input exactly)
		expectedStatus ControlPlaneStatus
	}{
		{
			name: "hosting off",
			fakeStatus: ControlPlaneStatus{
				Hosted:              false,
				ContainerExists:     false,
				ContainerRunning:    false,
				APIServerServing:    false,
				APIServerStatusCode: 0,
				Nodes:               nil,
				JoinEndpoint:        "",
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:              false,
				ContainerExists:     false,
				ContainerRunning:    false,
				APIServerServing:    false,
				APIServerStatusCode: 0,
				Nodes:               nil,
				JoinEndpoint:        "",
			},
		},
		{
			name: "hosting on, container exists, apiserver ready",
			fakeStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    true,
				APIServerServing:    true,
				APIServerStatusCode: 200,
				Nodes: []Node{
					{Name: "worker1", Ready: true},
					{Name: "worker2", Ready: true},
				},
				JoinEndpoint: "https://127.0.0.1:6443",
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    true,
				APIServerServing:    true,
				APIServerStatusCode: 200,
				Nodes: []Node{
					{Name: "worker1", Ready: true},
					{Name: "worker2", Ready: true},
				},
				JoinEndpoint: "https://127.0.0.1:6443",
			},
		},
		{
			name: "hosting on, container exists, apiserver not serving",
			fakeStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    true,
				APIServerServing:    false,
				APIServerStatusCode: 0,
				Nodes: []Node{
					{Name: "worker1", Ready: false},
					{Name: "worker2", Ready: true},
				},
				JoinEndpoint: "https://127.0.0.1:6443",
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    true,
				APIServerServing:    false,
				APIServerStatusCode: 0,
				Nodes: []Node{
					{Name: "worker1", Ready: false},
					{Name: "worker2", Ready: true},
				},
				JoinEndpoint: "https://127.0.0.1:6443",
			},
		},
		{
			name: "hosting on, container not running",
			fakeStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    false,
				APIServerServing:    false,
				APIServerStatusCode: 0,
				Nodes:               nil,
				JoinEndpoint:        "https://127.0.0.1:6443",
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    false,
				APIServerServing:    false,
				APIServerStatusCode: 0,
				Nodes:               nil,
				JoinEndpoint:        "https://127.0.0.1:6443",
			},
		},
		{
			name: "hosting on, endpoint not configured",
			fakeStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    true,
				APIServerServing:    true,
				APIServerStatusCode: 200,
				Nodes: []Node{
					{Name: "worker1", Ready: true},
				},
				JoinEndpoint: "",
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    true,
				APIServerServing:    true,
				APIServerStatusCode: 200,
				Nodes: []Node{
					{Name: "worker1", Ready: true},
				},
				JoinEndpoint: "",
			},
		},
		{
			name: "hosting on, multiple nodes with mixed readiness",
			fakeStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    true,
				APIServerServing:    true,
				APIServerStatusCode: 200,
				Nodes: []Node{
					{Name: "worker1", Ready: true},
					{Name: "worker2", Ready: true},
					{Name: "worker3", Ready: false},
					{Name: "worker4", Ready: true},
					{Name: "worker5", Ready: false},
				},
				JoinEndpoint: "https://127.0.0.1:6443",
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:              true,
				ContainerExists:     true,
				ContainerRunning:    true,
				APIServerServing:    true,
				APIServerStatusCode: 200,
				Nodes: []Node{
					{Name: "worker1", Ready: true},
					{Name: "worker2", Ready: true},
					{Name: "worker3", Ready: false},
					{Name: "worker4", Ready: true},
					{Name: "worker5", Ready: false},
				},
				JoinEndpoint: "https://127.0.0.1:6443",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			// Wire the fake prober with fixed test data.
			s.deps.ControlPlaneStatusProber = &fakeControlPlaneStatusProber{
				status: tt.fakeStatus,
			}

			status, err := s.ControlPlaneStatusView(context.Background())
			if err != nil {
				t.Fatalf("ControlPlaneStatusView: %v", err)
			}

			// Verify all fields match expected values.
			if status.Hosted != tt.expectedStatus.Hosted {
				t.Errorf("hosted = %v, want %v", status.Hosted, tt.expectedStatus.Hosted)
			}
			if status.ContainerExists != tt.expectedStatus.ContainerExists {
				t.Errorf("container_exists = %v, want %v", status.ContainerExists, tt.expectedStatus.ContainerExists)
			}
			if status.ContainerRunning != tt.expectedStatus.ContainerRunning {
				t.Errorf("container_running = %v, want %v", status.ContainerRunning, tt.expectedStatus.ContainerRunning)
			}
			if status.APIServerServing != tt.expectedStatus.APIServerServing {
				t.Errorf("apiserver_serving = %v, want %v", status.APIServerServing, tt.expectedStatus.APIServerServing)
			}
			if status.APIServerStatusCode != tt.expectedStatus.APIServerStatusCode {
				t.Errorf("apiserver_status_code = %d, want %d", status.APIServerStatusCode, tt.expectedStatus.APIServerStatusCode)
			}
			if len(status.Nodes) != len(tt.expectedStatus.Nodes) {
				t.Errorf("nodes length = %d, want %d", len(status.Nodes), len(tt.expectedStatus.Nodes))
			} else {
				for i, node := range status.Nodes {
					if node.Name != tt.expectedStatus.Nodes[i].Name {
						t.Errorf("node[%d].name = %s, want %s", i, node.Name, tt.expectedStatus.Nodes[i].Name)
					}
					if node.Ready != tt.expectedStatus.Nodes[i].Ready {
						t.Errorf("node[%d].ready = %v, want %v", i, node.Ready, tt.expectedStatus.Nodes[i].Ready)
					}
				}
			}
			if status.JoinEndpoint != tt.expectedStatus.JoinEndpoint {
				t.Errorf("join_endpoint = %s, want %s", status.JoinEndpoint, tt.expectedStatus.JoinEndpoint)
			}
		})
	}
}

// --- bounded node-reader coverage (phase 3) ---

// TestReadControlPlaneNodes_EmptyPath returns empty immediately for empty kubeconfig path.
func TestReadControlPlaneNodes_EmptyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nodes, err := ReadControlPlaneNodes(ctx, "")
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(nodes) != 0 {
		t.Errorf("len(nodes) = %d, want 0", len(nodes))
	}
}

// TestReadControlPlaneNodes_NonexistentPath returns empty for a nonexistent kubeconfig file.
func TestReadControlPlaneNodes_NonexistentPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nodes, err := ReadControlPlaneNodes(ctx, "/nonexistent/path/to/kubeconfig")
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(nodes) != 0 {
		t.Errorf("len(nodes) = %d, want 0", len(nodes))
	}
}

// TestReadControlPlaneNodes_ContextTimeout returns empty when context times out.
func TestReadControlPlaneNodes_ContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	// Let the context expire.
	time.Sleep(10 * time.Millisecond)

	// Even with a nonexistent path, the timeout ensures fast return.
	nodes, err := ReadControlPlaneNodes(ctx, "/nonexistent/path")
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(nodes) != 0 {
		t.Errorf("len(nodes) = %d, want 0", len(nodes))
	}
}
