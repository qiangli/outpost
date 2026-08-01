package runtime

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Readiness for a HOSTED control plane.
//
// `podman ps` saying Up is not evidence that a cluster works. The container
// runs an entrypoint that supervises k3s and frps; either can be dead, or k3s
// can be alive and refusing to serve, while the container stays Up — and every
// symptom then lands on the WORKERS ("connection refused", "certificate signed
// by unknown authority"), which is the last place anyone looks for a control
// plane that never came up.
//
// So the liveness signal is taken from the apiserver itself, on the same
// address the operator's kubeconfig names.

// serverReadyPath is the apiserver's unauthenticated-by-convention readiness
// endpoint. /readyz exists on every k3s-supported Kubernetes version.
const serverReadyPath = "/readyz"

// probeTimeout is deliberately short. This is a liveness question, not a
// request whose answer we need: a control plane that cannot say hello in five
// seconds is not serving as far as a worker is concerned either.
const probeTimeout = 5 * time.Second

// ServerHealth is a point-in-time answer to "is this host's control plane
// actually serving?". Exported, and cached in LastServerHealth, so a status
// surface can report it without owning a probe of its own.
type ServerHealth struct {
	// Endpoint is the URL that was probed.
	Endpoint string `json:"endpoint"`
	// Serving is the load-bearing field: the apiserver answered in HTTP.
	//
	// ANY well-formed HTTP response counts, including 401 and 403. An
	// apiserver that denies anonymous requests still had to accept the
	// connection, complete a TLS handshake and route the request to say so —
	// which is the whole question. Only a transport error (refused, timed
	// out, TLS never completed) means dead. Treating a 401 as unhealthy
	// would have the supervisor restart a perfectly good, correctly-locked
	// down control plane in a loop.
	Serving bool `json:"serving"`
	// Status is the HTTP status code when Serving.
	Status int `json:"status,omitempty"`
	// Err is the transport error when not Serving.
	Err string `json:"err,omitempty"`
	// ContainerRunning / ContainerExists describe the container the plane
	// runs in. Populated by CheckServer, not by ProbeAPIServer.
	ContainerRunning bool `json:"container_running"`
	ContainerExists  bool `json:"container_exists"`

	CheckedAt time.Time `json:"checked_at"`
}

// probeClient skips certificate verification ON PURPOSE. The target is a
// loopback address published by this host's own container, and k3s signs its
// apiserver with a CA it minted itself — there is no chain to verify against
// without reading the kubeconfig we are trying to find out whether we can
// trust. This connection carries no credentials and reads no data; it exists
// to learn whether something answers. Authentication happens on the operator's
// kubectl connection, which does verify.
var probeClient = &http.Client{
	Timeout: probeTimeout,
	Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: probeTimeout,
	},
}

// APIReadyURL is the address the hosted apiserver's readiness endpoint is
// reachable at FROM THIS HOST — the same host:port the kubeconfig k3s writes
// names, because the container publishes the apiserver there precisely so the
// kubeconfig is valid unmodified.
//
// A wildcard bind is probed on loopback: 0.0.0.0 is where the listener accepts,
// not an address anything dials.
func (o ServerOptions) APIReadyURL() string {
	o.applyDefaults()
	host := o.TunnelBindAddr
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(o.APIPort)) + serverReadyPath
}

// ProbeAPIServer asks one question: does an apiserver answer at endpoint?
//
// It never fails — a dead server is a result, not an error — so callers get a
// classification rather than an error to interpret.
func ProbeAPIServer(ctx context.Context, endpoint string) ServerHealth {
	h := ServerHealth{Endpoint: endpoint, CheckedAt: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		h.Err = err.Error()
		return h
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		h.Err = err.Error()
		return h
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused/closed cleanly;
	// the body is diagnostic ("ok" or a component list) and never needed.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	h.Serving = true
	h.Status = resp.StatusCode
	return h
}

// CheckServer answers the whole question for one host: is the control-plane
// container running, and is the apiserver inside it serving? The result is
// cached for LastServerHealth.
//
// This is the function a status surface calls. It is safe on a host that hosts
// nothing — the container simply does not exist and Serving is false.
func CheckServer(ctx context.Context, opts ServerOptions) ServerHealth {
	opts.applyDefaults()
	running, exists := serverContainerState(ctx, opts)
	h := ProbeAPIServer(ctx, opts.APIReadyURL())
	h.ContainerRunning = running
	h.ContainerExists = exists
	recordServerHealth(h)
	return h
}

// serverContainerState reports whether the control-plane container is running
// and whether it exists at all. A missing container engine reads as "neither",
// which is the correct input for the supervisor: it will try to bring the
// plane up and report the engine error from there rather than from here.
func serverContainerState(ctx context.Context, opts ServerOptions) (running, exists bool) {
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return false, false
	}
	_, running, exists = inspectRuntime(ctx, bin, ServerContainerName(opts.AgentName))
	return running, exists
}

var (
	lastHealthMu sync.RWMutex
	lastHealth   ServerHealth
	lastHealthOK bool
)

func recordServerHealth(h ServerHealth) {
	lastHealthMu.Lock()
	lastHealth, lastHealthOK = h, true
	lastHealthMu.Unlock()
}

// LastServerHealth returns the most recent readiness result the supervisor (or
// a CheckServer caller) observed, and whether there has been one at all.
//
// Exported for the status surface: reading a cached answer costs nothing, so a
// status call never has to wait on a network timeout to say something useful.
func LastServerHealth() (ServerHealth, bool) {
	lastHealthMu.RLock()
	defer lastHealthMu.RUnlock()
	return lastHealth, lastHealthOK
}

// String renders a health result for logs and for an operator-facing status
// line. Kept here so every surface says the same thing.
func (h ServerHealth) String() string {
	switch {
	case h.Serving:
		return fmt.Sprintf("serving (HTTP %d) at %s", h.Status, h.Endpoint)
	case h.Err != "":
		return fmt.Sprintf("not serving at %s: %s", h.Endpoint, h.Err)
	default:
		return "not serving at " + h.Endpoint
	}
}
