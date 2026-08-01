package admincore

import (
	"context"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/runtime"
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
	if got.NodeCount != 0 || got.JoinEndpointAvailable {
		t.Error("status without prober should have counts/flags empty")
	}
}

// TestControlPlaneStatusView_WithProber tests basic status flow with a fake prober.
func TestControlPlaneStatusView_WithProber(t *testing.T) {
	s := newTestServer(t)
	fakeStatus := ControlPlaneStatus{
		Hosted:               true,
		ContainerExists:      true,
		ContainerRunning:     true,
		APIServerServing:     true,
		APIServerStatusCode:  200,
		NodeCount:            3,
		JoinEndpointAvailable: true,
		CheckedAt:            time.Now(),
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
	if got.NodeCount != fakeStatus.NodeCount {
		t.Errorf("node_count = %d, want %d", got.NodeCount, fakeStatus.NodeCount)
	}
	if got.JoinEndpointAvailable != fakeStatus.JoinEndpointAvailable {
		t.Errorf("join_endpoint_available = %v, want %v", got.JoinEndpointAvailable, fakeStatus.JoinEndpointAvailable)
	}
}

// TestDefaultControlPlaneStatusProber_NotHosted tests the prober when hosting is off.
func TestDefaultControlPlaneStatusProber_NotHosted(t *testing.T) {
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
			prober := NewDefaultControlPlaneStatusProber(
				"test-host",
				func() bool { return tt.controlPlaneEnabled },
				func() bool { return false },
				func() runtime.ServerOptions { return runtime.ServerOptions{} },
				nil,
			)

			status, err := prober.ProbeControlPlaneStatus(context.Background())
			if err != nil {
				t.Fatalf("ProbeControlPlaneStatus: %v", err)
			}

			if status.Hosted != tt.expectedHosted {
				t.Errorf("hosted = %v, want %v", status.Hosted, tt.expectedHosted)
			}
		})
	}
}

// TestFakeProber_TableDriven uses table-driven test with a fake prober to
// cover combinations of container states and apiserver health without calling
// the real runtime probe (which would fail on missing containers).
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
				Hosted:                false,
				ContainerExists:       false,
				ContainerRunning:      false,
				APIServerServing:      false,
				APIServerStatusCode:   0,
				NodeCount:             0,
				JoinEndpointAvailable: false,
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:                false,
				ContainerExists:       false,
				ContainerRunning:      false,
				APIServerServing:      false,
				APIServerStatusCode:   0,
				NodeCount:             0,
				JoinEndpointAvailable: false,
			},
		},
		{
			name: "hosting on, container exists, apiserver ready",
			fakeStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      true,
				APIServerServing:      true,
				APIServerStatusCode:   200,
				NodeCount:             2,
				JoinEndpointAvailable: true,
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      true,
				APIServerServing:      true,
				APIServerStatusCode:   200,
				NodeCount:             2,
				JoinEndpointAvailable: true,
			},
		},
		{
			name: "hosting on, container exists, apiserver not serving",
			fakeStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      true,
				APIServerServing:      false,
				APIServerStatusCode:   0,
				NodeCount:             2,
				JoinEndpointAvailable: true,
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      true,
				APIServerServing:      false,
				APIServerStatusCode:   0,
				NodeCount:             2,
				JoinEndpointAvailable: true,
			},
		},
		{
			name: "hosting on, container not running",
			fakeStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      false,
				APIServerServing:      false,
				APIServerStatusCode:   0,
				NodeCount:             0,
				JoinEndpointAvailable: true,
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      false,
				APIServerServing:      false,
				APIServerStatusCode:   0,
				NodeCount:             0,
				JoinEndpointAvailable: true,
			},
		},
		{
			name: "hosting on, endpoint not configured",
			fakeStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      true,
				APIServerServing:      true,
				APIServerStatusCode:   200,
				NodeCount:             1,
				JoinEndpointAvailable: false,
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      true,
				APIServerServing:      true,
				APIServerStatusCode:   200,
				NodeCount:             1,
				JoinEndpointAvailable: false,
			},
		},
		{
			name: "hosting on, multiple nodes",
			fakeStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      true,
				APIServerServing:      true,
				APIServerStatusCode:   200,
				NodeCount:             5,
				JoinEndpointAvailable: true,
			},
			expectedStatus: ControlPlaneStatus{
				Hosted:                true,
				ContainerExists:       true,
				ContainerRunning:      true,
				APIServerServing:      true,
				APIServerStatusCode:   200,
				NodeCount:             5,
				JoinEndpointAvailable: true,
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
			if status.NodeCount != tt.expectedStatus.NodeCount {
				t.Errorf("node_count = %d, want %d", status.NodeCount, tt.expectedStatus.NodeCount)
			}
			if status.JoinEndpointAvailable != tt.expectedStatus.JoinEndpointAvailable {
				t.Errorf("join_endpoint_available = %v, want %v", status.JoinEndpointAvailable, tt.expectedStatus.JoinEndpointAvailable)
			}
		})
	}
}
