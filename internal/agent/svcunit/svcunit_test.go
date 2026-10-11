package svcunit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// fakeApps / fakeMesh record what the reconciler publishes and withdraws.
type fakeApps struct {
	mu   sync.Mutex
	live map[string]string
	meta map[string]AppMeta
	fail error
}

func newFakeApps() *fakeApps {
	return &fakeApps{live: map[string]string{}, meta: map[string]AppMeta{}}
}

func (f *fakeApps) RegisterApp(name, target string, meta AppMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.live[name] = target
	f.meta[name] = meta
	return nil
}

func (f *fakeApps) UnregisterApp(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, name)
	delete(f.meta, name)
}

func (f *fakeApps) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.live))
	for n := range f.live {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

type fakeMesh struct {
	mu   sync.Mutex
	live map[string]string
}

func newFakeMesh() *fakeMesh { return &fakeMesh{live: map[string]string{}} }

func (f *fakeMesh) Expose(name, addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[name] = addr
}

func (f *fakeMesh) Unexpose(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, name)
}

func (f *fakeMesh) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.live))
	for n := range f.live {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// fakeSource is a scripted `bashy service` producer.
type fakeSource struct {
	mu       sync.Mutex
	units    []fleet.Service
	statuses []service.Status
	statErr  error
	unitErr  error
}

func (f *fakeSource) Units(context.Context) ([]fleet.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fleet.Service(nil), f.units...), f.unitErr
}

func (f *fakeSource) Statuses(context.Context) ([]service.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]service.Status(nil), f.statuses...), f.statErr
}

func (f *fakeSource) set(units []fleet.Service, statuses []service.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.units, f.statuses = units, statuses
}

type fixedSecrets struct{ secret string }

func (s fixedSecrets) SSOSecret(string) (string, error) { return s.secret, nil }

// webUnit is a process unit with both an app and a mesh exposure. It is
// deliberately a NAME the reconciler has never heard of: nothing in the
// package may behave differently for a seeded built-in.
func webUnit(name string, enabled bool) fleet.Service {
	return fleet.Service{
		Name:        name,
		Kind:        fleet.KindService,
		Description: "a third-party daemon declared in YAML",
		Exec:        []string{"some-gateway", "serve"},
		Ports:       []fleet.ServicePort{{Name: "http", Port: 9731}},
		Expose: fleet.ServiceExpose{
			App:  &fleet.ServiceApp{Name: name, RequireLogin: true},
			Mesh: []fleet.ServiceMesh{{Name: name + "-mesh"}},
		},
		Enabled: enabled,
	}
}

// quiet keeps a passing run readable: the reconciler logs every transition,
// which is what an operator wants and a test does not.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestReconciler(src UnitSource, apps AppExposer, msh MeshExposer, internal *service.Internal) *Reconciler {
	return New(Deps{Source: src, Apps: apps, Mesh: msh, Internal: internal,
		Secrets: fixedSecrets{"k"}, Log: quiet()})
}

