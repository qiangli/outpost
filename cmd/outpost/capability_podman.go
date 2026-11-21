package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/localsock"
	"github.com/qiangli/outpost/internal/agent/sandbox"
)

// The two podman-socket proxies as in-process capabilities:
//
//   - `outpost.podman` — the RAW libpod passthrough. Root-equivalent, so
//     admin-role and login-required; the mount a trusted operator uses for
//     their own machine.
//   - `outpost.sandbox` — the FILTERED mount. Same socket, separate app,
//     proxy wrapped by the sandbox filter (strips privileged, host
//     namespaces, host binds, added caps, devices; clamps memory/cpu/pids),
//     plus the capacity intercept and the image prewarmer. This is the
//     mount a thin client or an untrusted tenant talks to.
//
// Both are capabilities rather than `provider: process` units for the same
// reason: there is no child to run. The endpoint is DETECTED on this host
// (and platform-dependent — a Windows podman answers on a named pipe), and
// the app route carries facets a declaration cannot express: the detector's
// scheme, the admin role, the capability descriptor cloudbox discovers
// sandbox hosts by, the proxy wrap and the `/_pool/capacity` intercept.
//
// Socket detection stays HERE, in the capability, exactly as the retained
// inventory asks: a declaration that pinned a socket path would be wrong on
// the next host. `settings.socket` is available as an operator override for
// a non-standard endpoint, and nothing else is needed to use either mount.

// podmanCapability is the raw libpod passthrough.
type podmanCapability struct {
	Apps   *agent.AppRegistry
	Detect func() agent.BuiltinTarget
	// EnsureMachine starts an existing stopped podman machine when
	// `settings.autostart_machine` is true (defaults to ensurePodmanRuntime).
	// Never runs `podman machine init`.
	EnsureMachine func(context.Context) (string, error)
	Now           func() time.Time
	BackoffBase   time.Duration

	idleState
	mu      sync.Mutex
	socket  string
	readyAt string

	startCancel context.CancelFunc
	starting    bool
	nextTry     time.Time
	failures    int
	noMachine   bool
	lastErr     string
}

const (
	defaultPodmanAutostartBackoff  = 15 * time.Second
	maxPodmanAutostartBackoff      = 5 * time.Minute
	noPodmanMachineRecheckInterval = 5 * time.Minute
)

func (c *podmanCapability) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *podmanCapability) backoff(failures int) time.Duration {
	base := c.BackoffBase
	if base <= 0 {
		base = defaultPodmanAutostartBackoff
	}
	d := base
	for i := 1; i < failures && d < maxPodmanAutostartBackoff; i++ {
		d *= 2
	}
	if d > maxPodmanAutostartBackoff {
		d = maxPodmanAutostartBackoff
	}
	return d
}

