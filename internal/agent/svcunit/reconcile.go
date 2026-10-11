package svcunit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// UnitSource supplies the declarations and the monitor's runtime state. The
// Client implements it against `bashy service`; tests implement it directly.
type UnitSource interface {
	Units(ctx context.Context) ([]fleet.Service, error)
	Statuses(ctx context.Context) ([]service.Status, error)
}

// EventSource is the optional push half. When the monitor publishes state
// transitions (S16), the reconciler reacts to them instead of waiting for
// the next poll; without it the poll alone drives reconciliation. Keeping
// this an interface is what lets S16 plug events in with no change here.
type EventSource interface {
	Events(ctx context.Context) (<-chan service.Event, error)
}

// Deps is everything the reconciler acts on. Any of them may be nil: an
// unpaired host has no tunnel registry worth publishing to, a mesh-off host
// has no forwarder, and a process with no registered capabilities has no
// internal provider.
type Deps struct {
	Source   UnitSource
	Apps     AppExposer
	Mesh     MeshExposer
	Secrets  Secrets
	Internal *service.Internal
	// Owner is the link to the service monitor's owner control socket
	// (service.Client implements it). When set and the monitor answers, the
	// monitor decides which of this process's internal units are wanted now
	// (`desired`) and is told what the capabilities actually did (`report`).
	// When it is nil or the monitor is unreachable, the local declaration's
	// enabled bit decides, exactly as before: a host without a monitor must
	// not lose its in-process features.
	Owner OwnerLink
	// Runtime is handed to the internal provider on Activate/Configure. Emit
	// is filled in by the reconciler so a capability's own failure shows up
	// as a state change and withdraws its exposure.
	Runtime service.Runtime
	// Interval is the status poll period. Zero uses DefaultInterval.
	Interval time.Duration
	Log      *slog.Logger
}

// OwnerLink is the owner half of the monitor's control protocol, as the
// reconciler uses it. service.Client is the production implementation.
type OwnerLink interface {
	Desired(owner string) ([]service.Status, error)
	Report(st service.Status) error
}

// DefaultInterval is how often the monitor's structured status is re-read.
// This is NOT the retired 30 s "grep bashy X status for the word stopped"
// poller: no process is launched or judged here. It is the fallback refresh
// for the exposure reconciler, short because withdrawing a failed unit's
// tunnel route promptly is the point, and cheap because it is one status
// call for the whole host.
const DefaultInterval = 5 * time.Second

// Reconciler keeps outpost's tunnel and mesh exposure — and the internal
// capabilities this process hosts — equal to the unit declarations and the
// monitor's state.
type Reconciler struct {
	deps Deps
	log  *slog.Logger

	kick chan struct{}

	mu sync.Mutex
	// units is the last declaration snapshot. Re-read on boot and on Kick,
	// not on every status poll: a declaration only changes when something
	// writes it, and every writer inside outpost calls Kick.
	units []fleet.Service
	// live is what is currently published, keyed by unit name.
	live map[string]Exposure
	// states is the last state seen per unit, for the status surface.
	states map[string]service.Status
	// stale records that the monitor could not be reached on the last pass.
	stale bool
	// applied fingerprints the declaration last pushed into each internal
	// capability, so Configure runs on a change and not on every pass.
	applied map[string]string
	// activateErrs is the last activation failure reported per unit, so an
	// unchanged refusal is logged once instead of on every pass.
	activateErrs map[string]string
	// linkNote is the last monitor-link failure logged, so an unchanged
	// condition (no monitor running) is said once.
	linkNote string
	// reported tracks which internal units this process has told the
	// monitor about, so shutdown can report them inactive.
	reported map[string]bool
}

// noteActivateFailure records an activation outcome and reports whether it
// is worth logging. A nil error clears the unit's remembered reason.
func (r *Reconciler) noteActivateFailure(unit string, err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.activateErrs, unit)
		return false
	}
	reason := err.Error()
	if r.activateErrs[unit] == reason {
		return false
	}
	r.activateErrs[unit] = reason
	return true
}

