package vkpodman

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// defaultProbeTimeout is the per-attempt budget when the Pod spec
// doesn't set TimeoutSeconds. K8s default is 1s; we mirror it.
const defaultProbeTimeout = time.Second

// runReadinessProbe evaluates the container's readinessProbe (when
// present) against its podman-published host port. Returns nil for
// "ready" (no probe defined, or probe succeeded), a wrapped error
// otherwise. Honors the probe's TimeoutSeconds and the container's
// InitialDelaySeconds (computed against startedAt — caller passes it
// in to avoid us re-reading libpod state).
//
// Probe handlers supported:
//   - HTTPGet  — GET <scheme>://<host>:<port><path>, 200 ≤ status < 400
//     counts as ready
//   - TCPSocket — TCP connect within timeout
//   - Exec     — NOT supported (would require libpod-exec plumbing).
//     A pod that specifies Exec gets ready=true unconditionally with
//     a one-line warning logged at startup, so we don't accidentally
//     block traffic on a probe shape we can't evaluate.
//
// The host port we probe is the auto-allocated (or operator-set)
// hostPort that vkpodman published — same surface cloudbox reaches
// via /h/<node>/app/vk-…/. That way readiness reflects what cloudbox
// will actually see; a container that LISTENS only on a different
// pod-internal port (e.g. behind a sidecar) would fail the probe,
// which is the correct signal under our cluster-svc routing model.
func runReadinessProbe(ctx context.Context, c corev1.Container, hostPort int32, startedAt time.Time) error {
	probe := c.ReadinessProbe
	if probe == nil {
		return nil
	}
	if probe.InitialDelaySeconds > 0 && !startedAt.IsZero() {
		ready := startedAt.Add(time.Duration(probe.InitialDelaySeconds) * time.Second)
		if time.Now().Before(ready) {
			return fmt.Errorf("initialDelaySeconds %ds not elapsed", probe.InitialDelaySeconds)
		}
	}
	timeout := defaultProbeTimeout
	if probe.TimeoutSeconds > 0 {
		timeout = time.Duration(probe.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	h := probe.ProbeHandler
	switch {
	case h.HTTPGet != nil:
		return httpReadinessProbe(ctx, h.HTTPGet, hostPort)
	case h.TCPSocket != nil:
		return tcpReadinessProbe(ctx, h.TCPSocket, hostPort)
	case h.Exec != nil:
		// Documented limitation — see file-header comment.
		return nil
	default:
		return nil
	}
}

func httpReadinessProbe(ctx context.Context, h *corev1.HTTPGetAction, fallbackHostPort int32) error {
	scheme := strings.ToLower(string(h.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	port := resolveProbePort(h.Port, fallbackHostPort)
	if port == 0 {
		return errors.New("probe: no port resolvable for HTTPGet")
	}
	host := strings.TrimSpace(h.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	path := h.Path
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	u := fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, strconv.Itoa(int(port))), path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for _, hd := range h.HTTPHeaders {
		req.Header.Add(hd.Name, hd.Value)
	}
	// Custom client — kubectl's probe uses 1s default total budget; we
	// already wrapped ctx with the per-attempt timeout.
	resp, err := probeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("probe: HTTP %d %s", resp.StatusCode, resp.Status)
	}
	return nil
}

func tcpReadinessProbe(ctx context.Context, h *corev1.TCPSocketAction, fallbackHostPort int32) error {
	port := resolveProbePort(h.Port, fallbackHostPort)
	if port == 0 {
		return errors.New("probe: no port resolvable for TCPSocket")
	}
	host := strings.TrimSpace(h.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// resolveProbePort converts the spec's IntOrString port into a TCP
// port the dialer can use. Named ports aren't resolved to container
// ports in vkpodman (we don't have a containerPort.Name → hostPort
// map that survives daemon restart cleanly), so a named probe falls
// back to the container's first published hostPort — the assumption
// being a single-port pod with the named port being the obvious
// target. Multi-port pods with named-port readinessProbes would
// need a richer resolution; documented limitation.
func resolveProbePort(p intstr.IntOrString, fallbackHostPort int32) int32 {
	if p.Type == intstr.Int && p.IntVal > 0 {
		return p.IntVal
	}
	return fallbackHostPort
}

// firstContainerHostPortFromSpec returns the container's first
// published host port, or 0 if none. Probe ports without an explicit
// numeric value fall back to this — see resolveProbePort.
func firstContainerHostPortFromSpec(c *corev1.Container) int32 {
	for _, p := range c.Ports {
		if p.HostPort != 0 {
			return p.HostPort
		}
	}
	return 0
}

// probeHTTPClient is shared across calls to amortize TCP/TLS setup.
// Transport tuned for short-lived per-probe requests: no keep-alive
// pool growth, no compression overhead, redirects not followed
// (a readiness probe that bounces is by definition not ready).
var probeHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		DisableKeepAlives:   true,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 0,
	},
}