// Activate registers the mount. A host with no podman socket is not a
// failure: the capability goes idle (nothing published, reason in
// Unavailable) and Health re-probes until a socket appears.
//
// When `settings.autostart_machine` is true on a host with an existing
// stopped podman machine, Activate remains non-blocking (never stalling
// daemon boot or burning the upgrade confirm window): it rests Inactive with
// `unavailable: starting podman machine...` and kicks off a single-flight
// background start with backoff.
func (c *podmanCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Apps == nil {
		return fmt.Errorf("podman: no app registry on this host")
	}
	autostart := settingBoolDefault(cfg, "autostart_machine", false)
	if !autostart {
		c.stopAutostart()
	}
	readyTCP := strings.TrimSpace(setting(cfg, "ready_tcp"))
	bt, err := c.resolve(cfg)
	if err != nil {
		if isAbsent(err) {
			c.withdrawApp()
			if autostart {
				reason := c.triggerAutostart(cfg)
				c.set("podman", cfg, reason)
				return nil
			}
			c.set("podman", cfg, err.Error())
			return nil
		}
		return err
	}
	if readyTCP != "" && !probeTCPAddr(readyTCP, 2*time.Second) {
		c.withdrawApp()
		c.set("podman", cfg, fmt.Sprintf("unavailable: podman socket %s is up, waiting for %s", bt.Socket, readyTCP))
		return nil
	}
	c.mu.Lock()
	c.failures, c.noMachine, c.lastErr, c.nextTry = 0, false, "", time.Time{}
	c.socket, c.readyAt = bt.Socket, readyTCP
	c.mu.Unlock()
	c.clear()
	if err := c.Apps.RegisterFromConfig(conf.AppConfig{
		// Scheme comes from the detector, never a literal: on Windows
		// podman's endpoint is a named pipe, so a hardcoded "unix" would
		// register an unreachable app.
		Name: agent.BuiltinPodman, Scheme: bt.Scheme, Socket: bt.Socket,
		// RequireLogin MUST be set explicitly: the raw podman socket is
		// root-equivalent. Role "admin" alone does NOT gate it — the
		// Role→RequireLogin migration only runs for configs read from JSON,
		// which this in-code AppConfig bypasses, so without this the mount
		// reached cloudbox's anonymous proxy path.
		Role: "admin", RequireLogin: true, Enabled: true,
	}); err != nil {
		return fmt.Errorf("podman: register app: %w", err)
	}
	slog.Info("podman: proxying the libpod socket", "socket", bt.Socket, "scheme", bt.Scheme)
	return nil
}

func (c *podmanCapability) triggerAutostart(cfg service.Config) string {
	c.mu.Lock()
	now := c.now()
	if c.starting {
		c.mu.Unlock()
		return "unavailable: starting podman machine..."
	}
	if c.noMachine && now.Before(c.nextTry) {
		msg := "unavailable: " + c.lastErr
		c.mu.Unlock()
		return msg
	}
	if c.failures > 0 && now.Before(c.nextTry) {
		wait := c.nextTry.Sub(now).Round(time.Second)
		msg := fmt.Sprintf("unavailable: podman machine start backoff (%s remaining): %s", wait, c.lastErr)
		c.mu.Unlock()
		return msg
	}
	ensureFn := c.EnsureMachine
	if ensureFn == nil {
		ensureFn = ensurePodmanRuntime
	}
	runCtx, cancel := context.WithCancel(context.Background())
	c.starting = true
	c.startCancel = cancel
	c.mu.Unlock()

	go func() {
		_, err := ensureFn(runCtx)
		cancelled := runCtx.Err() != nil
		cancel()

		c.mu.Lock()
		c.starting = false
		c.startCancel = nil
		if cancelled {
			c.mu.Unlock()
			return
		}
		finished := c.now()
		var nextReason string
		switch {
		case err == nil:
			c.failures, c.noMachine, c.lastErr, c.nextTry = 0, false, "", time.Time{}
			nextReason = "unavailable: podman machine started, awaiting socket probe"
		case errors.Is(err, errNoPodmanMachine):
			c.noMachine = true
			c.lastErr = err.Error()
			c.nextTry = finished.Add(noPodmanMachineRecheckInterval)
			nextReason = "unavailable: " + err.Error()
			slog.Warn("podman: autostart skipped because no podman machine exists", "err", err)
		default:
			c.failures++
			delay := c.backoff(c.failures)
			c.lastErr = err.Error()
			c.nextTry = finished.Add(delay)
			nextReason = fmt.Sprintf("unavailable: podman machine start failed (retry in %s): %s", delay, err.Error())
			slog.Warn("podman: autostart failed; will retry after backoff", "attempt", c.failures, "backoff", delay, "err", err)
		}
		c.mu.Unlock()

		if prevCfg, idle := c.pending(); idle {
			c.set("podman", prevCfg, nextReason)
		} else {
			c.set("podman", cfg, nextReason)
		}
	}()
	return "unavailable: starting podman machine..."
}

func (c *podmanCapability) withdrawApp() {
	c.mu.Lock()
	had := c.socket != ""
	c.socket, c.readyAt = "", ""
	c.mu.Unlock()
	if had && c.Apps != nil {
		c.Apps.Unregister(agent.BuiltinPodman)
	}
}