// New returns a reconciler. It publishes nothing until Reconcile or Run runs.
func New(deps Deps) *Reconciler {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{
		deps:         deps,
		log:          log,
		kick:         make(chan struct{}, 1),
		live:         map[string]Exposure{},
		states:       map[string]service.Status{},
		applied:      map[string]string{},
		activateErrs: map[string]string{},
		reported:     map[string]bool{},
	}
}

// Kick asks for an immediate reconcile. Every outpost writer of desired state
// (builtins set, the MCP tool, the admin UI) calls it after the write, which
// is what makes a toggle apply live instead of scheduling a daemon restart.
// Non-blocking and coalescing.
func (r *Reconciler) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx ends: on boot, on every Kick, on each interval
// tick, and — when the source publishes them — on each monitor state event.
func (r *Reconciler) Run(ctx context.Context) error {
	interval := r.deps.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if err := r.ReloadAndReconcile(ctx); err != nil {
		r.log.Warn("service exposure: initial reconcile failed", "err", err)
	}

	var events <-chan service.Event
	if es, ok := r.deps.Source.(EventSource); ok {
		ch, err := es.Events(ctx)
		if err != nil {
			r.log.Debug("service exposure: no monitor event stream; polling only", "err", err)
		} else {
			events = ch
		}
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	defer r.withdrawAll()
	defer r.shutdownInternal()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.kick:
			if err := r.ReloadAndReconcile(ctx); err != nil {
				r.log.Warn("service exposure: reconcile after write failed", "err", err)
			}
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			r.log.Info("service state", "unit", ev.Unit, "from", ev.From, "to", ev.To, "reason", ev.Reason)
			if err := r.Reconcile(ctx); err != nil {
				r.log.Warn("service exposure: reconcile after state event failed", "unit", ev.Unit, "err", err)
			}
		case <-tick.C:
			if err := r.Reconcile(ctx); err != nil {
				r.log.Warn("service exposure: periodic reconcile failed", "err", err)
			}
		}
	}
}

// ReloadAndReconcile re-reads the declarations and then reconciles.
func (r *Reconciler) ReloadAndReconcile(ctx context.Context) error {
	units, err := r.deps.Source.Units(ctx)
	if err != nil {
		// Keep the previous declarations rather than withdrawing everything
		// because one `bashy service list` call failed: a transient failure
		// to READ the desired state is not a statement that the state changed.
		r.log.Warn("service exposure: reading unit declarations failed; keeping last snapshot", "err", err)
		return r.Reconcile(ctx)
	}
	r.mu.Lock()
	r.units = units
	r.mu.Unlock()
	return r.Reconcile(ctx)
}

// Reconcile brings the internal provider and the exposure surface in line
// with the current declarations and the monitor's state. One pass:
//
//  1. read the monitor's state for every unit;
//  2. drive the internal provider's desired state for units this process
//     hosts (and take their state from the provider, which is authoritative
//     for a capability with no child process);
//  3. publish or withdraw each unit's declared app/mesh exposure.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	units := append([]fleet.Service(nil), r.units...)
	r.mu.Unlock()

	states, stale := r.readStates(ctx, units)

	desired, monitorUp := r.readDesired(units)

	var errs []error
	for _, u := range units {
		if st, ok := r.applyInternal(ctx, u, desired, monitorUp); ok {
			states[u.Name] = st
		}
	}

	for _, u := range units {
		st := states[u.Name]
		// The effective state is what exposure is decided on AND what the
		// status surface reports, so the two can never disagree about why a
		// route is or is not published.
		eff := r.effectiveState(u, st, stale)
		if eff != st.State {
			st.Unit, st.Provider, st.State, st.Enabled = u.Name, u.ProviderName(), eff, u.Enabled
			switch {
			case !u.Enabled:
				// Read from the local declaration; the monitor is not why.
				st.Reason = "unit disabled"
			case stale && st.Reason == "":
				st.Reason = "service monitor unreachable"
			}
			states[u.Name] = st
		}
		if err := r.applyExposure(u, eff); err != nil {
			errs = append(errs, err)
		}
	}

	// A unit that vanished from the declarations keeps nothing published.
	declared := map[string]bool{}
	for _, u := range units {
		declared[u.Name] = true
	}
	r.mu.Lock()
	for name := range r.live {
		if !declared[name] {
			r.withdrawLocked(name, "unit removed")
		}
	}
	r.states = states
	r.stale = stale
	r.mu.Unlock()
	return errors.Join(errs...)
}

