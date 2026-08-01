package runtime

import (
	"context"
	"log/slog"
	"time"
)

// Supervision for a HOSTED control plane.
//
// A one-shot UpServer is not enough. The container's entrypoint exits 1 on any
// inner failure ON PURPOSE, expecting a supervisor to recreate it — and that
// host-side supervisor did not exist, so a control plane that died stayed dead
// until the next daemon restart, with every symptom landing on the WORKERS
// ("connection refused", "unknown authority"), which is the last place anyone
// looks for a control plane that never came back.
//
// And `podman ps` reporting Up is not evidence the apiserver serves: the
// entrypoint can hold the container up with a dead k3s inside. So the
// supervisor gates on the apiserver readiness probe (health.go), not just on
// container state — container up + apiserver dead for a sustained window is a
// restart trigger, exactly like a crashed container.
//
// It mirrors the agent-join side's joinClusterWithRetry: a loop under the
// daemon errgroup, capped exponential backoff, unbounded attempts, stopping
// only when the daemon's context is cancelled.

// SupervisorConfig tunes SuperviseServer. The zero value is usable — every
// field falls back to a production-sane default via withDefaults.
type SupervisorConfig struct {
	// CheckInterval is how often the readiness probe runs once the container
	// is up.
	CheckInterval time.Duration
	// UnhealthyGrace is how long the apiserver must be continuously unhealthy
	// (container gone, or up but not serving) before the container is
	// recreated. A single missed probe is not a restart: k3s can be briefly
	// unresponsive under load, and a hair-trigger supervisor is its own
	// outage.
	UnhealthyGrace time.Duration
	// FirstBackoff / MaxBackoff bound the retry after a bring-up or recreate
	// failure — e.g. a container engine still cold at boot, the same reason
	// the join side retries rather than treating the first failure as fatal.
	FirstBackoff time.Duration
	MaxBackoff   time.Duration
}

const (
	defaultCheckInterval  = 15 * time.Second
	defaultUnhealthyGrace = 90 * time.Second
	defaultFirstBackoff   = 5 * time.Second
	defaultMaxBackoff     = 2 * time.Minute
)

func (c SupervisorConfig) withDefaults() SupervisorConfig {
	if c.CheckInterval <= 0 {
		c.CheckInterval = defaultCheckInterval
	}
	if c.UnhealthyGrace <= 0 {
		c.UnhealthyGrace = defaultUnhealthyGrace
	}
	if c.FirstBackoff <= 0 {
		c.FirstBackoff = defaultFirstBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = defaultMaxBackoff
	}
	return c
}

// SuperviseServer keeps this host's control-plane container up and serving for
// the life of ctx. It returns nil when ctx is cancelled and never returns
// otherwise — run it under the daemon errgroup.
//
// A bring-up failure is never terminal: unlike the previous one-shot UpServer,
// a cold engine at boot only delays the plane, it does not disable it until the
// next restart.
func SuperviseServer(ctx context.Context, opts ServerOptions, cfg SupervisorConfig) error {
	return (&serverSupervisor{
		opts:  opts,
		cfg:   cfg.withDefaults(),
		up:    UpServer,
		check: CheckServer,
		now:   time.Now,
	}).run(ctx)
}

// serverSupervisor carries the seams the tests replace: up and check stand in
// for the real podman/HTTP calls, now for the clock. Production wiring is
// UpServer / CheckServer / time.Now — so a test never touches a container
// engine or the network.
type serverSupervisor struct {
	opts  ServerOptions
	cfg   SupervisorConfig
	up    func(context.Context, ServerOptions) error
	check func(context.Context, ServerOptions) ServerHealth
	now   func() time.Time
}

func (s *serverSupervisor) run(ctx context.Context) error {
	backoff := s.cfg.FirstBackoff
	for {
		if err := s.up(ctx, s.opts); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("control plane: bring-up failed, will retry",
				"err", err, "retry_in", backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, s.cfg.MaxBackoff)
			continue
		}
		// A clean bring-up resets the backoff for the next failure.
		backoff = s.cfg.FirstBackoff

		// Monitor readiness until the apiserver is unhealthy past the grace
		// window (or ctx is cancelled). A recreate loops us back to the top to
		// re-establish and reset monitoring.
		if !s.monitor(ctx) {
			return nil
		}
	}
}

// monitor probes readiness on CheckInterval and returns true when the container
// should be recreated (unhealthy for the whole grace window), or false when ctx
// was cancelled.
func (s *serverSupervisor) monitor(ctx context.Context) bool {
	var unhealthySince time.Time
	for {
		if !sleepCtx(ctx, s.cfg.CheckInterval) {
			return false
		}
		h := s.check(ctx, s.opts)
		if h.Serving {
			if !unhealthySince.IsZero() {
				slog.Info("control plane: apiserver serving again", "detail", h.String())
			}
			unhealthySince = time.Time{}
			continue
		}
		if unhealthySince.IsZero() {
			unhealthySince = s.now()
			slog.Warn("control plane: apiserver not serving",
				"detail", h.String(),
				"container_running", h.ContainerRunning,
				"grace", s.cfg.UnhealthyGrace)
		}
		if s.now().Sub(unhealthySince) >= s.cfg.UnhealthyGrace {
			slog.Warn("control plane: unhealthy past grace, recreating container",
				"detail", h.String())
			recreate := s.opts
			recreate.ForceRecreate = true
			if err := s.up(ctx, recreate); err != nil {
				if ctx.Err() != nil {
					return false
				}
				// Leave the retry to the outer loop, which backs off.
				slog.Warn("control plane: recreate failed, will retry", "err", err)
			}
			return true
		}
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	if cur *= 2; cur > max {
		return max
	}
	return cur
}

// sleepCtx waits for d or until ctx is cancelled, returning false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
