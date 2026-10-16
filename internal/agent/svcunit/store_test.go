package svcunit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// testStore pins a Store to a scratch registry root so a test never reads or
// writes the developer's real unit store.
func testStore(t *testing.T, seed ...fleet.Service) *Store {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "services")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pin the catalog to a known world: the real embedded seeds are what a
	// host sees, but a test that asserts on the whole set must not move
	// whenever S17 adds one.
	cat := fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
	for _, u := range seed {
		if err := cat.SaveService(u); err != nil {
			t.Fatalf("seed %s: %v", u.Name, err)
		}
	}
	return &Store{Catalog: cat}
}

// The declaration round-trips through the same store `bashy service` uses.
func TestStoreReadsDeclarations(t *testing.T) {
	st := testStore(t, webUnit("gateway", true), internalUnit("registry", "outpost.registry", false))
	units, err := st.Units(context.Background())
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("units = %v, want gateway + registry", unitNames(units))
	}
}

// A unit with neither exposure nor an internal capability is not outpost's
// business — it is the monitor's — so outpost does not carry it.
func TestStoreSkipsUnitsOutpostHasNoWorkFor(t *testing.T) {
	plain := fleet.Service{
		Name: "cron", Kind: fleet.KindService,
		Exec: []string{"some-daemon", "run"}, Enabled: true,
	}
	st := testStore(t, plain, webUnit("gateway", true))
	units, err := st.Units(context.Background())
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	if len(units) != 1 || units[0].Name != "gateway" {
		t.Fatalf("units = %v, want only gateway", unitNames(units))
	}
}