// readStates asks the monitor for every unit's state. A failure is reported
// as stale rather than as "everything is inactive": see effectiveState.
func (r *Reconciler) readStates(ctx context.Context, units []fleet.Service) (map[string]service.Status, bool) {
	out := map[string]service.Status{}
	rows, err := r.deps.Source.Statuses(ctx)
	if err != nil {
		// Info, not Warn: on a host whose bashy predates the monitor this
		// fires every tick, and an operator needs it visible once without a
		// warning stream. The status surfaces report monitor_reachable.
		r.log.Info("service exposure: monitor status unavailable", "err", err)
		return out, true
	}
	for _, st := range rows {
		out[st.Unit] = st
	}
	// A unit the monitor does not mention is one it is not running.
	for _, u := range units {
		if _, ok := out[u.Name]; !ok {
			out[u.Name] = service.Status{Unit: u.Name, Provider: u.ProviderName(),
				Enabled: u.Enabled, State: service.Inactive, Reason: "not reported by the monitor"}
		}
	}
	return out, false
}

// effectiveState is the state exposure decisions are made on.
//
// The one judgement call in this package: when the monitor cannot be reached
// (it is restarting, or the installed bashy has no monitor yet), outpost does
// NOT tear down a healthy host's tunnel routes. An enabled unit keeps its
// exposure and the condition is reported as degraded — withdrawal needs the
// monitor to actually say the unit is gone. A disabled unit is withdrawn
// regardless: that fact comes from the declaration, which was read locally.
func (r *Reconciler) effectiveState(u fleet.Service, st service.Status, stale bool) service.State {
	if !u.Enabled {
		return service.Inactive
	}
	if stale {
		return service.Degraded
	}
	if st.State == "" {
		return service.Inactive
	}
	return st.State
}

