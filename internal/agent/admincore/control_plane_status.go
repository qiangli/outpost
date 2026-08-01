package admincore

import (
	"context"
	"time"

	"github.com/qiangli/outpost/internal/agent/runtime"
)

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

	// NodeCount is the number of nodes in the cluster joining this plane.
	// Zero when the plane is not hosted or cannot be queried; absence of a
	// cluster join path does not mean zero nodes (they may exist but be
	// unreachable).
	NodeCount int `json:"node_count"`

	// JoinEndpointAvailable reports whether this host has configured a join
	// endpoint (has_endpoint) for workers to reach. The actual endpoint is never
	// returned; it lives in the token reveal path.
	JoinEndpointAvailable bool `json:"join_endpoint_available"`

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
// cmd/outpost/main.go. It probes the container, apiserver, and cluster
// using the existing readiness surfaces in internal/agent/runtime.
type defaultControlPlaneStatusProber struct {
	// agentName is the outpost's registered identity (the host name).
	agentName string
	// controlPlaneEnabled reports whether hosting is actually on.
	controlPlaneEnabled func() bool
	// joinEndpointAvailable reports whether a join endpoint has been
	// configured (independent of whether the container is running).
	joinEndpointAvailable func() bool
	// serverOptions builds the runtime.ServerOptions for this host.
	// Used to probe the container and apiserver.
	serverOptions func() runtime.ServerOptions
	// nodeCount returns the number of cluster nodes joined to this plane.
	// May return 0 if the plane cannot be reached; an error is only for
	// unexpected system failures.
	nodeCount func(ctx context.Context) (int, error)
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

	status.JoinEndpointAvailable = p.joinEndpointAvailable()

	// Probe the container and apiserver.
	opts := p.serverOptions()
	health := runtime.CheckServer(ctx, opts)
	status.ContainerExists = health.ContainerExists
	status.ContainerRunning = health.ContainerRunning
	status.APIServerServing = health.Serving
	status.APIServerStatusCode = health.Status

	// Query node count. A failure here is not fatal; we report what we could
	// measure and leave the field at zero.
	if p.nodeCount != nil {
		if count, err := p.nodeCount(ctx); err == nil {
			status.NodeCount = count
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
	joinEndpointAvailable func() bool,
	serverOptions func() runtime.ServerOptions,
	nodeCount func(ctx context.Context) (int, error),
) ControlPlaneStatusProber {
	return &defaultControlPlaneStatusProber{
		agentName:             agentName,
		controlPlaneEnabled:   controlPlaneEnabled,
		joinEndpointAvailable: joinEndpointAvailable,
		serverOptions:         serverOptions,
		nodeCount:             nodeCount,
	}
}