func TestActiveUnitIsPublishedOnTunnelAndMesh(t *testing.T) {
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{
		units:    []fleet.Service{webUnit("gateway", true)},
		statuses: []service.Status{{Unit: "gateway", State: service.Active}},
	}
	r := newTestReconciler(src, apps, msh, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := apps.names(); len(got) != 1 || got[0] != "gateway" {
		t.Fatalf("tunnel apps = %v, want [gateway]", got)
	}
	if got := msh.names(); len(got) != 1 || got[0] != "gateway-mesh" {
		t.Fatalf("mesh services = %v, want [gateway-mesh]", got)
	}
	if got := apps.live["gateway"]; got != "http://127.0.0.1:9731" {
		t.Fatalf("app target = %q, want the declared loopback port", got)
	}
	if !apps.meta["gateway"].RequireLogin {
		t.Fatal("require_login from the declaration was dropped")
	}
}

// The behaviour the retired poller never had: a crash-looping unit the
// monitor reports as failed loses its tunnel route and its mesh name.
func TestFailedUnitIsWithdrawnFromTunnelAndMesh(t *testing.T) {
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{
		units:    []fleet.Service{webUnit("gateway", true)},
		statuses: []service.Status{{Unit: "gateway", State: service.Active}},
	}
	r := newTestReconciler(src, apps, msh, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(apps.names()) != 1 || len(msh.names()) != 1 {
		t.Fatalf("precondition: unit not published")
	}

	src.set([]fleet.Service{webUnit("gateway", true)},
		[]service.Status{{Unit: "gateway", State: service.Failed, Reason: "start limit hit"}})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after failure: %v", err)
	}
	if got := apps.names(); len(got) != 0 {
		t.Fatalf("failed unit still on the tunnel: %v", got)
	}
	if got := msh.names(); len(got) != 0 {
		t.Fatalf("failed unit still on the mesh: %v", got)
	}
	snap := r.Snapshot()
	if len(snap.Units) != 1 || snap.Units[0].State != service.Failed {
		t.Fatalf("snapshot = %+v, want one failed row", snap.Units)
	}
	if len(snap.Units[0].Publishing) != 0 {
		t.Fatalf("snapshot still claims exposure: %v", snap.Units[0].Publishing)
	}
}

// Degraded is still serving, so it stays published: withdrawing a unit whose
// health probe blipped would be worse than the blip.
func TestDegradedUnitStaysPublished(t *testing.T) {
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{
		units:    []fleet.Service{webUnit("gateway", true)},
		statuses: []service.Status{{Unit: "gateway", State: service.Degraded}},
	}
	r := newTestReconciler(src, apps, msh, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(apps.names()) != 1 || len(msh.names()) != 1 {
		t.Fatalf("degraded unit was withdrawn: apps=%v mesh=%v", apps.names(), msh.names())
	}
}

// A disabled unit is withdrawn even while the monitor is unreachable: that
// fact comes from the locally-read declaration.
func TestDisabledUnitIsWithdrawnEvenWithoutTheMonitor(t *testing.T) {
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{
		units:    []fleet.Service{webUnit("gateway", true)},
		statuses: []service.Status{{Unit: "gateway", State: service.Active}},
	}
	r := newTestReconciler(src, apps, msh, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	src.mu.Lock()
	src.statErr = errors.New("monitor socket closed")
	src.mu.Unlock()
	src.set([]fleet.Service{webUnit("gateway", false)}, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after disable: %v", err)
	}
	if got := apps.names(); len(got) != 0 {
		t.Fatalf("disabled unit still on the tunnel: %v", got)
	}
	if got := msh.names(); len(got) != 0 {
		t.Fatalf("disabled unit still on the mesh: %v", got)
	}
}

// An unreachable monitor must not tear down a healthy host. The exposure
// survives and the condition is reported, which is what makes "kill bashy
// supervisord, outpost restarts it, units are re-adopted" non-disruptive.
func TestUnreachableMonitorKeepsEnabledUnitsPublished(t *testing.T) {
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{units: []fleet.Service{webUnit("gateway", true)}, statErr: errors.New("no monitor")}
	r := newTestReconciler(src, apps, msh, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := apps.names(); len(got) != 1 {
		t.Fatalf("enabled unit was withdrawn while the monitor was down: %v", got)
	}
	snap := r.Snapshot()
	if snap.MonitorReachable {
		t.Fatal("snapshot claims the monitor is reachable")
	}
	if snap.Units[0].State != service.Degraded {
		t.Fatalf("state with no monitor = %q, want degraded", snap.Units[0].State)
	}

	// Monitor comes back and re-adopts the unit.
	src.mu.Lock()
	src.statErr = nil
	src.mu.Unlock()
	src.set([]fleet.Service{webUnit("gateway", true)},
		[]service.Status{{Unit: "gateway", State: service.Active, PID: 4242}})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after monitor restart: %v", err)
	}
	snap = r.Snapshot()
	if !snap.MonitorReachable || snap.Units[0].State != service.Active || snap.Units[0].PID != 4242 {
		t.Fatalf("re-adopted row = %+v, want active pid 4242", snap.Units[0])
	}
	if got := apps.names(); len(got) != 1 {
		t.Fatalf("re-adopted unit lost its route: %v", got)
	}
}

// Repeated passes must not churn the registry: the route is registered once.
func TestReconcileIsIdempotent(t *testing.T) {
	apps := &countingApps{fakeApps: newFakeApps()}
	src := &fakeSource{
		units:    []fleet.Service{webUnit("gateway", true)},
		statuses: []service.Status{{Unit: "gateway", State: service.Active}},
	}
	r := newTestReconciler(src, apps, newFakeMesh(), nil)
	for range 4 {
		if err := r.ReloadAndReconcile(context.Background()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if apps.registers != 1 {
		t.Fatalf("RegisterApp called %d times, want 1", apps.registers)
	}
}

type countingApps struct {
	*fakeApps
	registers int
}

func (c *countingApps) RegisterApp(name, target string, meta AppMeta) error {
	c.registers++
	return c.fakeApps.RegisterApp(name, target, meta)
}

// A unit deleted from the registry keeps nothing published.
func TestRemovedUnitIsWithdrawn(t *testing.T) {
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{
		units:    []fleet.Service{webUnit("gateway", true)},
		statuses: []service.Status{{Unit: "gateway", State: service.Active}},
	}
	r := newTestReconciler(src, apps, msh, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	src.set(nil, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after removal: %v", err)
	}
	if len(apps.names()) != 0 || len(msh.names()) != 0 {
		t.Fatalf("removed unit still published: apps=%v mesh=%v", apps.names(), msh.names())
	}
}

func TestRunReloadsDeclarationsAddedOutsideOutpost(t *testing.T) {
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{units: []fleet.Service{webUnit("existing", true)},
		statuses: []service.Status{{Unit: "existing", State: service.Active}}}
	r := New(Deps{Source: src, Apps: apps, Mesh: msh, Secrets: fixedSecrets{"k"},
		Interval: 10 * time.Millisecond, Log: quiet()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("run: %v", err)
		}
	}()

	deadline := time.After(time.Second)
	for len(apps.names()) == 0 {
		select {
		case <-deadline:
			t.Fatal("initial unit was not published")
		case <-time.After(10 * time.Millisecond):
		}
	}
	src.set([]fleet.Service{webUnit("existing", true), webUnit("fresh-custom", true)},
		[]service.Status{{Unit: "existing", State: service.Active}, {Unit: "fresh-custom", State: service.Active}})
	deadline = time.After(time.Second)
	for {
		if len(apps.names()) == 2 && len(msh.names()) == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("new unit was not published by periodic reload: apps=%v mesh=%v", apps.names(), msh.names())
		case <-time.After(10 * time.Millisecond):
		}
	}
	src.set([]fleet.Service{webUnit("existing", true)},
		[]service.Status{{Unit: "existing", State: service.Active}})
	deadline = time.After(time.Second)
	for {
		if len(apps.names()) == 1 && len(msh.names()) == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("removed unit was not withdrawn by periodic reload: apps=%v mesh=%v", apps.names(), msh.names())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// trust_cloud_identity needs an SSO secret; a declaration that asks for the
// trusted-header contract with no key must be refused, not published with a
// blank signature.
func TestTrustCloudIdentityWithoutSecretIsRefused(t *testing.T) {
	u := webUnit("console", true)
	u.Expose.App.TrustCloudIdentity = true
	if _, err := Plan(u, service.Active, fixedSecrets{""}); err == nil {
		t.Fatal("expected a refusal for trust_cloud_identity with an empty secret")
	}
	ex, err := Plan(u, service.Active, fixedSecrets{"hmac-key"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if ex.App.Meta.SSOSecret != "hmac-key" || !ex.App.Meta.TrustCloudIdentity {
		t.Fatalf("app meta = %+v, want the minted key and the trusted-identity flag", ex.App.Meta)
	}
}

// An exposure naming a listener the unit does not declare is an operator
// error in the YAML: loud, and never published.
func TestExposureNamingAnUndeclaredPortIsRefused(t *testing.T) {
	u := webUnit("gateway", true)
	u.Expose.App.Port = "admin"
	if _, err := Plan(u, service.Active, fixedSecrets{"k"}); err == nil {
		t.Fatal("expected a refusal for an app exposure on an undeclared listener")
	}
}

// Elevation and the named-listener selection both come from the declaration.
func TestExposureReadsElevationAndNamedListener(t *testing.T) {
	u := webUnit("console", true)
	u.Ports = append(u.Ports, fleet.ServicePort{Name: "admin", Port: 9999})
	u.Expose.App.Port = "admin"
	u.Expose.App.Elevation = true
	u.Expose.Mesh = []fleet.ServiceMesh{{Name: "console-mesh", Port: "http"}}
	ex, err := Plan(u, service.Active, fixedSecrets{"k"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if ex.App.Target != "http://127.0.0.1:9999" {
		t.Fatalf("app target = %q, want the named admin listener", ex.App.Target)
	}
	if !ex.App.Meta.ElevationRequired {
		t.Fatal("elevation from the declaration was dropped")
	}
	if len(ex.Mesh) != 1 || ex.Mesh[0].Addr != "127.0.0.1:9731" {
		t.Fatalf("mesh exposure = %+v, want the named http listener", ex.Mesh)
	}
}

// ---- internal provider ----

type recordingCap struct {
	mu                               sync.Mutex
	activated, configured, deactived int
	lastCfg                          service.Config
	activateErr                      error
	healthErr                        error
}

func (c *recordingCap) Activate(_ context.Context, cfg service.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activated++
	c.lastCfg = cfg
	return c.activateErr
}

func (c *recordingCap) Configure(_ context.Context, cfg service.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configured++
	c.lastCfg = cfg
	return nil
}

func (c *recordingCap) Deactivate(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deactived++
	return nil
}

func (c *recordingCap) Health(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.healthErr
}

func internalUnit(name, capID string, enabled bool) fleet.Service {
	return fleet.Service{
		Name:       name,
		Kind:       fleet.KindService,
		Provider:   fleet.ServiceProviderInternal,
		Capability: capID,
		Owner:      fleet.ServiceOwnerOutpost,
		Ports:      []fleet.ServicePort{{Name: "http", Port: 5000}},
		Settings:   map[string]string{"mode": "a"},
		Expose: fleet.ServiceExpose{
			App:  &fleet.ServiceApp{Name: name},
			Mesh: []fleet.ServiceMesh{{Name: "registry"}},
		},
		Enabled: enabled,
	}
}

// An internal capability travels exactly the same path as a process unit:
// same declaration, same desired state, same exposure — and no pid.
func TestInternalCapabilityUsesTheSameMachinery(t *testing.T) {
	cap := &recordingCap{}
	internal, err := Hosted(map[string]service.Capability{"outpost.registry": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{units: []fleet.Service{internalUnit("registry", "outpost.registry", true)}}
	r := newTestReconciler(src, apps, msh, internal)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cap.activated != 1 {
		t.Fatalf("capability activated %d times, want 1", cap.activated)
	}
	if cap.lastCfg.Settings["mode"] != "a" {
		t.Fatalf("declaration settings did not reach the capability: %+v", cap.lastCfg)
	}
	if got := apps.names(); len(got) != 1 || got[0] != "registry" {
		t.Fatalf("internal unit not exposed on the tunnel: %v", got)
	}
	if got := msh.names(); len(got) != 1 || got[0] != "registry" {
		t.Fatalf("internal unit not exposed on the mesh: %v", got)
	}
	snap := r.Snapshot()
	if snap.Units[0].State != service.Active {
		t.Fatalf("internal unit state = %q, want active", snap.Units[0].State)
	}
	if snap.Units[0].PID != 0 {
		t.Fatalf("internal unit reported pid %d; an in-process capability has none", snap.Units[0].PID)
	}

	// Disabling it deactivates the capability and withdraws the exposure —
	// there is no separate feature flag anywhere.
	src.set([]fleet.Service{internalUnit("registry", "outpost.registry", false)}, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after disable: %v", err)
	}
	if cap.deactived != 1 {
		t.Fatalf("capability deactivated %d times, want 1", cap.deactived)
	}
	if len(apps.names()) != 0 || len(msh.names()) != 0 {
		t.Fatalf("disabled internal unit still published: apps=%v mesh=%v", apps.names(), msh.names())
	}
}

// A changed declaration reconfigures a live capability; an unchanged one does
// not re-push on every pass.
func TestInternalCapabilityReconfiguresOnlyOnChange(t *testing.T) {
	cap := &recordingCap{}
	internal, err := Hosted(map[string]service.Capability{"outpost.registry": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	src := &fakeSource{units: []fleet.Service{internalUnit("registry", "outpost.registry", true)}}
	r := newTestReconciler(src, newFakeApps(), newFakeMesh(), internal)
	for range 3 {
		if err := r.ReloadAndReconcile(context.Background()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if cap.configured != 0 {
		t.Fatalf("Configure called %d times with no declaration change", cap.configured)
	}
	changed := internalUnit("registry", "outpost.registry", true)
	changed.Settings = map[string]string{"mode": "b"}
	src.set([]fleet.Service{changed}, nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after change: %v", err)
	}
	if cap.configured != 1 {
		t.Fatalf("Configure called %d times after a change, want 1", cap.configured)
	}
	if cap.lastCfg.Settings["mode"] != "b" {
		t.Fatalf("new settings did not reach the capability: %+v", cap.lastCfg)
	}
}

// A capability that refuses to activate ends failed and publishes nothing.
func TestInternalActivationFailureWithdrawsExposure(t *testing.T) {
	cap := &recordingCap{activateErr: fmt.Errorf("port already held")}
	internal, err := Hosted(map[string]service.Capability{"outpost.registry": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	apps := newFakeApps()
	src := &fakeSource{units: []fleet.Service{internalUnit("registry", "outpost.registry", true)}}
	r := newTestReconciler(src, apps, newFakeMesh(), internal)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(apps.names()) != 0 {
		t.Fatalf("a capability that failed to activate was still exposed: %v", apps.names())
	}
	if st := r.Snapshot().Units[0]; st.State != service.Failed {
		t.Fatalf("state = %q, want failed", st.State)
	}
}

// A unit whose capability is hosted by another process is left alone here.
func TestInternalUnitOwnedElsewhereIsNotActivatedHere(t *testing.T) {
	cap := &recordingCap{}
	internal, err := Hosted(map[string]service.Capability{"meet.events": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	u := internalUnit("events", "meet.events", true)
	u.Owner = "meet"
	u.Expose = fleet.ServiceExpose{}
	src := &fakeSource{units: []fleet.Service{u}}
	r := newTestReconciler(src, newFakeApps(), newFakeMesh(), internal)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cap.activated != 0 {
		t.Fatal("outpost activated a capability owned by another process")
	}
}

// Kick is what makes a toggle apply live. It must never block.
func TestKickIsNonBlockingAndCoalesces(t *testing.T) {
	r := newTestReconciler(&fakeSource{}, newFakeApps(), newFakeMesh(), nil)
	for range 100 {
		r.Kick()
	}
}

// ---- monitor: the one thing outpost cannot answer locally ----

// The monitor client must read the exact status document the Bashy S16 CLI
// emits. A fleet-list envelope must fail instead of silently hiding units.
func TestMonitorReadsStatuses(t *testing.T) {
	var argv []string
	var body = `{"schema_version":"bashy-service-status-v1","monitor":{"running":true},"units":[{"unit":"gateway","provider":"process","enabled":true,"state":"active","pid":91}]}`
	m := &Monitor{Run: func(_ context.Context, args ...string) ([]byte, error) {
		argv = args
		return []byte(body), nil
	}}
	sts, err := m.Statuses(context.Background())
	if err != nil {
		t.Fatalf("statuses: %v", err)
	}
	if fmt.Sprint(argv) != fmt.Sprint([]string{"service", "status", "--json"}) {
		t.Fatalf("argv = %v", argv)
	}
	if len(sts) != 1 || sts[0].State != service.Active || sts[0].PID != 91 {
		t.Fatalf("statuses = %+v", sts)
	}
	if _, err := decodeStatusUnits([]byte(`{"items":[]}`)); err == nil {
		t.Fatal("fleet list envelope was silently accepted as service status")
	}
}

func TestMonitorWithoutARunnerIsAnError(t *testing.T) {
	var m *Monitor
	if _, err := m.Statuses(context.Background()); err == nil {
		t.Fatal("expected an error with no monitor configured")
	}
}

func unitNames(us []fleet.Service) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.Name)
	}
	return out
}

// A host with no mesh forwarder (mesh off, or unpaired) must not report a
// mesh name as published. Claiming one tells an operator a peer can reach a
// service that no peer can reach.
func TestExposureOnlyClaimsSurfacesTheHostHas(t *testing.T) {
	apps := newFakeApps()
	src := &fakeSource{
		units:    []fleet.Service{webUnit("gateway", true)},
		statuses: []service.Status{{Unit: "gateway", State: service.Active}},
	}
	// Apps wired, mesh nil.
	r := New(Deps{Source: src, Apps: apps, Secrets: fixedSecrets{"k"}, Log: quiet()})
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	snap := r.Snapshot()
	if len(snap.Units) != 1 {
		t.Fatalf("snapshot = %+v", snap.Units)
	}
	if got := snap.Units[0].Publishing; len(got) != 1 || got[0] != "app:gateway" {
		t.Fatalf("publishing = %v, want only the tunnel route", got)
	}

	// Symmetrically: mesh wired, no app registry.
	msh := newFakeMesh()
	r2 := New(Deps{Source: src, Mesh: msh, Secrets: fixedSecrets{"k"}, Log: quiet()})
	if err := r2.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := r2.Snapshot().Units[0].Publishing; len(got) != 1 || got[0] != "mesh:gateway-mesh" {
		t.Fatalf("publishing = %v, want only the mesh name", got)
	}
}

// Reporting must not send an operator to the wrong log: a disabled unit is
// inactive because the declaration says so, not because the monitor is down.
func TestDisabledUnitReasonDoesNotBlameTheMonitor(t *testing.T) {
	src := &fakeSource{units: []fleet.Service{webUnit("gateway", false)}, statErr: errors.New("no monitor")}
	r := newTestReconciler(src, newFakeApps(), newFakeMesh(), nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row := r.Snapshot().Units[0]
	if row.State != service.Inactive {
		t.Fatalf("state = %q, want inactive", row.State)
	}
	if row.Reason != "unit disabled" {
		t.Fatalf("reason = %q, want the declaration's reason", row.Reason)
	}
}

// A unit outpost has no work for — no exposure, no internal capability — is
// still the operator's business. The monitor described it, so pass its row
// through instead of leaving a blank state next to the tracked units.
func TestSnapshotPassesThroughUnitsOutpostDoesNotPublish(t *testing.T) {
	plain := fleet.Service{Name: "cron", Kind: fleet.KindService, Exec: []string{"d", "run"}, Enabled: true}
	src := &fakeSource{
		// Only the exposing unit is tracked; the monitor reports both.
		units: []fleet.Service{webUnit("gateway", true)},
		statuses: []service.Status{
			{Unit: "gateway", State: service.Active},
			{Unit: "cron", Provider: "process", Enabled: true, State: service.Failed, Reason: "start limit"},
		},
	}
	_ = plain
	r := newTestReconciler(src, newFakeApps(), newFakeMesh(), nil)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	snap := r.Snapshot()
	if len(snap.Units) != 2 {
		t.Fatalf("snapshot = %+v, want both units", snap.Units)
	}
	// Name-sorted: cron then gateway.
	if snap.Units[0].Unit != "cron" || snap.Units[0].State != service.Failed {
		t.Fatalf("untracked row = %+v", snap.Units[0])
	}
	if len(snap.Units[0].Publishing) != 0 {
		t.Fatalf("an untracked unit claims exposure: %v", snap.Units[0].Publishing)
	}
}

// An enabled unit whose capability keeps refusing is re-activated on every
// pass — that is how it recovers once the operator fixes the reason. The
// WARNING, though, is logged once per distinct reason: at a 5 s cadence an
// identical line forever would bury the state it reports. A changed reason
// is logged again.
func TestRepeatedActivationFailureIsLoggedOncePerReason(t *testing.T) {
	cap := &recordingCap{activateErr: fmt.Errorf("no registration token")}
	internal, err := Hosted(map[string]service.Capability{"outpost.registry": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	var buf bytes.Buffer
	src := &fakeSource{units: []fleet.Service{internalUnit("registry", "outpost.registry", true)}}
	r := New(Deps{Source: src, Internal: internal, Secrets: fixedSecrets{"k"},
		Log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))})

	for range 3 {
		if err := r.ReloadAndReconcile(context.Background()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if cap.activated != 3 {
		t.Fatalf("capability activated %d times, want one attempt per pass", cap.activated)
	}
	if got := strings.Count(buf.String(), "activate failed"); got != 1 {
		t.Fatalf("logged the same refusal %d times, want 1:\n%s", got, buf.String())
	}

	// A different reason is news.
	cap.mu.Lock()
	cap.activateErr = fmt.Errorf("forge unreachable")
	cap.mu.Unlock()
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := strings.Count(buf.String(), "activate failed"); got != 2 {
		t.Fatalf("a changed refusal was swallowed:\n%s", buf.String())
	}
}

// An in-process capability has no child for the monitor to watch, so when
// its own Health says it stopped serving, the reconciler is the only thing
// that can notice. It cycles the unit — and a capability that then refuses
// is reported failed and publishes nothing, instead of sitting at "active"
// forever with nothing behind it.
func TestUnhealthyInternalCapabilityIsCycled(t *testing.T) {
	cap := &recordingCap{}
	internal, err := Hosted(map[string]service.Capability{"outpost.registry": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	apps := newFakeApps()
	src := &fakeSource{units: []fleet.Service{internalUnit("registry", "outpost.registry", true)}}
	r := newTestReconciler(src, apps, newFakeMesh(), internal)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cap.activated != 1 || len(apps.names()) != 1 {
		t.Fatalf("not published after a healthy activate: activated=%d apps=%v", cap.activated, apps.names())
	}

	// It stops serving and cannot be brought back.
	cap.mu.Lock()
	cap.healthErr = fmt.Errorf("daemon exited")
	cap.activateErr = fmt.Errorf("no registration token")
	cap.mu.Unlock()
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after it died: %v", err)
	}
	if cap.deactived != 1 || cap.activated != 2 {
		t.Fatalf("unhealthy capability was not cycled: deactivated=%d activated=%d", cap.deactived, cap.activated)
	}
	if st := r.Snapshot().Units[0]; st.State != service.Failed {
		t.Fatalf("state = %q, want failed", st.State)
	}
	if len(apps.names()) != 0 {
		t.Fatalf("a dead capability kept its route: %v", apps.names())
	}
}