// applyInternal drives the desired state of a unit whose capability lives in
// this process. It returns the provider's status, which is authoritative for
// an internal unit — there is no child for the monitor to observe, and
// Status.PID stays 0 by contract.
//
// Configure runs only when the declaration actually changed, so a 5 s
// reconcile pass does not re-push identical settings into a live capability.
func (r *Reconciler) applyInternal(ctx context.Context, u fleet.Service, desired map[string]bool, monitorUp bool) (service.Status, bool) {
	prov := r.deps.Internal
	if prov == nil || u.ProviderName() != fleet.ServiceProviderInternal {
		return service.Status{}, false
	}
	if u.Owner != fleet.ServiceOwnerOutpost {
		return service.Status{}, false // another process hosts it
	}
	rt := r.deps.Runtime
	rt.Emit = r.emit
	cur := prov.Status(u.Name)
	// wanted is whether the unit should be running now. The monitor decides
	// when it answers and lists the unit (it owns policy: start limits,
	// dependencies); the local declaration can only turn a unit OFF early,
	// so a disable applies before the monitor has re-read it.
	wanted := u.Enabled
	if w, ok := desired[u.Name]; ok {
		wanted = u.Enabled && w
	}
	var applyErr error
	switch {
	case !wanted:
		if cur.State != service.Inactive {
			if err := prov.Deactivate(ctx, u.Name); err != nil {
				r.log.Warn("internal service: deactivate failed", "unit", u.Name, "err", err)
			}
		}
	case cur.State != service.Active:
		if err := prov.Activate(ctx, u, rt); err != nil {
			applyErr = err
			// Once per distinct reason, not once per 5 s pass: an enabled
			// unit whose capability refuses (a CI runner with no
			// registration token, say) is re-activated on every tick, and a
			// repeating identical warning would bury the state it is
			// reporting. A changed reason, or a recovery followed by a new
			// failure, is logged again.
			if r.noteActivateFailure(u.Name, err) {
				r.log.Warn("internal service: activate failed", "unit", u.Name, "capability", u.Capability, "err", err)
			}
		} else {
			r.noteActivateFailure(u.Name, nil)
		}
		r.rememberApplied(u)
	case r.declarationChanged(u):
		err := prov.Configure(ctx, u, rt)
		if errors.Is(err, service.ErrRestartRequired) {
			if derr := prov.Deactivate(ctx, u.Name); derr != nil {
				r.log.Warn("internal service: restart deactivate failed", "unit", u.Name, "err", derr)
			}
			err = prov.Activate(ctx, u, rt)
		}
		if err != nil {
			applyErr = err
			r.log.Warn("internal service: configure failed", "unit", u.Name, "err", err)
		}
		r.rememberApplied(u)
	default:
		// Active and unchanged — but nobody else is watching. A process
		// unit's liveness is the monitor's business; an in-process
		// capability has no child for the monitor to see, so if its own
		// Health says it stopped serving, this pass is the only thing that
		// can notice. Cycle it: Activate either brings it back or refuses
		// with a reason, and the unit stops reporting active either way.
		if err := prov.Health(ctx, u.Name); err != nil {
			if r.noteActivateFailure(u.Name, err) {
				r.log.Warn("internal service: unhealthy; cycling", "unit", u.Name, "capability", u.Capability, "err", err)
			}
			if derr := prov.Deactivate(ctx, u.Name); derr != nil {
				r.log.Warn("internal service: cycle deactivate failed", "unit", u.Name, "err", derr)
			}
			if aerr := prov.Activate(ctx, u, rt); aerr != nil {
				applyErr = aerr
				r.log.Warn("internal service: cycle activate failed", "unit", u.Name, "err", aerr)
			}
			r.rememberApplied(u)
		}
	}
	st := prov.Status(u.Name)
	st.Unit, st.Provider, st.Enabled = u.Name, fleet.ServiceProviderInternal, u.Enabled
	// A capability that refused (or is not registered here at all) never
	// reached the provider's own bookkeeping; say why instead of reporting a
	// bare inactive.
	if applyErr != nil && st.State != service.Active {
		st.State, st.Reason = service.Failed, applyErr.Error()
	}
	if st.State == service.Active {
		st.Health = "ok"
	}
	if monitorUp {
		rep := st
		rep.Enabled = wanted
		r.report(rep)
	}
	return st, true
}

// readDesired asks the monitor which of this process's internal units are
// wanted now. The second result is whether the monitor answered; a nil map
// with false means the local declaration decides (see Deps.Owner).
func (r *Reconciler) readDesired(units []fleet.Service) (map[string]bool, bool) {
	if r.deps.Owner == nil || r.deps.Internal == nil {
		return nil, false
	}
	hosted := false
	for _, u := range units {
		if u.ProviderName() == fleet.ServiceProviderInternal && u.Owner == fleet.ServiceOwnerOutpost {
			hosted = true
			break
		}
	}
	if !hosted {
		return nil, false
	}
	rows, err := r.deps.Owner.Desired(fleet.ServiceOwnerOutpost)
	if err != nil {
		r.noteLink(err)
		return nil, false
	}
	r.noteLink(nil)
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[row.Unit] = row.Enabled
	}
	return out, true
}

// report tells the monitor what this process observes for one internal unit.
// It runs on every pass, not only on change: a monitor that restarted has
// lost every report, and the next pass is what re-adopts the units.
func (r *Reconciler) report(st service.Status) {
	if err := r.deps.Owner.Report(st); err != nil {
		r.noteLink(err)
		return
	}
	r.mu.Lock()
	r.reported[st.Unit] = true
	r.mu.Unlock()
}

