package admincore

import (
	"context"
	"testing"
)

// Naming a port is not asking for a service.
//
// loom used to be a `loom` toggle beside a `loom_port` toggle under the
// shared "absent means on" rule, so setting ONLY loom_port — naming a port,
// not asking for a forge — silently started a git server and published it on
// the mesh as the `git` service. The toggle therefore carried a one-off
// explicit-opt-in exception that every reader had to know about.
//
// The hazard is structural now rather than special-cased: a declaration
// override (service_settings) and a desired-state write (services) are
// different fields, and nothing in the former can reach `enabled`. The
// exception is gone because the shape that needed it is gone.
func TestServiceSettingsCannotEnableAUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    BuiltinsParams
		want bool
	}{
		{"a field override alone does not enable it",
			BuiltinsParams{ServiceSettings: map[string]map[string]string{"loom": {"ports.0.port": "31880"}}}, false},
		{"an explicit true enables it",
			BuiltinsParams{Services: map[string]bool{"loom": true}}, true},
		{"an explicit false wins over an adjacent override",
			BuiltinsParams{
				Services:        map[string]bool{"loom": false},
				ServiceSettings: map[string]map[string]string{"loom": {"ports.0.port": "31880"}},
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, _ := newTestCore(t)
			ops := &stubServiceOps{units: []string{"loom"}, enabled: map[string]bool{}, fields: map[string]map[string]string{}}
			core.SetServices(ops)
			if _, err := core.SetBuiltins(context.Background(), tc.p); err != nil {
				t.Fatal(err)
			}
			if got := ops.enabled["loom"]; got != tc.want {
				t.Errorf("loom enabled = %v, want %v", got, tc.want)
			}
		})
	}
}
