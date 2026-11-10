package conf

import (
	"path/filepath"
	"testing"
)

// The attachment overlay serves the daemon in memory and is never persisted;
// a legacy top-level pairing wins over it.
func TestApplyAttachmentTunnel(t *testing.T) {
	att := SatelliteAttachment{SatelliteID: "sat_1", Endpoint: "https://sat.example.com", Token: "stok",
		Tunnel: &AttachmentTunnel{AgentName: "studio", ServerAddr: "sat.example.com", ServerPort: 443, Protocol: "wss", Token: "m", RemotePort: 7001}}
	fc := &FileConfig{Attachments: []SatelliteAttachment{att}}
	if !fc.ApplyAttachmentTunnel() {
		t.Fatal("overlay not applied")
	}
	if fc.AgentName != "studio" || fc.ServerAddr != "sat.example.com" || fc.RemotePort != 7001 || fc.AccessToken != "stok" || fc.PortalURL != att.Endpoint {
		t.Fatalf("overlay = %+v", fc)
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	back, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.LegacyPaired() || back.ServerAddr != "" || len(back.Attachments) != 1 {
		t.Fatalf("overlay persisted: %+v", back)
	}

	legacy := &FileConfig{AgentName: "old", AccessToken: "a", ServerAddr: "old.example.com", Attachments: []SatelliteAttachment{att}}
	if legacy.ApplyAttachmentTunnel() || legacy.ServerAddr != "old.example.com" {
		t.Fatalf("overlay replaced a legacy pairing: %+v", legacy)
	}
	if (&FileConfig{Attachments: []SatelliteAttachment{{SatelliteID: "x", Token: "t"}}}).ApplyAttachmentTunnel() {
		t.Fatal("overlay applied without a registration")
	}
}
