package svcunit

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// fakeMonitor speaks the owner half of bashy supervisord's control socket
// ({"op":"desired"} / {"op":"report"}, one JSON request and response per
// connection) so the reconciler is driven over a real unix socket by the
// real service.Client.
type fakeMonitor struct {
	path string

	mu       sync.Mutex
	ln       net.Listener
	want     map[string]bool
	reports  map[string]service.Status
	reported int
}

func newFakeMonitor(t *testing.T) *fakeMonitor {
	t.Helper()
	// unix socket paths are short-limited; t.TempDir() can blow the limit.
	dir, err := os.MkdirTemp("", "mon")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	m := &fakeMonitor{path: filepath.Join(dir, "s.sock"), want: map[string]bool{}, reports: map[string]service.Status{}}
	m.start(t)
	t.Cleanup(m.stop)
	return m
}

func (m *fakeMonitor) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("unix", m.path)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.ln = ln
	m.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
}

func (m *fakeMonitor) stop() {
	m.mu.Lock()
	ln := m.ln
	m.ln = nil
	m.mu.Unlock()
	if ln != nil {
		ln.Close()
		os.Remove(m.path)
	}
}

// restart models the monitor process restarting: everything it was told is lost.
func (m *fakeMonitor) restart(t *testing.T) {
	m.stop()
	m.mu.Lock()
	m.reports = map[string]service.Status{}
	m.mu.Unlock()
	m.start(t)
}

func (m *fakeMonitor) serve(c net.Conn) {
	defer c.Close()
	var req struct {
		Op     string         `json:"op"`
		Owner  string         `json:"owner"`
		Status service.Status `json:"status"`
	}
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	resp := map[string]any{"ok": true}
	switch req.Op {
	case "desired":
		var units []service.Status
		for name, on := range m.want {
			units = append(units, service.Status{Unit: name, Enabled: on})
		}
		resp["units"] = units
	case "report":
		m.reports[req.Status.Unit] = req.Status
		m.reported++
	default:
		resp = map[string]any{"ok": false, "error": "unknown op"}
	}
	_ = json.NewEncoder(c).Encode(resp)
}

func (m *fakeMonitor) setWant(unit string, on bool) {
	m.mu.Lock()
	m.want[unit] = on
	m.mu.Unlock()
}

func (m *fakeMonitor) report(unit string) (service.Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.reports[unit]
	return st, ok
}

func ownerReconciler(t *testing.T, mon *fakeMonitor, cap service.Capability, enabled bool) (*Reconciler, *fakeSource, *fakeApps, *fakeMesh) {
	t.Helper()
	internal, err := Hosted(map[string]service.Capability{"outpost.registry": cap})
	if err != nil {
		t.Fatal(err)
	}
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{units: []fleet.Service{internalUnit("registry", "outpost.registry", enabled)}}
	r := New(Deps{Source: src, Apps: apps, Mesh: msh, Internal: internal, Secrets: fixedSecrets{"k"},
		Owner: service.Client{Socket: mon.path, Timeout: time.Second}, Log: quiet()})
	return r, src, apps, msh
}

// The monitor's desired state, not a local flag, decides whether an internal
// capability runs; the owner reports what it actually did, and a restarted
// monitor is re-informed on the next pass.
func TestInternalUnitFollowsMonitorDesiredStateAndReports(t *testing.T) {
	mon := newFakeMonitor(t)
	cap := &recordingCap{}
	r, _, apps, msh := ownerReconciler(t, mon, cap, true)
	ctx := context.Background()

	// Declared enabled, but the monitor does not want it (e.g. a dependency
	// is down): it must stay off.
	mon.setWant("registry", false)
	if err := r.ReloadAndReconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if cap.activated != 0 {
		t.Fatalf("activated %d times against the monitor's wishes", cap.activated)
	}
	if st, ok := mon.report("registry"); !ok || st.State != service.Inactive || st.Enabled {
		t.Fatalf("report = %+v ok=%v, want inactive/not enabled", st, ok)
	}
	if len(apps.names()) != 0 {
		t.Fatalf("unwanted unit published: %v", apps.names())
	}

	// The monitor wants it: activate, publish, report active with health.
	mon.setWant("registry", true)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if cap.activated != 1 {
		t.Fatalf("activated %d times, want 1", cap.activated)
	}
	st, _ := mon.report("registry")
	if st.State != service.Active || !st.Enabled || st.Health != "ok" || st.PID != 0 {
		t.Fatalf("report = %+v, want active/enabled/health ok/no pid", st)
	}
	if len(apps.names()) != 1 || len(msh.names()) != 1 {
		t.Fatalf("not published: apps=%v mesh=%v", apps.names(), msh.names())
	}

	// A steady pass does not re-activate, but keeps reporting.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if cap.activated != 1 {
		t.Fatalf("steady pass re-activated: %d", cap.activated)
	}

	// Monitor restarts and forgets everything: the next pass re-adopts.
	mon.restart(t)
	mon.setWant("registry", true)
	if _, ok := mon.report("registry"); ok {
		t.Fatal("restarted monitor should have no reports yet")
	}
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, ok := mon.report("registry"); !ok || st.State != service.Active {
		t.Fatalf("not re-adopted after monitor restart: %+v ok=%v", st, ok)
	}
	if cap.activated != 1 {
		t.Fatalf("re-adoption must not bounce the capability: activated %d", cap.activated)
	}

	// The monitor withdraws its wish: deactivate, withdraw, report inactive.
	mon.setWant("registry", false)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if cap.deactived != 1 {
		t.Fatalf("deactivated %d times, want 1", cap.deactived)
	}
	if st, _ := mon.report("registry"); st.State != service.Inactive || st.Enabled {
		t.Fatalf("report after disable = %+v", st)
	}
	if len(apps.names()) != 0 || len(msh.names()) != 0 {
		t.Fatalf("still published: apps=%v mesh=%v", apps.names(), msh.names())
	}
}

