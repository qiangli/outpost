package admincore

import (
	"testing"
)

// The default must be OFF and LOOPBACK. A host that shipped hosting a control
// plane, or listening on every interface, would be surprising in the one
// direction that matters.
func TestControlPlaneView_DefaultsOffAndLoopback(t *testing.T) {
	s := newTestServer(t)
	got, err := s.ControlPlaneView(false)
	if err != nil {
		t.Fatalf("ControlPlaneView: %v", err)
	}
	if got.ControlPlane {
		t.Error("control plane on by default")
	}
	if got.BindAddr != "127.0.0.1" || got.BindPort != 7000 {
		t.Errorf("default bind = %s:%d, want 127.0.0.1:7000", got.BindAddr, got.BindPort)
	}
	if got.LANExposed {
		t.Error("loopback bind reported as LAN-exposed")
	}
	if got.HasToken || got.TunnelToken != "" {
		t.Error("a host hosting nothing should have no join token")
	}
}

// Enabling must mint the token in the same operation, or the operator ends up
// with a control plane that is on but unjoinable and no obvious next step.
func TestSetControlPlane_EnableMintsToken(t *testing.T) {
	s := newTestServer(t)
	on := true
	got, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on})
	if err != nil {
		t.Fatalf("SetControlPlane: %v", err)
	}
	if !got.ControlPlane {
		t.Error("not enabled")
	}
	if !got.HasToken {
		t.Fatal("enabling did not mint a join token")
	}
	// The setter must not leak it — that is the reveal path's job.
	if got.TunnelToken != "" {
		t.Error("SetControlPlane returned the token; only the reveal path should")
	}

	revealed, err := s.ControlPlaneView(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(revealed.TunnelToken) < 32 {
		t.Errorf("revealed token too short: %q", revealed.TunnelToken)
	}
}

// Disabling must PRESERVE the token. Deleting it would invalidate every
// worker's configuration as a side effect of an off/on toggle, and the token
// is inert while nothing is listening anyway.
func TestSetControlPlane_DisablePreservesToken(t *testing.T) {
	s := newTestServer(t)
	on, off := true, false
	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ControlPlaneView(true)

	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	after, err := s.ControlPlaneView(true)
	if err != nil {
		t.Fatal(err)
	}
	if after.ControlPlane {
		t.Error("still enabled after disable")
	}
	if after.TunnelToken != before.TunnelToken {
		t.Error("disabling rotated the token — an off/on toggle would break every worker")
	}
}

// A nil field means "leave alone": flipping the switch must not reset a
// customized bind back to the default.
func TestSetControlPlane_PartialUpdatePreservesBind(t *testing.T) {
	s := newTestServer(t)
	addr, port := "0.0.0.0", 7100
	if _, err := s.SetControlPlane(ControlPlaneParams{BindAddr: &addr, BindPort: &port}); err != nil {
		t.Fatal(err)
	}
	on := true
	got, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if got.BindAddr != "0.0.0.0" || got.BindPort != 7100 {
		t.Errorf("bind reset to %s:%d by an unrelated update", got.BindAddr, got.BindPort)
	}
	if !got.LANExposed {
		t.Error("0.0.0.0 not reported as LAN-exposed — this is the flag's whole job")
	}
}

// A hostname is refused: an unresolvable name would take the listener down at
// boot, long after the operator left the config screen and with no obvious
// connection to what they changed.
func TestSetControlPlane_RejectsNonIPBind(t *testing.T) {
	s := newTestServer(t)
	bad := "control-plane.local"
	if _, err := s.SetControlPlane(ControlPlaneParams{BindAddr: &bad}); err == nil {
		t.Fatal("accepted a hostname as a bind address")
	}
	for _, p := range []int{-1, 70000} {
		port := p
		if _, err := s.SetControlPlane(ControlPlaneParams{BindPort: &port}); err == nil {
			t.Errorf("accepted out-of-range port %d", p)
		}
	}
}

// Rotation must actually change the value and hand it back — a rotate that
// returned the old token would look successful while revoking nothing.
func TestRotateControlPlaneToken_ChangesAndReturns(t *testing.T) {
	s := newTestServer(t)
	on := true
	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ControlPlaneView(true)

	got, err := s.RotateControlPlaneToken()
	if err != nil {
		t.Fatalf("RotateControlPlaneToken: %v", err)
	}
	if got.TunnelToken == "" {
		t.Fatal("rotate did not return the new token")
	}
	if got.TunnelToken == before.TunnelToken {
		t.Fatal("rotate returned the same token — nothing was revoked")
	}

	persisted, err := s.ControlPlaneView(true)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TunnelToken != got.TunnelToken {
		t.Error("rotated token was not persisted")
	}
}

// Rotating on a host that never hosted a plane should still work — it is how
// an operator pre-seeds one before turning the switch on.
func TestRotateControlPlaneToken_WorksBeforeEnable(t *testing.T) {
	s := newTestServer(t)
	got, err := s.RotateControlPlaneToken()
	if err != nil {
		t.Fatalf("RotateControlPlaneToken: %v", err)
	}
	if got.TunnelToken == "" {
		t.Error("no token minted")
	}
	if got.ControlPlane {
		t.Error("rotate turned the control plane on as a side effect")
	}
}

// The SafeView row is the fourth surface. Without it `outpost status` and the
// admin UI cannot tell an operator that this host is a control plane — which
// matters because restarting one is a cluster-wide event, not a local one.
func TestSafeView_ReportsControlPlane(t *testing.T) {
	s := newTestServer(t)
	on := true
	addr := "0.0.0.0"
	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on, BindAddr: &addr}); err != nil {
		t.Fatal(err)
	}
	view, err := s.SafeView()
	if err != nil {
		t.Fatalf("SafeView: %v", err)
	}
	if !view.Cluster.ControlPlane {
		t.Error("SafeView does not report control-plane placement")
	}
	if !view.Cluster.HasTunnelToken {
		t.Error("SafeView does not report token presence")
	}
	if !view.Cluster.TunnelLANExposed {
		t.Error("SafeView does not report the LAN exposure")
	}
	// Presence, never the value — SafeView is rendered in the admin UI and
	// returned by `outpost status`.
	if view.Cluster.TunnelBindAddr != "0.0.0.0" {
		t.Errorf("bind addr = %q", view.Cluster.TunnelBindAddr)
	}
}
