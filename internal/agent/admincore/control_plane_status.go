package admincore

import (
	"context"
	"time"

	"github.com/qiangli/outpost/internal/agent/runtime"
)

// Node represents a cluster node joining the control plane.
type Node struct {
	// Name is the node's registered name.
	Name string `json:"name"`
	// Ready reports whether the node is ready to accept workloads.
	Ready bool `json:"ready"`
}

// ControlPlaneStatus is a read-only snapshot of the hosted control plane's
// health and readiness. It never carries credential values — presence is
// reported as has_* booleans only.
type ControlPlaneStatus struct {
	// Hosted is true when this host is configured to host a control plane.
	Hosted bool `json:"hosted"`

	// ContainerExists reports whether the control-plane container exists.
	ContainerExists bool `json:"container_exists"`
	// ContainerRunning reports whether the control-plane container is running.
	// Only meaningful when ContainerExists is true.
	ContainerRunning bool `json:"container_running"`

	// APIServerServing reports whether the apiserver is accepting HTTP connections.
	// Requires the container to be running; a dead container always has
	// APIServerServing false.
	APIServerServing bool `json:"apiserver_serving"`
	// APIServerStatusCode is the HTTP status when APIServerServing is true.
	APIServerStatusCode int `json:"apiserver_status_code,omitempty"`

	// Nodes is the list of cluster nodes joining this plane, with readiness status.
	// Empty when the plane is not hosted or cannot be queried.
	Nodes []Node `json:"nodes,omitempty"`

	// JoinEndpoint is the endpoint URL workers can use to join this control plane.
	// Empty when this host has not configured a join endpoint.
	JoinEndpoint string `json:"join_endpoint,omitempty"`

	// CheckedAt is when this status was last measured.
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

// ControlPlaneStatusProber is the interface seam for testing. Faked
// implementations replace real probes (container inspection, apiserver health
// checks, cluster queries) with test data.
type ControlPlaneStatusProber interface {
	// ProbeControlPlaneStatus returns the current health and readiness.
	ProbeControlPlaneStatus(ctx context.Context) (ControlPlaneStatus, error)
}

// ControlPlaneStatusView returns the current hosted control plane's status.
// Requires a configured control plane; returns hosted=false when none exists.
//
// This method does not return an error for a missing control plane — an
// unpaired host or one that has not enabled hosting returns a zero status
// with hosted=false. It only errors on unexpected probe failures (e.g.
// system lookup failures that should never happen).
func (s *Server) ControlPlaneStatusView(ctx context.Context) (ControlPlaneStatus, error) {
	if s.deps.ControlPlaneStatusProber == nil {
		return ControlPlaneStatus{}, nil
	}
	return s.deps.ControlPlaneStatusProber.ProbeControlPlaneStatus(ctx)
}

// defaultControlPlaneStatusProber is the real implementation, wired in
// cmd/outpost/main.go. It reads cached control-plane health and queries
// cluster state using the existing readiness surfaces in internal/agent/runtime.
type defaultControlPlaneStatusProber struct {
	// agentName is the outpost's registered identity (the host name).
	agentName string
	// controlPlaneEnabled reports whether hosting is actually on.
	controlPlaneEnabled func() bool
	// joinEndpoint returns the join endpoint URL for workers (may be empty).
	// Independent of whether the container is running.
	joinEndpoint func() string
	// nodes returns the list of cluster nodes joined to this plane,
	// with their readiness status. May return empty if the plane cannot be reached;
	// an error is only for unexpected system failures.
	nodes func(ctx context.Context) ([]Node, error)
}

func (p *defaultControlPlaneStatusProber) ProbeControlPlaneStatus(ctx context.Context) (ControlPlaneStatus, error) {
	status := ControlPlaneStatus{
		Hosted:    p.controlPlaneEnabled(),
		CheckedAt: time.Now(),
	}

	if !status.Hosted {
		// Not hosting: no need to probe further.
		return status, nil
	}

	// Read cached health instead of probing. The supervisor maintains
	// LastServerHealth; we reuse that answer without repeating the probe.
	health, ok := runtime.LastServerHealth()
	if ok {
		status.ContainerExists = health.ContainerExists
		status.ContainerRunning = health.ContainerRunning
		status.APIServerServing = health.Serving
		status.APIServerStatusCode = health.Status
	}

	status.JoinEndpoint = p.joinEndpoint()

	// Query node state. A failure here is not fatal; we report what we could
	// measure and leave nodes empty.
	if p.nodes != nil {
		if nodeList, err := p.nodes(ctx); err == nil {
			status.Nodes = nodeList
		}
	}

	return status, nil
}

// NewDefaultControlPlaneStatusProber wires the real implementation.
// Called from cmd/outpost/main.go when the cluster runtime is configured.
//
// The closures capture the dependencies so the prober doesn't need to import
// the cluster packages directly — keeping admincore focused on the interface.
func NewDefaultControlPlaneStatusProber(
	agentName string,
	controlPlaneEnabled func() bool,
	joinEndpoint func() string,
	nodes func(ctx context.Context) ([]Node, error),
) ControlPlaneStatusProber {
	return &defaultControlPlaneStatusProber{
		agentName:           agentName,
		controlPlaneEnabled: controlPlaneEnabled,
		joinEndpoint:        joinEndpoint,
		nodes:               nodes,
	}
}
