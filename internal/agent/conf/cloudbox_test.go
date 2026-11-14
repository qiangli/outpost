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

func TestTwoAttachmentsSurviveOverlayAndRestart(t *testing.T) {
	fc := &FileConfig{Attachments: []SatelliteAttachment{
		{SatelliteID: "one", Tunnel: &AttachmentTunnel{AgentName: "host", ServerAddr: "one.example"}},
		{SatelliteID: "two", Tunnel: &AttachmentTunnel{AgentName: "host", ServerAddr: "two.example"}},
	}}
	if !fc.ApplyAttachmentTunnel() {
		t.Fatal("overlay failed")
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	back, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.LegacyPaired() || len(back.Attachments) != 2 || back.Attachments[0].Tunnel.ServerAddr != "one.example" || back.Attachments[1].Tunnel.ServerAddr != "two.example" {
		t.Fatalf("restart config = %+v", back)
	}
}

// LoadClientFile reads a pair-once host with attachments as paired (the
// overlay a client command needs), and never persists that overlay.
func TestLoadClientFileAppliesAttachmentOverlay(t *testing.T) {
	att := SatelliteAttachment{SatelliteID: "sat_1", Endpoint: "https://sat.example.com", Token: "stok",
		Tunnel: &AttachmentTunnel{AgentName: "studio", ServerAddr: "sat.example.com", ServerPort: 443, Protocol: "wss", Token: "m", RemotePort: 7001}}
	path := t.TempDir() + "/agent.json"
	if err := SaveFile(path, &FileConfig{Attachments: []SatelliteAttachment{att}}); err != nil {
		t.Fatal(err)
	}
	plain, err := LoadFile(path)
	if err != nil || plain == nil || plain.LegacyPaired() || plain.ServerAddr != "" {
		t.Fatalf("LoadFile must stay overlay-free: %+v %v", plain, err)
	}
	fc, err := LoadClientFile(path)
	if err != nil || fc == nil {
		t.Fatalf("LoadClientFile: %v", err)
	}
	if fc.ServerAddr != "sat.example.com" || fc.AgentName != "studio" || fc.AccessToken != "stok" || !fc.AttachmentTunnelActive() {
		t.Fatalf("overlay not applied: %+v", fc)
	}
	if err := SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	back, err := LoadFile(path)
	if err != nil || back.LegacyPaired() || back.ServerAddr != "" {
		t.Fatalf("overlay leaked into the file: %+v %v", back, err)
	}
	if missing, err := LoadClientFile(t.TempDir() + "/none.json"); err != nil || missing != nil {
		t.Fatalf("missing file: %v %v", missing, err)
	}
}
