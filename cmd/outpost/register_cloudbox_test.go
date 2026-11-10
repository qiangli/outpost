package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// `outpost register --code` against the cloudbox master (§3 of yoke
// docs/cloudbox-members-attach-wire.md) stores the identity in the
// `cloudbox` block and leaves the tunnel pairing and attachments alone.
func TestDoPairWithMasterStoresIdentityOnly(t *testing.T) {
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/hosts/pair" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"host":{"host":"studio","owner_email":"owner@example.com","paired_at":"2026-11-09T18:10:00Z"},"token":"htok","scopes":["host"],"tunnel":false}`))
	}))
	defer master.Close()

	path := writeConfig(t, &conf.FileConfig{
		AgentName: "studio", ServerAddr: "sat.example.com", AccessToken: "legacy", MCPBearerToken: "mcp",
		Attachments: []conf.SatelliteAttachment{{SatelliteID: "sat_1", Endpoint: "https://sat.example.com", Token: "stok"}},
	})
	id, err := doPair(context.Background(), master.URL, "K7QX-2MPD", "studio", "", "", path, false, "")
	if err != nil || id == nil {
		t.Fatalf("doPair: id=%v err=%v", id, err)
	}
	fc, _ := conf.LoadFile(path)
	if !fc.CloudboxPaired() || fc.Cloudbox.Host != "studio" || fc.Cloudbox.OwnerEmail != "owner@example.com" || fc.Cloudbox.PortalURL != master.URL {
		t.Errorf("cloudbox = %+v", fc.Cloudbox)
	}
	if fc.ServerAddr != "sat.example.com" || fc.AccessToken != "legacy" || fc.MCPBearerToken != "mcp" || len(fc.Attachments) != 1 {
		t.Errorf("identity pairing clobbered other config: %+v", fc)
	}
}
