package ollama

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// The provisioned GPU worker's own stop timers. The master stops an idle
// worker once its warm window lapses; this watchdog is the second,
// independent stop so a lost control plane can never leave a GPU running.
// Both knobs are env-only, like WorkerRequestIDEnv: the bootstrapper sets
// them (or leaves the defaults) and nothing else should.
const (
	GuestIdleEnv       = "OUTPOST_GPU_WORKER_IDLE"
	GuestMaxSessionEnv = "OUTPOST_GPU_WORKER_MAX_SESSION"

	// DefaultGuestIdle is deliberately longer than the master's warm
	// window, so the master's stop normally wins and this one only fires
	// when the master is gone.
	DefaultGuestIdle = 20 * time.Minute
	// DefaultGuestMaxSession caps a session regardless of load.
	DefaultGuestMaxSession = 4 * time.Hour
)

// Trigger reasons, logged and handed to the final push.
const (
	GuestStopIdle       = "idle"
	GuestStopMaxSession = "max_session"
)

const (
	defaultGuestInterval = 30 * time.Second
	defaultGuestRetry    = 30 * time.Second
	guestFinalPushWait   = 20 * time.Second
)

// GuestWatchdogDurations reads GuestIdleEnv and GuestMaxSessionEnv (Go
// durations, e.g. "20m", "4h"). Unset keeps the default; an unparseable or
// non-positive value also keeps the default and is reported in warnings —
// a typo must never disable the stop.
func GuestWatchdogDurations() (idle, maxSession time.Duration, warnings []string) {
	read := func(env string, def time.Duration) time.Duration {
		v := strings.TrimSpace(os.Getenv(env))
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			warnings = append(warnings, fmt.Sprintf("%s=%q is not a positive duration; using %s", env, v, def))
			return def
		}
		return d
	}
	return read(GuestIdleEnv, DefaultGuestIdle), read(GuestMaxSessionEnv, DefaultGuestMaxSession), warnings
}

// GuestWatchdogConfig wires the watchdog. Everything with a side effect
// (clock, activity probe, final push, power-off) is injected so tests never
// power anything off.
type GuestWatchdogConfig struct {
	// RequestID is the provisioned-worker marker (WorkerRequestID()).
	// Empty — every ordinary host — means Run returns at once, unarmed.
	RequestID string

	// Idle stops the host after this long with no loaded model and no
	// in-flight inference; MaxSession after this much host uptime
	// regardless of load. Zero → the defaults.
	Idle, MaxSession time.Duration

	// Active reports whether the host is serving: a model resident in
	// /api/ps or a request in flight. An error counts as NOT active — a
	// dead daemon must not keep the GPU running.
	Active func(ctx context.Context) (bool, error)

	// FinalPush sends one last registry update before power-off. Optional;
	// a failure is logged and never blocks the stop.
	FinalPush func(ctx context.Context, reason string) error

	// PowerOff powers the host off. A failure is logged and retried every
	// Retry until it succeeds or ctx ends. Nil → PowerOffHost.
	PowerOff func() error

	// Uptime is the host's uptime, so the session cap survives a daemon
	// restart. Nil → HostUptime; an error counts from daemon start.
	Uptime func() (time.Duration, error)

	// Interval is the check cadence, Retry the power-off retry delay.
	// Zero → defaults.
	Interval, Retry time.Duration

	// Now and Sleep are the clock. Nil → time.Now / a ctx-aware timer.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	Logger *slog.Logger
}

// GuestWatchdog powers a provisioned GPU worker off when it idles or
// outlives its session.
type GuestWatchdog struct{ cfg GuestWatchdogConfig }

// NewGuestWatchdog fills cfg's defaults.
func NewGuestWatchdog(cfg GuestWatchdogConfig) *GuestWatchdog {
	if cfg.Idle <= 0 {
		cfg.Idle = DefaultGuestIdle
	}
	if cfg.MaxSession <= 0 {
		cfg.MaxSession = DefaultGuestMaxSession
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultGuestInterval
	}
	if cfg.Retry <= 0 {
		cfg.Retry = defaultGuestRetry
	}
	if cfg.PowerOff == nil {
		cfg.PowerOff = PowerOffHost
	}
	if cfg.Uptime == nil {
		cfg.Uptime = HostUptime
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &GuestWatchdog{cfg: cfg}
}

// Run blocks until a trigger powers the host off (nil), or ctx ends (nil
// while still watching; the last power-off error if one was being retried).
func (g *GuestWatchdog) Run(ctx context.Context) error {
	c := g.cfg
	if c.RequestID == "" {
		return nil
	}
	start := c.Now()
	sessionStart := start
	if up, err := c.Uptime(); err != nil {
		c.Logger.Warn("gpu worker watchdog: host uptime unknown; capping the session from daemon start", "err", err)
	} else {
		sessionStart = start.Add(-up)
	}
	lastActive := start
	c.Logger.Info("gpu worker watchdog: armed", "idle", c.Idle, "max_session", c.MaxSession)
	for {
		if err := c.Sleep(ctx, c.Interval); err != nil {
			return nil
		}
		now := c.Now()
		if up := now.Sub(sessionStart); up >= c.MaxSession {
			return g.stop(ctx, GuestStopMaxSession, "uptime", up.Round(time.Second))
		}
		if c.Active != nil {
			active, err := c.Active(ctx)
			if err != nil {
				c.Logger.Debug("gpu worker watchdog: activity probe failed; counting as idle", "err", err)
			}
			if active {
				lastActive = now
			}
		}
		if idle := now.Sub(lastActive); idle >= c.Idle {
			return g.stop(ctx, GuestStopIdle, "idle_for", idle.Round(time.Second))
		}
	}
}

func (g *GuestWatchdog) stop(ctx context.Context, reason, key string, d time.Duration) error {
	c := g.cfg
	c.Logger.Warn("gpu worker watchdog: stopping this host", "reason", reason, key, d,
		"idle", c.Idle, "max_session", c.MaxSession)
	if c.FinalPush != nil {
		pctx, cancel := context.WithTimeout(ctx, guestFinalPushWait)
		err := c.FinalPush(pctx, reason)
		cancel()
		if err != nil {
			c.Logger.Warn("gpu worker watchdog: final registry push failed; powering off anyway", "err", err)
		}
	}
	for {
		err := c.PowerOff()
		if err == nil {
			c.Logger.Warn("gpu worker watchdog: power-off issued", "reason", reason)
			return nil
		}
		c.Logger.Error("gpu worker watchdog: power-off failed; retrying", "reason", reason, "err", err, "retry", c.Retry)
		if serr := c.Sleep(ctx, c.Retry); serr != nil {
			return fmt.Errorf("gpu worker watchdog: power-off (%s) never succeeded: %w", reason, err)
		}
	}
}

// HostUptime reads /proc/uptime (Linux). Elsewhere it errors, and the
// watchdog caps the session from daemon start.
func HostUptime() (time.Duration, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("/proc/uptime: empty")
	}
	s, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/uptime: %w", err)
	}
	return time.Duration(s * float64(time.Second)), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
