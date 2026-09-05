package admincore

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// loom turns on only when someone asks for it — never as a side effect of an
// adjacent setting.
//
// The other builtins use "absent means on", which is right when you are
// configuring a service because you want it. For loom that shape meant setting
// loom_port alone — naming a port, not asking for a forge — silently started a
// git server and published it on the mesh as the `git` service. loom serves
// p2p/sphere work; a single-host workflow is served by `bashy weave` and needs
// no forge at all.
func TestLoomIsExplicitOptInOnly(t *testing.T) {
	port := 31880
	on, off := true, false

	for _, tc := range []struct {
		name string
		p    BuiltinsParams
		want bool
	}{
		{"port alone does not enable it", BuiltinsParams{LoomPort: &port}, false},
		{"explicit true enables it", BuiltinsParams{Loom: &on}, true},
		{"explicit false wins over a port", BuiltinsParams{Loom: &off, LoomPort: &port}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, cfgPath := newTestCore(t)
			if _, err := core.SetBuiltins(tc.p); err != nil {
				t.Fatal(err)
			}
			fc, err := conf.LoadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			var found, enabled bool
			for _, s := range fc.BashyServices {
				if s.Name == "loom" {
					found, enabled = true, s.Enabled
				}
			}
			if tc.want && !enabled {
				t.Errorf("loom enabled = %v (entry present %v), want enabled", enabled, found)
			}
			if !tc.want && enabled {
				t.Errorf("loom was enabled without an explicit opt-in")
			}
		})
	}
}
