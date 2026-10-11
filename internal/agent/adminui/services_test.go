package adminui

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

// fakeServiceOps is a unit registry + monitor stand-in: enough to drive the
// HTTP surface without a bashy binary or a real registry on disk.
type fakeServiceOps struct {
	units   []fleet.Service
	enabled map[string]bool
	fields  map[string]map[string]string
	kicks   int
	snap    svcunit.Snapshot
}

func newFakeServiceOps(names ...string) *fakeServiceOps {
	f := &fakeServiceOps{enabled: map[string]bool{}, fields: map[string]map[string]string{}}
	for _, n := range names {
		f.units = append(f.units, fleet.Service{
			Name: n, Kind: fleet.KindService, Description: n + " unit",
			Exec:   []string{n, "serve"},
			Ports:  []fleet.ServicePort{{Name: "http", Port: 22749}},
			Expose: fleet.ServiceExpose{App: &fleet.ServiceApp{Name: n, RequireLogin: true}},
		})
	}
	f.snap = svcunit.Snapshot{MonitorReachable: true}
	return f
}

func (f *fakeServiceOps) All() ([]fleet.Service, error) {
	out := append([]fleet.Service(nil), f.units...)
	for i := range out {
		out[i].Enabled = f.enabled[out[i].Name]
	}
	return out, nil
}

func (f *fakeServiceOps) Unit(name string) (fleet.Service, bool) {
	units, _ := f.All()
	for _, u := range units {
		if u.Name == name {
			return u, true
		}
	}
	return fleet.Service{}, false
}

func (f *fakeServiceOps) SetEnabled(name string, on bool) error {
	if _, ok := f.Unit(name); !ok {
		return &svcunit.UnknownUnitError{Name: name, Known: f.names()}
	}
	f.enabled[name] = on
	return nil
}

func (f *fakeServiceOps) SetFields(_ context.Context, name string, fields map[string]string) error {
	if _, ok := f.Unit(name); !ok {
		return &svcunit.UnknownUnitError{Name: name, Known: f.names()}
	}
	if f.fields[name] == nil {
		f.fields[name] = map[string]string{}
	}
	for k, v := range fields {
		f.fields[name][k] = v
	}
	return nil
}

func (f *fakeServiceOps) Snapshot() svcunit.Snapshot { return f.snap }
func (f *fakeServiceOps) Kick()                      { f.kicks++ }

func (f *fakeServiceOps) names() []string {
	out := make([]string, 0, len(f.units))
	for _, u := range f.units {
		out = append(out, u.Name)
	}
	return out
}