// With no monitor answering, an enabled declaration still runs (an older
// bashy must not cost the host its in-process features), and the unit is
// adopted the moment a monitor appears.
func TestInternalUnitFallsBackToDeclarationWithoutMonitor(t *testing.T) {
	mon := newFakeMonitor(t)
	mon.stop()
	cap := &recordingCap{}
	r, _, apps, _ := ownerReconciler(t, mon, cap, true)
	ctx := context.Background()

	if err := r.ReloadAndReconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if cap.activated != 1 || len(apps.names()) != 1 {
		t.Fatalf("unmanaged host lost the capability: activated=%d apps=%v", cap.activated, apps.names())
	}

	mon.start(t)
	mon.setWant("registry", true)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, ok := mon.report("registry"); !ok || st.State != service.Active {
		t.Fatalf("not adopted when the monitor appeared: %+v ok=%v", st, ok)
	}
	if cap.activated != 1 {
		t.Fatalf("adoption bounced the capability: %d", cap.activated)
	}
}

// A capability that refuses is reported failed with its reason, and its
// exposure is withdrawn.
func TestInternalActivationFailureIsReportedToMonitor(t *testing.T) {
	mon := newFakeMonitor(t)
	cap := &recordingCap{activateErr: context.DeadlineExceeded}
	r, _, apps, _ := ownerReconciler(t, mon, cap, true)
	mon.setWant("registry", true)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, ok := mon.report("registry")
	if !ok || st.State != service.Failed || st.Reason == "" {
		t.Fatalf("report = %+v ok=%v, want failed with a reason", st, ok)
	}
	if len(apps.names()) != 0 {
		t.Fatalf("failed unit published: %v", apps.names())
	}
}

// A capability the daemon never registered is reported failed with the
// reason, rather than as a silent inactive.
func TestUnregisteredCapabilityIsReportedFailed(t *testing.T) {
	mon := newFakeMonitor(t)
	r, src, _, _ := ownerReconciler(t, mon, &recordingCap{}, true)
	src.set([]fleet.Service{internalUnit("ghost", "outpost.nope", true)}, nil)
	mon.setWant("ghost", true)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, ok := mon.report("ghost")
	if !ok || st.State != service.Failed || st.Reason == "" {
		t.Fatalf("report = %+v ok=%v, want failed with reason", st, ok)
	}
}

// A local disable takes effect before a slow monitor has re-read the
// declaration.
func TestLocalDisableBeatsStaleMonitorDesire(t *testing.T) {
	mon := newFakeMonitor(t)
	cap := &recordingCap{}
	r, src, apps, _ := ownerReconciler(t, mon, cap, true)
	mon.setWant("registry", true)
	ctx := context.Background()
	if err := r.ReloadAndReconcile(ctx); err != nil {
		t.Fatal(err)
	}
	src.set([]fleet.Service{internalUnit("registry", "outpost.registry", false)}, nil)
	if err := r.ReloadAndReconcile(ctx); err != nil { // monitor still says wanted
		t.Fatal(err)
	}
	if cap.deactived != 1 || len(apps.names()) != 0 {
		t.Fatalf("disable not applied: deactivated=%d apps=%v", cap.deactived, apps.names())
	}
}

// Shutting the daemon down deactivates what it applied and tells the monitor.
func TestShutdownReportsInternalUnitsInactive(t *testing.T) {
	mon := newFakeMonitor(t)
	cap := &recordingCap{}
	r, _, _, _ := ownerReconciler(t, mon, cap, true)
	mon.setWant("registry", true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := mon.report("registry"); ok && st.State == service.Active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if st, _ := mon.report("registry"); st.State != service.Inactive {
		t.Fatalf("final report = %+v, want inactive", st)
	}
	if cap.deactived == 0 {
		t.Fatal("capability not deactivated on shutdown")
	}
}