// noteLink logs a change in the monitor link, once. A missing monitor is
// normal (older bashy, or the host runs without one) and not a warning.
func (r *Reconciler) noteLink(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	r.mu.Lock()
	changed := r.linkNote != msg
	r.linkNote = msg
	r.mu.Unlock()
	if !changed {
		return
	}
	switch {
	case err == nil:
		r.log.Info("service monitor link up: internal units follow the monitor's desired state")
	case errors.Is(err, service.ErrNoMonitor):
		r.log.Info("service monitor not running: internal units follow their local declaration", "err", err)
	default:
		r.log.Warn("service monitor link failed", "err", err)
	}
}

// shutdownInternal deactivates the internal units this process applied and
// tells the monitor, which reads the owner going away as the units going
// inactive. Without the report a restarting daemon would leave the monitor
// believing its capabilities are still active.
func (r *Reconciler) shutdownInternal() {
	prov := r.deps.Internal
	if prov == nil {
		return
	}
	r.mu.Lock()
	names := make([]string, 0, len(r.reported))
	for n := range r.reported {
		names = append(names, n)
	}
	r.reported = map[string]bool{}
	r.mu.Unlock()
	sort.Strings(names)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, n := range names {
		if err := prov.Deactivate(ctx, n); err != nil {
			r.log.Warn("internal service: deactivate on shutdown failed", "unit", n, "err", err)
		}
		if r.deps.Owner != nil {
			_ = r.deps.Owner.Report(prov.Status(n))
		}
	}
}

// declarationChanged reports whether a unit's configurable surface differs
// from what was last pushed into its capability.
func (r *Reconciler) declarationChanged(u fleet.Service) bool {
	key := declarationKey(u)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied[u.Name] != key
}

func (r *Reconciler) rememberApplied(u fleet.Service) {
	key := declarationKey(u)
	r.mu.Lock()
	r.applied[u.Name] = key
	r.mu.Unlock()
}

// declarationKey fingerprints the parts of a declaration an internal
// capability consumes (service.ConfigOf): settings, env and ports.
func declarationKey(u fleet.Service) string {
	b, err := json.Marshal(struct {
		Settings map[string]string   `json:"settings"`
		Env      map[string]string   `json:"env"`
		Ports    []fleet.ServicePort `json:"ports"`
	}{u.Settings, u.Env, u.Ports})
	if err != nil {
		return u.Name + ":unhashable"
	}
	return string(b)
}

func (r *Reconciler) emit(ev service.Event) {
	r.log.Info("service state", "unit", ev.Unit, "from", ev.From, "to", ev.To, "reason", ev.Reason)
	if f := r.deps.Runtime.Emit; f != nil {
		f(ev)
	}
	r.Kick()
}

// applyExposure publishes or withdraws one unit's declared exposure. It is
// idempotent: an unchanged plan touches nothing, so a 5 s poll does not churn
// the registry.
func (r *Reconciler) applyExposure(u fleet.Service, state service.State) error {
	want, err := Plan(u, state, r.deps.Secrets)
	if err != nil {
		// A declaration outpost cannot publish must not keep a stale route
		// alive, and must be loud: it is an operator error in the YAML.
		r.mu.Lock()
		r.withdrawLocked(u.Name, "invalid exposure declaration")
		r.mu.Unlock()
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	have, published := r.live[u.Name]
	if !want.Allowed {
		if published {
			r.withdrawLocked(u.Name, want.Reason)
		}
		return nil
	}
	if published && sameExposure(have, r.publishable(want)) {
		return nil
	}
	if published {
		r.withdrawLocked(u.Name, "exposure changed")
	}
	// What is recorded as live is what was actually published, not what the
	// declaration asked for. A host with no mesh forwarder (mesh off, or
	// unpaired) cannot publish a mesh name, and claiming one would tell an
	// operator that a peer can reach a service no peer can reach.
	done := want
	done.Mesh = nil
	if want.App != nil {
		if r.deps.Apps == nil {
			done.App = nil
		} else {
			if err := r.deps.Apps.RegisterApp(want.App.Name, want.App.Target, want.App.Meta); err != nil {
				return err
			}
			r.log.Info("service exposed on the tunnel", "unit", u.Name, "app", want.App.Name, "target", want.App.Target)
		}
	}
	if r.deps.Mesh != nil {
		for _, m := range want.Mesh {
			r.deps.Mesh.Expose(m.Name, m.Addr)
			r.log.Info("service exposed on the mesh", "unit", u.Name, "mesh_service", m.Name, "addr", m.Addr)
		}
		done.Mesh = want.Mesh
	}
	r.live[u.Name] = done
	return nil
}

// withdrawLocked removes everything a unit had published. Called with r.mu.
func (r *Reconciler) withdrawLocked(name, reason string) {
	ex, ok := r.live[name]
	if !ok {
		return
	}
	if ex.App != nil && r.deps.Apps != nil {
		r.deps.Apps.UnregisterApp(ex.App.Name)
	}
	if r.deps.Mesh != nil {
		for _, m := range ex.Mesh {
			r.deps.Mesh.Unexpose(m.Name)
		}
	}
	delete(r.live, name)
	r.log.Info("service withdrawn from the tunnel and mesh", "unit", name, "reason", reason,
		"withdrew", ex.Names())
}

func (r *Reconciler) withdrawAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.live))
	for n := range r.live {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r.withdrawLocked(n, "outpost shutting down")
	}
}

