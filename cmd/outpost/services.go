package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/mesh"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

// This file is outpost's whole relationship with services after the
// inetd/systemd split (docs/bashy-service-design.md):
//
//   - `outpost supervisord` (supervisord.go) is INIT: it keeps exactly the
//     outpost daemon and `bashy supervisord` alive.
//   - `bashy supervisord` is the MONITOR: it owns every unit's process,
//     health, restart policy and start limits.
//   - the daemon — here — publishes what the units declare and withdraws it
//     when they stop being active, and hosts the capabilities whose
//     implementation lives inside this process.
//
// What used to be here instead: a 30 s loop per enabled service that ran
// `bashy <name> status`, grepped the output for the word "stopped", and
// re-ran `bashy <name> start` if it found it — with the service's app route
// and mesh name registered once at boot and never withdrawn, so a service
// that died stayed advertised until the daemon restarted.

// serviceStore wires the unit registry outpost reads desired state from and
// the monitor it reads runtime state from.
//
// Declarations come from fleet's registry rings in process: the embedded
// built-in seeds, the shared dirs on $BASHY_SERVICES_PATH, and the
// host-local overlays `bashy service set|enable` writes. Runtime state of a
// process unit comes from the monitor's own CLI.
func serviceStore() *svcunit.Store {
	return &svcunit.Store{
		Catalog: fleet.New(),
		Monitor: &svcunit.Monitor{Run: svcunit.ExecRunner(bashyResolver.Path)},
	}
}

// serviceControl is the admincore.ServiceOps implementation: the unit store
// for declarations and desired state, the reconciler for live state and the
// republish nudge. Keeping Kick here is what makes a toggle apply without a
// daemon restart.
type serviceControl struct {
	*svcunit.Store
	reconciler *svcunit.Reconciler
	// cli runs `bashy service …` for the one write fleet's Go API does not
	// expose: a dotted-path field override.
	cli svcunit.Runner
}

func (c *serviceControl) Snapshot() svcunit.Snapshot {
	if c.reconciler == nil {
		return svcunit.Snapshot{}
	}
	return c.reconciler.Snapshot()
}

func (c *serviceControl) Kick() {
	if c.reconciler != nil {
		c.reconciler.Kick()
	}
}

// SetFields applies declaration overrides through `bashy service set`.
//
// enable/disable is a single boolean on a record, so it goes through fleet's
// public Go API (Store.SetEnabled) — no subprocess, same validation. A
// dotted-path edit needs fleet's path editor, which is unexported, so it goes
// through the CLI that owns it rather than being reimplemented here. Two
// writers, one store, one set of rules.
func (c *serviceControl) SetFields(ctx context.Context, name string, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	if c.cli == nil {
		return fmt.Errorf("service %q: declaration overrides need the bashy binary (`bashy service set`)", name)
	}
	if _, ok := c.Unit(name); !ok {
		return &svcunit.UnknownUnitError{Name: name, Known: unitNames(c.Store)}
	}
	args := []string{"service", "set", name}
	for _, k := range sortedKeys(fields) {
		args = append(args, "--set", k+"="+fields[k])
	}
	_, err := c.cli(ctx, args...)
	return err
}