// A toggle is a desired-state write through the unit store — the same write
// `bashy service enable` performs — and nothing else.
func TestSetEnabledWritesDesiredState(t *testing.T) {
	st := testStore(t, webUnit("gateway", false))
	if err := st.SetEnabled("gateway", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	u, ok := st.Unit("gateway")
	if !ok || !u.Enabled {
		t.Fatalf("unit after enable = %+v, want enabled", u)
	}
	// Idempotent: enabling an enabled unit is not an error.
	if err := st.SetEnabled("gateway", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if err := st.SetEnabled("gateway", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if u, _ := st.Unit("gateway"); u.Enabled {
		t.Fatal("unit still enabled after disable")
	}
}

// Toggling a unit nobody declared names what IS declared, so a typo is
// answerable without a second command.
func TestSetEnabledOnAnUndeclaredUnitNamesTheAlternatives(t *testing.T) {
	st := testStore(t, webUnit("gateway", true))
	err := st.SetEnabled("gatway", true)
	if err == nil {
		t.Fatal("expected an error for an undeclared unit")
	}
	var unknown *UnknownUnitError
	if !asUnknown(err, &unknown) {
		t.Fatalf("error = %v, want *UnknownUnitError", err)
	}
	if len(unknown.Known) == 0 {
		t.Fatal("error does not name the declared units")
	}
}

// The port-conflict rule is fleet's, and a toggle must answer to it: two
// units may declare the same listener as alternatives, but only one of them
// can be enabled.
func TestEnablingAUnitThatWouldStealAHeldPortIsRefused(t *testing.T) {
	first := webUnit("gateway", true)
	second := webUnit("other-gateway", false)
	second.Ports = first.Ports
	st := testStore(t, first, second)
	err := st.SetEnabled("other-gateway", true)
	if err == nil {
		t.Fatal("expected a port-conflict refusal")
	}
	if u, _ := st.Unit("other-gateway"); u.Enabled {
		t.Fatal("the refused toggle was persisted anyway")
	}
}

// End to end through the real store: a fresh custom declaration an operator
// just wrote is enabled with the same verb a built-in uses, and the
// reconciler publishes it with no code that knows its name.
func TestFreshCustomDeclarationIsEnabledAndExposedLikeABuiltIn(t *testing.T) {
	st := testStore(t)
	custom := fleet.Service{
		Name: "openclaw", Kind: fleet.KindService,
		Description: "a third-party gateway added by YAML alone",
		Exec:        []string{"openclaw", "serve", "--port", "{port}"},
		Ports:       []fleet.ServicePort{{Name: "http", Port: 7788}},
		Expose: fleet.ServiceExpose{
			App:  &fleet.ServiceApp{Name: "openclaw", RequireLogin: true},
			Mesh: []fleet.ServiceMesh{{Name: "openclaw"}},
		},
		Enabled: false,
	}
	if err := st.Save(custom); err != nil {
		t.Fatalf("add: %v", err)
	}
	apps, msh := newFakeApps(), newFakeMesh()
	st.Monitor = statusStub{{Unit: "openclaw", State: service.Active}}
	r := New(Deps{Source: st, Apps: apps, Mesh: msh, Secrets: fixedSecrets{"k"}, Log: quiet()})

	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile while disabled: %v", err)
	}
	if len(apps.names()) != 0 {
		t.Fatalf("a disabled unit was published: %v", apps.names())
	}

	if err := st.SetEnabled("openclaw", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after enable: %v", err)
	}
	if got := apps.names(); len(got) != 1 || got[0] != "openclaw" {
		t.Fatalf("tunnel apps = %v, want [openclaw]", got)
	}
	if got := msh.names(); len(got) != 1 || got[0] != "openclaw" {
		t.Fatalf("mesh services = %v, want [openclaw]", got)
	}
}

// An operator override of a seeded built-in is the same write, and the new
// port is what outpost proxies to.
func TestOperatorOverrideOfASeededUnitChangesTheExposure(t *testing.T) {
	st := testStore(t, webUnit("gateway", true))
	apps := newFakeApps()
	st.Monitor = statusStub{{Unit: "gateway", State: service.Active}}
	r := New(Deps{Source: st, Apps: apps, Secrets: fixedSecrets{"k"}, Log: quiet()})
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := apps.live["gateway"]; got != "http://127.0.0.1:9731" {
		t.Fatalf("target before the override = %q", got)
	}
	over, _ := st.Unit("gateway")
	over.Ports[0].Port = 9900
	if err := st.Save(over); err != nil {
		t.Fatalf("override: %v", err)
	}
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after override: %v", err)
	}
	if got := apps.live["gateway"]; got != "http://127.0.0.1:9900" {
		t.Fatalf("target after the override = %q, want the overridden port", got)
	}
}

// statusStub is a Monitor stand-in that always reports the same rows.
type statusStub []service.Status

func (s statusStub) Statuses(context.Context) ([]service.Status, error) {
	return []service.Status(s), nil
}

func asUnknown(err error, target **UnknownUnitError) bool {
	for err != nil {
		if u, ok := err.(*UnknownUnitError); ok {
			*target = u
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// The rod-not-fish acceptance, on the real seeded ring: a compiled-in
// built-in (meet), an internal outpost capability and a fresh third-party
// declaration are each configured and enabled through the same verbs, and
// each ends up published by the same reconciler. No branch in this package
// names any of them.
func TestSeededBuiltinInternalAndCustomShareOneMachinery(t *testing.T) {
	root := t.TempDir()
	// The real embedded seeds, an isolated local store.
	cat := fleet.New(fleet.WithRoot(root), fleet.WithoutCloudOverlay())
	st := &Store{Catalog: cat}

	seeded, ok := st.Unit("meet")
	if !ok {
		t.Fatal("the meet built-in is not seeded; S17 owns the embedded units")
	}
	if seeded.Ring.String() != "embedded" {
		t.Fatalf("meet came from ring %q, want the compiled-in seed", seeded.Ring)
	}
	defaultOn := map[string]bool{
		"ollama": true, "ollama-pool": true, "shard": true, "apps": true,
		"meet": true, "otel": true, "otel-stack": true, "warm": true, "files": true, "podman": true,
	}
	seededAll, err := st.Units(context.Background())
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	if len(seededAll) == 0 {
		t.Fatal("no seeded units")
	}
	for _, u := range seededAll {
		if u.Enabled != defaultOn[u.Name] {
			t.Errorf("seeded unit %q default Enabled=%v, want %v (golden: default-on %v; all others opt-in)",
				u.Name, u.Enabled, defaultOn[u.Name], defaultOn)
		}
	}
	for name := range defaultOn {
		if _, ok := st.Unit(name); !ok {
			t.Errorf("golden default-on unit %q is not seeded", name)
		}
	}

	internal := fleet.Service{
		Name: "files", Kind: fleet.KindService,
		Provider: fleet.ServiceProviderInternal, Capability: "outpost.files",
		Owner: fleet.ServiceOwnerOutpost,
		Ports: []fleet.ServicePort{{Name: "http", Port: 28080}},
		Expose: fleet.ServiceExpose{
			App: &fleet.ServiceApp{Name: "files", RequireLogin: true},
		},
	}
	custom := fleet.Service{
		Name: "codync-host", Kind: fleet.KindService,
		Exec:  []string{"codync-host", "serve", "--port", "{port}"},
		Ports: []fleet.ServicePort{{Name: "http", Port: 7799}},
		Expose: fleet.ServiceExpose{
			Mesh: []fleet.ServiceMesh{{Name: "codync"}},
		},
	}
	for _, u := range []fleet.Service{internal, custom} {
		if err := st.Save(u); err != nil {
			t.Fatalf("add %s: %v", u.Name, err)
		}
	}

	cap := &recordingCap{}
	prov, err := Hosted(map[string]service.Capability{"outpost.files": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	apps, msh := newFakeApps(), newFakeMesh()
	st.Monitor = statusStub{
		{Unit: "meet", State: service.Active},
		{Unit: "codync-host", State: service.Active},
	}
	r := New(Deps{Source: st, Apps: apps, Mesh: msh, Internal: prov,
		Secrets: fixedSecrets{"k"}, Log: quiet()})

	// One verb for all three.
	for _, name := range []string{"meet", "files", "codync-host"} {
		if err := st.SetEnabled(name, true); err != nil {
			t.Fatalf("enable %s: %v", name, err)
		}
	}
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// meet (a seeded process unit) and files (an internal capability) each
	// declare an app route; codync-host declares only a mesh name.
	if got := apps.names(); len(got) != 2 || got[0] != "files" || got[1] != "meet" {
		t.Fatalf("tunnel apps = %v, want [files meet]", got)
	}
	if got := msh.names(); len(got) != 1 || got[0] != "codync" {
		t.Fatalf("mesh services = %v, want [codync]", got)
	}
	if cap.activated != 1 {
		t.Fatalf("the internal capability was activated %d times, want 1", cap.activated)
	}

	// And one verb takes all three away again.
	for _, name := range []string{"meet", "files", "codync-host"} {
		if err := st.SetEnabled(name, false); err != nil {
			t.Fatalf("disable %s: %v", name, err)
		}
	}
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after disable: %v", err)
	}
	if len(apps.names()) != 0 || len(msh.names()) != 0 {
		t.Fatalf("still published after disable: apps=%v mesh=%v", apps.names(), msh.names())
	}
	if cap.deactived != 1 {
		t.Fatalf("the internal capability was deactivated %d times, want 1", cap.deactived)
	}
}
