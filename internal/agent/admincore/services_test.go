package admincore

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent/svcunit"
)

// stubServiceOps is a unit registry + monitor stand-in for admincore's own
// tests: no bashy binary, no registry on disk.
type stubServiceOps struct {
	units   []string
	enabled map[string]bool
	fields  map[string]map[string]string
	kicks   int
	snap    svcunit.Snapshot
	setErr  error
}

func (s *stubServiceOps) All() ([]fleet.Service, error) {
	out := make([]fleet.Service, 0, len(s.units))
	for _, n := range s.units {
		out = append(out, fleet.Service{
			Name: n, Kind: fleet.KindService, Description: n + " unit",
			Exec:    []string{n, "serve"},
			Ports:   []fleet.ServicePort{{Name: "http", Port: 9000}},
			Expose:  fleet.ServiceExpose{App: &fleet.ServiceApp{Name: n, RequireLogin: true}},
			Enabled: s.enabled[n],
		})
	}
	return out, nil
}

func (s *stubServiceOps) Unit(name string) (fleet.Service, bool) {
	units, _ := s.All()
	for _, u := range units {
		if u.Name == name {
			return u, true
		}
	}
	return fleet.Service{}, false
}

func (s *stubServiceOps) SetEnabled(name string, on bool) error {
	if s.setErr != nil {
		return s.setErr
	}
	if _, ok := s.Unit(name); !ok {
		return &svcunit.UnknownUnitError{Name: name, Known: s.units}
	}
	s.enabled[name] = on
	return nil
}

func (s *stubServiceOps) SetFields(_ context.Context, name string, fields map[string]string) error {
	if _, ok := s.Unit(name); !ok {
		return &svcunit.UnknownUnitError{Name: name, Known: s.units}
	}
	if s.fields[name] == nil {
		s.fields[name] = map[string]string{}
	}
	for k, v := range fields {
		s.fields[name][k] = v
	}
	return nil
}

func (s *stubServiceOps) Snapshot() svcunit.Snapshot { return s.snap }
func (s *stubServiceOps) Kick()                      { s.kicks++ }

func newStubOps(units ...string) *stubServiceOps {
	return &stubServiceOps{units: units, enabled: map[string]bool{},
		fields: map[string]map[string]string{}, snap: svcunit.Snapshot{MonitorReachable: true}}
}

// A service toggle applies live: it is written to the unit store, the
// reconciler is nudged, and no restart is reported. The old surface returned
// RestartPending while the CLI printed a restart message for a change that
// scheduled nothing.
func TestServiceToggleAppliesLiveWithoutARestart(t *testing.T) {
	core, _ := newTestCore(t)
	ops := newStubOps("console")
	core.SetServices(ops)

	res, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Services: map[string]bool{"console": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.ServicesApplied {
		t.Fatalf("result = %+v, want ok + services_applied", res)
	}
	if res.RestartPending {
		t.Fatal("a service toggle reported a restart it did not schedule")
	}
	if !ops.enabled["console"] {
		t.Fatalf("desired state was not written: %+v", ops.enabled)
	}
	if ops.kicks != 1 {
		t.Fatalf("reconciler nudged %d times, want 1", ops.kicks)
	}
}

// A boot-only toggle in the SAME call still schedules the restart it needs.
func TestServiceToggleBesideABootOnlyToggleStillRestarts(t *testing.T) {
	core, _ := newTestCore(t)
	core.SetServices(newStubOps("console"))
	on := true
	res, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Services: map[string]bool{"console": true},
		Shell:    &on,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.RestartPending {
		t.Fatal("a boot-only toggle alongside a service toggle lost its restart")
	}
	if !res.ServicesApplied {
		t.Fatal("the service half was not reported as applied")
	}
}

// Overrides are applied BEFORE desired state in one call, so a unit switched
// on in the same breath comes up with the declaration just given to it.
func TestServiceSettingsAreAppliedBeforeDesiredState(t *testing.T) {
	core, _ := newTestCore(t)
	ops := &orderedOps{stubServiceOps: newStubOps("loom")}
	core.SetServices(ops)
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Services:        map[string]bool{"loom": true},
		ServiceSettings: map[string]map[string]string{"loom": {"ports.0.port": "31900"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ops.order, ","); got != "fields,enable" {
		t.Fatalf("order = %q, want fields then enable", got)
	}
}

type orderedOps struct {
	*stubServiceOps
	order []string
}

func (o *orderedOps) SetEnabled(name string, on bool) error {
	o.order = append(o.order, "enable")
	return o.stubServiceOps.SetEnabled(name, on)
}

func (o *orderedOps) SetFields(ctx context.Context, name string, f map[string]string) error {
	o.order = append(o.order, "fields")
	return o.stubServiceOps.SetFields(ctx, name, f)
}

// An unknown unit is a 400 naming the declared ones, and nothing is written.
func TestServiceToggleOnAnUnknownUnitIsABadRequest(t *testing.T) {
	core, _ := newTestCore(t)
	ops := newStubOps("console")
	core.SetServices(ops)
	_, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Services: map[string]bool{"consul": true},
	})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	ae := AsAPIError(err)
	if ae == nil || ae.Status != 400 {
		t.Fatalf("error = %v, want a 400 APIError", err)
	}
	if !strings.Contains(ae.Msg, "console") {
		t.Fatalf("the error does not name the declared units: %s", ae.Msg)
	}
	if len(ops.enabled) != 0 {
		t.Fatalf("something was written despite the refusal: %+v", ops.enabled)
	}
}