// publishable is want reduced to the surfaces this host actually has, so an
// unchanged declaration compares equal to what was published.
func (r *Reconciler) publishable(want Exposure) Exposure {
	out := want
	if r.deps.Apps == nil {
		out.App = nil
	}
	if r.deps.Mesh == nil {
		out.Mesh = nil
	}
	return out
}

func sameExposure(a, b Exposure) bool {
	if (a.App == nil) != (b.App == nil) {
		return false
	}
	if a.App != nil && *a.App != *b.App {
		return false
	}
	if len(a.Mesh) != len(b.Mesh) {
		return false
	}
	for i := range a.Mesh {
		if a.Mesh[i] != b.Mesh[i] {
			return false
		}
	}
	return true
}

// Snapshot is the status projection the admin UI, MCP and `outpost status`
// read: one row per declared unit, with the monitor's structured state and
// what outpost currently publishes for it. It replaces grepping a service's
// text output, and it is where the unwired apphealth prober's job now lives.
type Snapshot struct {
	// MonitorReachable is false when `bashy service status` could not be
	// read on the last pass; the rows then carry the last known state.
	MonitorReachable bool        `json:"monitor_reachable"`
	Units            []UnitState `json:"units"`
}

// UnitState is one unit's row in a Snapshot.
type UnitState struct {
	service.Status
	Description string   `json:"description,omitempty"`
	Ring        string   `json:"ring,omitempty"`
	Publishing  []string `json:"publishing,omitempty"`
}

// Snapshot returns the current status projection.
func (r *Reconciler) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := Snapshot{MonitorReachable: !r.stale}
	for _, u := range r.units {
		st, ok := r.states[u.Name]
		if !ok {
			st = service.Status{Unit: u.Name, Provider: u.ProviderName(), Enabled: u.Enabled, State: service.Inactive}
		}
		st.Enabled = u.Enabled
		row := UnitState{Status: st, Description: u.Description, Ring: u.Ring.String()}
		if ex, live := r.live[u.Name]; live {
			row.Publishing = ex.Names()
			row.Exposed = row.Publishing
		}
		snap.Units = append(snap.Units, row)
	}
	// Units outpost has no work for are still the operator's business: the
	// monitor reported them, so pass its rows through rather than leaving a
	// blank state next to the ones outpost tracks.
	tracked := make(map[string]bool, len(r.units))
	for _, u := range r.units {
		tracked[u.Name] = true
	}
	for name, st := range r.states {
		if !tracked[name] {
			snap.Units = append(snap.Units, UnitState{Status: st})
		}
	}
	sort.Slice(snap.Units, func(i, j int) bool { return snap.Units[i].Unit < snap.Units[j].Unit })
	return snap
}
