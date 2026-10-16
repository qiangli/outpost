package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/otel"
)

// The two ycode-proxy-backed surface families as in-process capabilities:
//
//   - `outpost.otel` — the observability views. ycode's bearer-authed proxy
//     fronts an embedded Prometheus / Alertmanager / VictoriaLogs / Jaeger /
//     Perses stack; each becomes its own tunnel app so a portal user can
//     drill into one host through the existing per-(host, app) elevation
//     cookie, with the ycode bearer injected on the way out so no
//     cloudbox-side caller has to learn ycode's credentials.
//   - `outpost.ycode` — the ycode UI surfaces (chat, canvas, the classic
//     SPA), published the same way.
//
// Capabilities because the proxy URL and its bearer are DETECTED from
// ycode's own manifest (outpost never spawns or restarts ycode — the
// operator owns its lifecycle), and because each surface's app route needs
// a per-surface capability descriptor and the bearer-injecting proxy wrap.
// The `settings` are the per-family knobs the retained inventory names:
// `pool` for otel, `share` for ycode.

// otelCapability publishes the observability surfaces.
type otelCapability struct {
	Apps   *agent.AppRegistry
	Detect func() otel.Target

	idleState
	mu         sync.Mutex
	registered []string
	poolShared bool
}

// PoolShared reports whether cloudbox may federate queries and fan out
// alert rules across this host's observability stack — the old
// `otel_pool_enabled`, now `settings.pool` on the unit. False while the
// unit is not active: there is nothing to federate.
func (c *otelCapability) PoolShared() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.poolShared
}

func (c *otelCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Apps == nil {
		return fmt.Errorf("otel: no app registry on this host")
	}
	t := c.detect()
	if !t.Available {
		_ = c.Deactivate(ctx)
		c.set("otel", cfg, fmt.Sprintf("otel: unavailable: no local observability proxy detected (tried %s) — run `ycode serve`, or the bashy observability stack", t.ManifestPath))
		return nil
	}
	if err := c.Deactivate(ctx); err != nil {
		return err
	}
	c.clear()
	wrap := otel.BearerInjector(t.Token)
	var registered []string
	for _, surface := range otel.Surfaces() {
		target := t.ProxyURL + otel.SubPath(surface)
		if err := c.Apps.RegisterWithMeta(surface, target, agent.AppMeta{
			RequireLogin: true,
			Capabilities: &agent.AppCapabilities{Type: surface},
		}); err != nil {
			slog.Warn("otel: register surface", "surface", surface, "err", err)
			continue
		}
		c.Apps.SetProxyWrap(surface, wrap)
		registered = append(registered, surface)
	}
	if len(registered) == 0 {
		return fmt.Errorf("otel: no observability surface could be registered")
	}
	c.mu.Lock()
	c.registered, c.poolShared = registered, settingBoolDefault(cfg, "pool", true)
	c.mu.Unlock()
	slog.Info("otel: observability surfaces registered", "surfaces", strings.Join(registered, ","), "proxy", t.ProxyURL, "pool", c.PoolShared())
	return nil
}

// Configure re-detects and re-registers: ycode may have restarted with a
// new bearer, and `settings.pool` is read on each pass.
func (c *otelCapability) Configure(ctx context.Context, cfg service.Config) error {
	return c.Activate(ctx, cfg)
}

func (c *otelCapability) Deactivate(context.Context) error {
	c.mu.Lock()
	registered := c.registered
	c.registered, c.poolShared = nil, false
	c.mu.Unlock()
	if c.Apps == nil {
		return nil
	}
	for _, surface := range registered {
		c.Apps.SetProxyWrap(surface, nil)
		c.Apps.Unregister(surface)
	}
	return nil
}

// Health re-probes the ycode proxy. When the operator stops ycode the
// surfaces are withdrawn rather than left as tiles that 502.
func (c *otelCapability) Health(ctx context.Context) error {
	if cfg, idle := c.pending(); idle {
		return c.Activate(ctx, cfg)
	}
	c.mu.Lock()
	n := len(c.registered)
	c.mu.Unlock()
	if n == 0 {
		return fmt.Errorf("otel: not activated")
	}
	if t := c.detect(); !t.Available {
		return fmt.Errorf("otel: the local observability proxy is no longer reachable (%s)", t.ManifestPath)
	}
	return nil
}