// Without a registry wired, the service path reports unavailable rather than
// panicking — the posture every other optional dependency takes here.
func TestServiceToggleWithoutARegistry(t *testing.T) {
	core, _ := newTestCore(t)
	_, err := core.SetBuiltins(context.Background(), BuiltinsParams{
		Services: map[string]bool{"console": true},
	})
	ae := AsAPIError(err)
	if ae == nil || ae.Status != 503 {
		t.Fatalf("error = %v, want a 503 APIError", err)
	}
	if _, err := core.ServiceStatus(); AsAPIError(err) == nil {
		t.Fatalf("ServiceStatus with no registry = %v, want an APIError", err)
	}
}

// ServiceStatus joins the declaration with the monitor's state, and says out
// loud when the monitor did not answer instead of presenting stale rows as
// live truth.
func TestServiceStatusJoinsDeclarationAndMonitorState(t *testing.T) {
	core, _ := newTestCore(t)
	ops := newStubOps("console", "rooms")
	ops.enabled["console"] = true
	ops.snap = svcunit.Snapshot{MonitorReachable: false, Units: []svcunit.UnitState{{
		Status:     service.Status{Unit: "console", State: service.Degraded, Reason: "service monitor unreachable"},
		Publishing: []string{"app:console"},
	}}}
	core.SetServices(ops)

	view, err := core.ServiceStatus()
	if err != nil {
		t.Fatal(err)
	}
	if view.MonitorReachable {
		t.Fatal("view claims the monitor is reachable")
	}
	if len(view.Units) != 2 || view.Units[0].Name != "console" {
		t.Fatalf("units = %+v", view.Units)
	}
	u := view.Units[0]
	if !u.Enabled || u.State != string(service.Degraded) || u.Reason == "" {
		t.Fatalf("console row = %+v", u)
	}
	if u.App != "console" || len(u.Ports) != 1 || u.Ports[0] != "loopback:9000" {
		t.Fatalf("declaration fields lost: %+v", u)
	}
	if len(u.Publishing) != 1 || u.Publishing[0] != "app:console" {
		t.Fatalf("published routes lost: %+v", u)
	}
	// A unit the monitor never mentioned still appears, with no state.
	if view.Units[1].Name != "rooms" || view.Units[1].State != "" {
		t.Fatalf("rooms row = %+v", view.Units[1])
	}
}

// SafeView carries the declarations so the console can render them without a
// second call, and never a secret.
func TestSafeViewCarriesServiceDeclarations(t *testing.T) {
	core, _ := newTestCore(t)
	ops := newStubOps("console")
	ops.enabled["console"] = true
	core.SetServices(ops)
	v, err := core.SafeView()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Services) != 1 || v.Services[0].Name != "console" || !v.Services[0].Enabled {
		t.Fatalf("SafeView.services = %+v", v.Services)
	}
}