// GET /api/services is the console's service picture: declarations joined
// with the monitor's structured state. It is what replaced grepping a
// service's text output, and the never-wired apphealth prober.
func TestServicesEndpointShowsDeclarationsAndMonitorState(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "h"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}
	s := newTestServer(t, configPath, map[string]string{user: "secret"}, nil)
	svc := newFakeServiceOps("console", "rooms")
	svc.enabled["console"] = true
	svc.snap = svcunit.Snapshot{MonitorReachable: true, Units: []svcunit.UnitState{{
		Status:     service.Status{Unit: "console", Provider: "process", State: service.Active, PID: 4242},
		Publishing: []string{"app:console"},
	}}}
	s.core.SetServices(svc)

	w := doJSON(s, http.MethodPost, "/api/login", map[string]string{"user": user, "password": "secret"}, "")
	cookie := extractCookie(w, cookieName)
	if cookie == "" {
		t.Fatal("missing session cookie after login")
	}
	w = doJSON(s, http.MethodGet, "/api/services", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/services = %d %s", w.Code, w.Body.String())
	}
	var view struct {
		MonitorReachable bool `json:"monitor_reachable"`
		Units            []struct {
			Name       string   `json:"name"`
			Enabled    bool     `json:"enabled"`
			State      string   `json:"state"`
			PID        int      `json:"pid"`
			App        string   `json:"app"`
			Publishing []string `json:"publishing"`
		} `json:"units"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if !view.MonitorReachable || len(view.Units) != 2 {
		t.Fatalf("view = %+v, want two units and a reachable monitor", view)
	}
	// Name-sorted: console then rooms.
	if view.Units[0].Name != "console" || !view.Units[0].Enabled {
		t.Fatalf("console row = %+v", view.Units[0])
	}
	if view.Units[0].State != "active" || view.Units[0].PID != 4242 {
		t.Fatalf("monitor state did not reach the console: %+v", view.Units[0])
	}
	if len(view.Units[0].Publishing) != 1 || view.Units[0].Publishing[0] != "app:console" {
		t.Fatalf("published routes missing: %+v", view.Units[0])
	}
	// A declared-but-disabled unit is still listed, with no runtime state:
	// the console must be able to offer it.
	if view.Units[1].Name != "rooms" || view.Units[1].Enabled || view.Units[1].State != "" {
		t.Fatalf("rooms row = %+v", view.Units[1])
	}
	if view.Units[1].App != "rooms" {
		t.Fatalf("declared app route lost: %+v", view.Units[1])
	}
}

// Naming a port is not asking for a service.
//
// This is the loom hazard, made structural. The old shape had a `loom_port`
// toggle beside a `loom` toggle under an "absent means on" rule, so setting
// only the port silently started a git forge and published it on the mesh as
// `git`. A declaration override and a desired-state write are now different
// fields: nothing in service_settings can flip `enabled`.
func TestServiceSettingsAloneNeverEnablesAUnit(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "h"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}
	s := newTestServer(t, configPath, map[string]string{user: "secret"}, nil)
	svc := newFakeServiceOps("loom")
	s.core.SetServices(svc)
	w := doJSON(s, http.MethodPost, "/api/login", map[string]string{"user": user, "password": "secret"}, "")
	cookie := extractCookie(w, cookieName)

	w = doJSON(s, http.MethodPost, "/api/config/builtins", map[string]any{
		"service_settings": map[string]any{"loom": map[string]any{"ports.0.port": "31900"}},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("override a declaration field: %d %s", w.Code, w.Body.String())
	}
	if svc.enabled["loom"] {
		t.Fatal("overriding a field enabled the unit")
	}
	if svc.fields["loom"]["ports.0.port"] != "31900" {
		t.Fatalf("the override was not applied: %+v", svc.fields)
	}

	// And an explicit false stays false even alongside an override.
	w = doJSON(s, http.MethodPost, "/api/config/builtins", map[string]any{
		"services":         map[string]any{"loom": false},
		"service_settings": map[string]any{"loom": map[string]any{"ports.0.port": "31901"}},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("explicit off with an override: %d %s", w.Code, w.Body.String())
	}
	if svc.enabled["loom"] {
		t.Fatal("an explicit off was overridden by an adjacent setting")
	}

	// Only the desired-state field turns it on.
	w = doJSON(s, http.MethodPost, "/api/config/builtins",
		map[string]any{"services": map[string]any{"loom": true}}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if !svc.enabled["loom"] {
		t.Fatal("an explicit on did not enable the unit")
	}
}

// With no registry wired, the service operations say so rather than panicking.
func TestServicesEndpointWithoutARegistry(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{AgentName: "h"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	user, err := hostauth.CurrentUser()
	if err != nil || user == "" {
		t.Skip("cannot determine OS user")
	}
	s := newTestServer(t, configPath, map[string]string{user: "secret"}, nil)
	w := doJSON(s, http.MethodPost, "/api/login", map[string]string{"user": user, "password": "secret"}, "")
	cookie := extractCookie(w, cookieName)

	w = doJSON(s, http.MethodGet, "/api/services", nil, cookie)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/services with no registry = %d, want 503", w.Code)
	}
	w = doJSON(s, http.MethodPost, "/api/config/builtins",
		map[string]any{"services": map[string]any{"x": true}}, cookie)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("toggle with no registry = %d, want 503", w.Code)
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "registry") {
		t.Fatalf("the error does not explain itself: %s", w.Body.String())
	}
}