func unitNames(st *svcunit.Store) []string {
	units, _ := st.All()
	out := make([]string, 0, len(units))
	for _, u := range units {
		out = append(out, u.Name)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Deterministic order so a multi-field `set` is reproducible in logs.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// appExposer adapts the live AppRegistry to the reconciler's narrow
// interface. The registry is in internal/agent, which must not depend on
// svcunit, so the translation lives here.
type appExposer struct{ reg *agent.AppRegistry }

func (a appExposer) RegisterApp(name, target string, meta svcunit.AppMeta) error {
	return a.reg.RegisterWithMeta(name, target, agent.AppMeta{
		RequireLogin:       meta.RequireLogin,
		ElevationRequired:  meta.ElevationRequired,
		TrustCloudIdentity: meta.TrustCloudIdentity,
		SSOSecret:          meta.SSOSecret,
		IndexPath:          meta.IndexPath,
	})
}

func (a appExposer) UnregisterApp(name string) { a.reg.Unregister(name) }

// meshExposer adapts the mesh forwarder.
type meshExposer struct{ host *mesh.Host }

func (m meshExposer) Expose(name, addr string) { m.host.Forwarder().Expose(name, addr) }
func (m meshExposer) Unexpose(name string)     { m.host.Forwarder().Unexpose(name) }

// serviceSecrets mints and persists the per-app trusted-identity HMAC keys a
// unit's `expose.app.trust_cloud_identity` asks for. The declaration never
// holds the key — it only says that one is wanted.
type serviceSecrets struct {
	path string
	fc   *conf.FileConfig
}

func (s serviceSecrets) SSOSecret(app string) (string, error) {
	return conf.EnsureServiceSSOSecret(s.path, s.fc, app)
}

// startServiceReconciler wires the exposure reconciler into the daemon's
// errgroup and returns the control surface admincore hands to the SPA, MCP
// and CLI.
//
// apps may be nil (nothing to publish to) and meshHost may be nil (mesh off);
// the reconciler simply has one fewer surface to reconcile.
func startServiceReconciler(g *errgroup.Group, ctx context.Context, store *svcunit.Store, cfgPath string, fc *conf.FileConfig,
	apps *agent.AppRegistry, meshHost *mesh.Host, deps capabilityDeps) *serviceControl {

	// Pin (or leave at latest) the bashy release the self-heal auto-install
	// fetches. Read once at boot; changing it schedules a restart, which
	// re-enters here.
	if fc != nil {
		bashyResolver.SetVersion(fc.BashyVersion)
	}
	// Fleet auto-roll: reconcile an existing outpost-managed bashy to the
	// matched version at boot even on a host with no enabled unit — it may
	// still run bashy for deploy jobs. Best-effort; never blocks startup.
	g.Go(func() error {
		bashyResolver.ReconcileExisting(ctx)
		return nil
	})

	if store == nil {
		store = serviceStore()
	}
	if _, err := migrateLegacyServices(cfgPath, fc, store); err != nil {
		slog.Warn("legacy service migration", "err", err)
	}
	// The in-process capabilities this daemon hosts. The caller built each
	// one with the live objects only it has (the app registry, the mesh
	// forwarder, the load sensor); the one thing resolved here is the forge
	// a CI runner defaults to, which is read from the unit store — the same
	// store the reconciler reads declarations from.
	deps.Forge = func(unit string) string { return localForgeURL(store, unit) }
	caps := outpostCapabilities(deps)
	internal, err := svcunit.Hosted(caps)
	if err != nil {
		// A duplicate capability id is a programming error in the wiring
		// below, not an operator mistake: say so and carry on without the
		// internal provider rather than refusing to boot.
		slog.Error("service capabilities: registry rejected the wiring", "err", err)
		internal = nil
	}

	rdeps := svcunit.Deps{
		Source:   store,
		Secrets:  serviceSecrets{path: cfgPath, fc: fc},
		Internal: internal,
		// The monitor's owner socket: it decides which of this daemon's
		// internal units run, and is told what they actually did.
		Owner: service.Client{},
	}
	if apps != nil {
		rdeps.Apps = appExposer{reg: apps}
	}
	if meshHost != nil {
		rdeps.Mesh = meshExposer{host: meshHost}
	}
	r := svcunit.New(rdeps)
	g.Go(func() error { return r.Run(ctx) })

	if ids := internalCapabilityIDs(caps); len(ids) > 0 {
		slog.Info("service capabilities registered in the outpost daemon", "capabilities", strings.Join(ids, ","))
	}
	return &serviceControl{Store: store, reconciler: r, cli: svcunit.ExecRunner(bashyResolver.Path)}
}

func internalCapabilityIDs(caps map[string]service.Capability) []string {
	out := make([]string, 0, len(caps))
	for id := range caps {
		out = append(out, id)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// capabilityEnabled reports whether this host has an ENABLED unit declaring
// the given in-process capability id.
//
// It keys off the CAPABILITY, never a unit name: the name belongs to the
// operator (two `outpost.ollama` units on different sockets are legal), the
// capability id is the cross-repo contract. One read of declared desired
// state, used by the few daemon decisions that are not themselves units —
// bringing up the mesh and peer plane for sharding, and reporting whether
// the LLM pool is wanted in `outpost pool status`.
func capabilityEnabled(st *svcunit.Store, capID string) bool {
	if st == nil {
		return false
	}
	units, _ := st.All()
	for _, u := range units {
		if u.IsRetired() || !u.Enabled {
			continue
		}
		if u.ProviderName() != fleet.ServiceProviderInternal {
			continue
		}
		if u.Capability == capID {
			return true
		}
	}
	return false
}

// localForgeURL is the git-forge base URL a dependent unit (act_runner) uses
// when the operator did not name one: the loom unit's declared loopback
// listener, but only while that unit is enabled.
//
// This replaces `fc.LoomOn() && fc.LoomPortOrDefault()`. The port now lives
// in loom's declaration, which is the only place it is written, so a port
// override through `bashy service set loom --set ports.0.port=…` moves the
// runner's instance with it instead of leaving two values to drift.
func localForgeURL(st *svcunit.Store, unit string) string {
	if st == nil {
		return ""
	}
	u, ok := st.Unit(unit)
	if !ok || !u.Enabled {
		return ""
	}
	port, ok := u.PrimaryPort()
	if !ok || port.Port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port.Port)
}
