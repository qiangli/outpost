package svcunit

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// A capability that serves with a known limitation (DegradedError from
// Health) reports degraded with the reason — and is NOT cycled: tearing
// the listener down and rebinding every pass would flap a working
// capability for a condition a restart cannot fix. Exposure stays
// published (degraded is an exposed state).
func TestDegradedHealthReportsDegradedWithoutCycling(t *testing.T) {
	cap := &recordingCap{healthErr: NewDegraded("firewall: access denied")}
	internal, err := Hosted(map[string]service.Capability{"outpost.sshd": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	apps, msh := newFakeApps(), newFakeMesh()
	src := &fakeSource{units: []fleet.Service{internalUnit("sshd", "outpost.sshd", true)}}
	r := newTestReconciler(src, apps, msh, internal)

	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := r.ReloadAndReconcile(context.Background()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	st := r.Snapshot().Units[0]
	if st.State != service.Degraded {
		t.Fatalf("state = %q, want degraded", st.State)
	}
	if !strings.Contains(st.Reason, "firewall") {
		t.Fatalf("reason = %q, want the firewall reason", st.Reason)
	}
	if cap.activated != 1 {
		t.Errorf("capability activated %d times, want 1 (no cycling while degraded)", cap.activated)
	}
	if cap.deactived != 0 {
		t.Errorf("capability deactivated %d times, want 0", cap.deactived)
	}
	if got := apps.names(); len(got) != 1 {
		t.Errorf("degraded unit withdrawn from the tunnel: %v", got)
	}
	if got := msh.names(); len(got) != 1 {
		t.Errorf("degraded unit withdrawn from the mesh: %v", got)
	}

	// Healing clears the condition with no restart: the next pass reports
	// active again.
	cap.mu.Lock()
	cap.healthErr = nil
	cap.mu.Unlock()
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after heal: %v", err)
	}
	if st := r.Snapshot().Units[0]; st.State != service.Active {
		t.Fatalf("state after heal = %q, want active", st.State)
	}
	if cap.activated != 1 {
		t.Errorf("capability activated %d times after heal, want still 1", cap.activated)
	}
}

// A DegradedError from Activate maps to degraded rather than failed — the
// unit never served, but the reason is a limitation, not a refusal.
func TestDegradedActivateReportsDegraded(t *testing.T) {
	cap := &recordingCap{activateErr: NewDegraded("firewall: access denied")}
	internal, err := Hosted(map[string]service.Capability{"outpost.sshd": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	src := &fakeSource{units: []fleet.Service{internalUnit("sshd", "outpost.sshd", true)}}
	r := newTestReconciler(src, newFakeApps(), newFakeMesh(), internal)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	st := r.Snapshot().Units[0]
	if st.State != service.Degraded {
		t.Fatalf("state = %q, want degraded", st.State)
	}
	if !strings.Contains(st.Reason, "firewall") {
		t.Fatalf("reason = %q, want the firewall reason", st.Reason)
	}
}

// A plain (non-degraded) health failure still cycles the capability —
// the degraded exception must not swallow real liveness failures.
func TestPlainHealthFailureStillCycles(t *testing.T) {
	cap := &recordingCap{healthErr: fmt.Errorf("listener stopped serving")}
	internal, err := Hosted(map[string]service.Capability{"outpost.registry": cap})
	if err != nil {
		t.Fatalf("hosted: %v", err)
	}
	src := &fakeSource{units: []fleet.Service{internalUnit("registry", "outpost.registry", true)}}
	r := newTestReconciler(src, newFakeApps(), newFakeMesh(), internal)
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := r.ReloadAndReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if cap.activated != 2 {
		t.Errorf("capability activated %d times, want 2 (cycled on health failure)", cap.activated)
	}
	if cap.deactived != 1 {
		t.Errorf("capability deactivated %d times, want 1", cap.deactived)
	}
}
