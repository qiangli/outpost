package admincore

import (
	"context"
	"time"

	"github.com/qiangli/outpost/internal/agent/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
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
	// NodeCount is the number of nodes in the cluster.
	NodeCount int `json:"node_count"`

	// JoinEndpoint is the endpoint URL workers can use to join this control plane.
	// Empty when this host has not configured a join endpoint.
	JoinEndpoint string `json:"join_endpoint,omitempty"`

	// HasJoinToken reports whether a join token credential exists (presence only).
	HasJoinToken bool `json:"has_join_token"`
	// HasNodeToken reports whether a node token credential exists (presence only).
	HasNodeToken bool `json:"has_node_token"`
	// HasSTCPSecret reports whether an STCP secret credential exists (presence only).
	HasSTCPSecret bool `json:"has_stcp_secret"`

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
	// hasJoinToken reports whether a join token credential is persisted.
	hasJoinToken func() bool
	// hasNodeToken reports whether a node token credential is persisted.
	hasNodeToken func() bool
	// hasSTCPSecret reports whether an STCP secret credential is persisted.
	hasSTCPSecret func() bool
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
			status.NodeCount = len(nodeList)
		}
	}

	// Report credential presence (never values).
	if p.hasJoinToken != nil {
		status.HasJoinToken = p.hasJoinToken()
	}
	if p.hasNodeToken != nil {
		status.HasNodeToken = p.hasNodeToken()
	}
	if p.hasSTCPSecret != nil {
		status.HasSTCPSecret = p.hasSTCPSecret()
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
	hasJoinToken func() bool,
	hasNodeToken func() bool,
	hasSTCPSecret func() bool,
) ControlPlaneStatusProber {
	return &defaultControlPlaneStatusProber{
		agentName:           agentName,
		controlPlaneEnabled: controlPlaneEnabled,
		joinEndpoint:        joinEndpoint,
		nodes:               nodes,
		hasJoinToken:        hasJoinToken,
		hasNodeToken:        hasNodeToken,
		hasSTCPSecret:       hasSTCPSecret,
	}
}

// ReadControlPlaneNodes queries the cluster nodes from a kubeconfig file.
// Returns empty list if the kubeconfig is unavailable or the query fails;
// an error is only for unexpected system failures.
// The context timeout is honored; a timeout returns an empty list, not an error.
func ReadControlPlaneNodes(ctx context.Context, kubeconfigPath string) ([]Node, error) {
	if kubeconfigPath == "" {
		return []Node{}, nil
	}

	// Load kubeconfig and create a clientset.
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		// Kubeconfig not available or invalid; return empty list, not an error.
		return []Node{}, nil
	}

	// Set a 5-second timeout on the REST client to bound network operations.
	config.Timeout = 5 * time.Second

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		// Failed to create client; return empty list, not an error.
		return []Node{}, nil
	}

	// List nodes from the cluster.
	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		// Failed to list nodes or context cancelled; return empty list, not an error.
		return []Node{}, nil
	}

	// Convert k8s nodes to our Node type.
	var nodes []Node
	for _, k8sNode := range nodeList.Items {
		node := Node{
			Name:  k8sNode.Name,
			Ready: isNodeReady(&k8sNode),
		}
		nodes = append(nodes, node)
	}

	return nodes, nil
}

// isNodeReady checks if a Kubernetes node is in the Ready condition.
func isNodeReady(k8sNode *corev1.Node) bool {
	for _, cond := range k8sNode.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