func (c *otelCapability) detect() otel.Target {
	if c.Detect != nil {
		return c.Detect()
	}
	return otel.Detect()
}

// ycodeCapability publishes the ycode UI surfaces.
type ycodeCapability struct {
	Apps   *agent.AppRegistry
	Detect func() otel.Target

	mu         sync.Mutex
	registered []string
}

func (c *ycodeCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Apps == nil {
		return fmt.Errorf("ycode: no app registry on this host")
	}
	t := c.detect()
	if !t.Available {
		return fmt.Errorf("ycode: the ycode proxy is not running (tried %s) — outpost never starts ycode; run `ycode serve`", t.ManifestPath)
	}
	if err := c.Deactivate(ctx); err != nil {
		return err
	}
	wrap := otel.BearerInjector(t.Token)
	// RequireLogin is off by default for these owner-targeted agentic
	// surfaces (matching custom-app conventions; cloudbox's host gate still
	// keeps non-owners out via HostShare). An operator who wants OS-password
	// elevation sets settings.require_login — it applies to ALL surfaces
	// uniformly, as it did before.
	requireLogin := settingBool(cfg, "require_login")
	want := ycodeShareSet(cfg)
	var registered []string
	for _, s := range otel.YcodeSurfaces() {
		if !want[s.Name] {
			continue
		}
		target := t.ProxyURL + s.Path
		if err := c.Apps.RegisterWithMeta(s.Name, target, agent.AppMeta{
			RequireLogin: requireLogin,
			Capabilities: &agent.AppCapabilities{Type: s.Name},
		}); err != nil {
			slog.Warn("ycode: register surface", "surface", s.Name, "err", err)
			continue
		}
		c.Apps.SetProxyWrap(s.Name, wrap)
		registered = append(registered, s.Name)
	}
	if len(registered) == 0 {
		return fmt.Errorf("ycode: settings.share selected no known surface (known: %s)", strings.Join(ycodeSurfaceNames(), ", "))
	}
	c.mu.Lock()
	c.registered = registered
	c.mu.Unlock()
	slog.Info("ycode: UI surfaces registered", "surfaces", strings.Join(registered, ","), "proxy", t.ProxyURL, "require_login", requireLogin)
	return nil
}

func (c *ycodeCapability) Configure(ctx context.Context, cfg service.Config) error {
	return c.Activate(ctx, cfg)
}

func (c *ycodeCapability) Deactivate(context.Context) error {
	c.mu.Lock()
	registered := c.registered
	c.registered = nil
	c.mu.Unlock()
	if c.Apps == nil {
		return nil
	}
	for _, surface := range registered {
		c.Apps.SetProxyWrap(surface, nil)
		c.Apps.Unregister(surface)
	}
	return nil
}

func (c *ycodeCapability) Health(context.Context) error {
	c.mu.Lock()
	n := len(c.registered)
	c.mu.Unlock()
	if n == 0 {
		return fmt.Errorf("ycode: not activated")
	}
	if t := c.detect(); !t.Available {
		return fmt.Errorf("ycode: the ycode proxy is no longer reachable (%s)", t.ManifestPath)
	}
	return nil
}

func (c *ycodeCapability) detect() otel.Target {
	if c.Detect != nil {
		return c.Detect()
	}
	return otel.Detect()
}

// ycodeShareSet resolves which surfaces `settings.share` selects: a
// comma-separated list of names, `all` for the whole catalog, or — when the
// setting is absent — the catalog's own defaults (today just the canonical
// chat tile, so enabling the unit lands the operator somewhere useful).
//
// This replaces the ycode_share_surfaces map: one declared list instead of
// a per-surface boolean overlay, because a list is what `bashy service set`
// can write and what an operator can read back.
func ycodeShareSet(cfg service.Config) map[string]bool {
	out := map[string]bool{}
	names := settingList(cfg, "share")
	if len(names) == 0 {
		for _, s := range otel.YcodeSurfaces() {
			out[s.Name] = s.DefaultOn
		}
		return out
	}
	for _, n := range names {
		if strings.EqualFold(n, "all") {
			for _, s := range otel.YcodeSurfaces() {
				out[s.Name] = true
			}
			return out
		}
		out[n] = true
	}
	return out
}

func ycodeSurfaceNames() []string {
	all := otel.YcodeSurfaces()
	out := make([]string, 0, len(all))
	for _, s := range all {
		out = append(out, s.Name)
	}
	return out
}