func (c *podmanCapability) stopAutostart() {
	c.mu.Lock()
	cancel := c.startCancel
	c.startCancel = nil
	c.starting = false
	c.failures, c.noMachine, c.lastErr, c.nextTry = 0, false, "", time.Time{}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Configure re-resolves the socket and re-registers, which is the whole of
// this capability's state — no restart needed.
func (c *podmanCapability) Configure(ctx context.Context, cfg service.Config) error {
	return c.Activate(ctx, cfg)
}

func (c *podmanCapability) Deactivate(context.Context) error {
	c.stopAutostart()
	c.clear()
	c.withdrawApp()
	return nil
}

// Health probes the socket. A podman that went away takes the mount's app
// route with it instead of leaving a 502 tile on the portal.
func (c *podmanCapability) Health(ctx context.Context) error {
	if cfg, idle := c.pending(); idle {
		return c.Activate(ctx, cfg)
	}
	c.mu.Lock()
	socket := c.socket
	readyAt := c.readyAt
	c.mu.Unlock()
	if err := probePodmanSocket("podman", socket); err != nil {
		return err
	}
	if readyAt != "" && !probeTCPAddr(readyAt, 2*time.Second) {
		return fmt.Errorf("podman: ready_tcp endpoint %s is no longer reachable", readyAt)
	}
	return nil
}

func probeTCPAddr(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (c *podmanCapability) resolve(cfg service.Config) (agent.BuiltinTarget, error) {
	return resolvePodmanTarget("podman", c.Detect, setting(cfg, "socket"))
}

// sandboxCapability is the filtered container mount plus its prewarmer.
type sandboxCapability struct {
	Apps   *agent.AppRegistry
	Detect func() agent.BuiltinTarget

	prewarm goroutineRunner

	mu     sync.Mutex
	socket string
	svc    *sandbox.Service
}

// sandboxPolicy is the declaration as the filter reads it. The ESCAPE knobs
// are deliberately not here: the mount always strips privileged, host
// namespaces, host binds outside the scratch prefix, added capabilities,
// device passthrough and confinement-disabling security options, and no
// setting can re-enable them.
func sandboxPolicy(cfg service.Config) sandbox.Policy {
	return sandbox.Policy{
		MaxMemoryBytes:    settingInt(cfg, "max_memory_mb", 0) * 1024 * 1024,
		NanoCPUs:          int64(settingFloat(cfg, "cpus", 0) * 1e9),
		PidsLimit:         settingInt(cfg, "pids_limit", 0),
		MaxContainers:     int(settingInt(cfg, "max_containers", 0)),
		AllowedImages:     settingList(cfg, "allowed_images"),
		ScratchHostPrefix: setting(cfg, "scratch_dir"),
	}
}

// sandboxPrewarmImages is the set the prewarmer pulls: the explicit list
// when the declaration has one, else the concrete entries of the allowlist —
// pre-pulling exactly what callers may run.
func sandboxPrewarmImages(cfg service.Config) []string {
	if imgs := settingList(cfg, "prewarm_images"); len(imgs) > 0 {
		return imgs
	}
	// A wildcard entry is a pattern, not something that can be pulled.
	var out []string
	for _, img := range settingList(cfg, "allowed_images") {
		if strings.Contains(img, "*") {
			continue
		}
		out = append(out, img)
	}
	return out
}

func (c *sandboxCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Apps == nil {
		return fmt.Errorf("sandbox: no app registry on this host")
	}
	bt, err := c.resolve(cfg)
	if err != nil {
		return err
	}
	// Withdraw first: AddIntercept APPENDS, so re-activating over a live
	// registration would mount a second /_pool/capacity handler on every
	// Configure pass. Unregister is what clears a name's intercepts.
	if err := c.Deactivate(ctx); err != nil {
		return err
	}
	if err := c.Apps.RegisterFromConfig(conf.AppConfig{
		Name: agent.BuiltinSandbox, Scheme: bt.Scheme, Socket: bt.Socket,
		RequireLogin: true, Enabled: true,
	}); err != nil {
		return fmt.Errorf("sandbox: register app: %w", err)
	}
	svc := sandbox.NewService(sandboxPolicy(cfg))
	c.Apps.SetCapabilities(agent.BuiltinSandbox, &agent.AppCapabilities{Type: sandbox.CapabilityType})
	c.Apps.SetProxyWrap(agent.BuiltinSandbox, svc.WrapProxy)
	c.Apps.AddIntercept(agent.BuiltinSandbox, "/_pool/capacity", svc.CapacityHandler())
	c.mu.Lock()
	c.socket, c.svc = bt.Socket, svc
	c.mu.Unlock()
	// Image prewarmer: pull the runnable images so a remote create+start
	// skips the pull cost, which is the dominant cold-start latency.
	if imgs := sandboxPrewarmImages(cfg); len(imgs) > 0 {
		pw := sandbox.NewPrewarmer(bt.Socket, imgs)
		svc.SetPrewarmer(pw)
		c.prewarm.start(ctx, pw.Run)
	} else {
		c.prewarm.stop(ctx)
	}
	slog.Info("sandbox: filtered container mount registered", "socket", bt.Socket, "prewarm", len(sandboxPrewarmImages(cfg)))
	return nil
}

// Configure rebuilds the policy and the prewarm set live — the filter reads
// the policy per request, so a changed cap applies to the next create.
func (c *sandboxCapability) Configure(ctx context.Context, cfg service.Config) error {
	return c.Activate(ctx, cfg)
}

func (c *sandboxCapability) Deactivate(ctx context.Context) error {
	c.prewarm.stop(ctx)
	c.mu.Lock()
	had := c.socket != ""
	c.socket, c.svc = "", nil
	c.mu.Unlock()
	if had && c.Apps != nil {
		c.Apps.SetProxyWrap(agent.BuiltinSandbox, nil)
		c.Apps.Unregister(agent.BuiltinSandbox)
	}
	return nil
}

func (c *sandboxCapability) Health(context.Context) error {
	c.mu.Lock()
	socket := c.socket
	c.mu.Unlock()
	return probePodmanSocket("sandbox", socket)
}

func (c *sandboxCapability) resolve(cfg service.Config) (agent.BuiltinTarget, error) {
	return resolvePodmanTarget("sandbox", c.Detect, setting(cfg, "socket"))
}

// resolvePodmanTarget picks the endpoint a podman-backed mount proxies to:
// an operator-declared socket when there is one, else what the detector
// found. A refusal names what was tried, so an enabled unit on a host with
// no podman reports WHY instead of looking fine while serving nothing — the
// improvement over the old boot path, which logged one line and moved on.
func resolvePodmanTarget(what string, detect func() agent.BuiltinTarget, explicit string) (agent.BuiltinTarget, error) {
	if explicit != "" {
		path := localsock.Normalize(explicit)
		return agent.BuiltinTarget{
			Name: what, Scheme: localsock.Scheme(path), Socket: path, Available: true,
		}, nil
	}
	if detect == nil {
		detect = agent.DetectPodman
	}
	bt := detect()
	if !bt.Available || bt.Socket == "" {
		tried := bt.Socket
		if tried == "" {
			tried = "the standard rootless and system paths"
		}
		return bt, newAbsent("%s: unavailable: no podman socket detected (tried %s) — install podman, start the machine, or set `settings.socket`", what, tried)
	}
	return bt, nil
}

// probePodmanSocket is the health check both mounts share.
func probePodmanSocket(what, socket string) error {
	if socket == "" {
		return fmt.Errorf("%s: not activated", what)
	}
	if !localsock.Probe(socket, 2*time.Second) {
		return fmt.Errorf("%s: podman socket %s is no longer reachable", what, socket)
	}
	return nil
}
