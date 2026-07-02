package admincore

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestSetBuiltinsPersistsGenericBashyServices(t *testing.T) {
	core, cfgPath := newTestCore(t)
	services := []conf.BashyService{{
		Name:               "loom",
		Enabled:            true,
		AppName:            "loom",
		AppPort:            13100,
		RequireLogin:       true,
		TrustCloudIdentity: true,
		MeshService:        "git",
	}}
	res, err := core.SetBuiltins(BuiltinsParams{BashyServices: services})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.RestartPending {
		t.Fatalf("unexpected result: %+v", res)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.BashyServices) != 1 || fc.BashyServices[0].Name != "loom" || !fc.BashyServices[0].Enabled {
		t.Fatalf("bashy service not persisted: %+v", fc.BashyServices)
	}
	if !fc.BashyServices[0].RequireLogin || !fc.BashyServices[0].TrustCloudIdentity {
		t.Fatalf("bashy service auth flags not persisted: %+v", fc.BashyServices[0])
	}
}
