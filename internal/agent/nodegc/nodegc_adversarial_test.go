package nodegc

// Gate-authored adversarial tests (sprint 42, corrective for outpost#38).
// These probe the fleet-level guards from the attacker's side: can a
// healthy node still be deleted? The three D1-D3 tests were originally
// written to assert the DEFECTIVE behaviour and have been INVERTED to
// assert the corrected behaviour; the three regression pins (empty scope,
// single-node exemption, small-cluster boundary, recovery between list
// and delete) verify scenarios the implementation already handled
// correctly and must stay green.

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// D1 (was a defect, now fixed) — the circuit-breaker denominator must
// count only the ELIGIBLE population. Nodes that are structurally
// ineligible for deletion (this host's own nodes, control-plane-role
// nodes) can never be candidates, so counting them into scope would
// dilute the stale fraction and let a full-worker partition slip under
// the threshold.
//
// Realistic setup: the control-plane host itself joined an agent runtime
// to its own plane and rejoined three times (k3s --with-node-id mints a
// NEW node name per join), leaving three dead self-host ghosts that
// SelfHost excludes from candidacy. Then the whole real worker
// population (2 of 2) partitions.
//
// 100% of the deletable fleet is stale — the exact "observer is the
// suspect" state the breaker exists for. With the eligible denominator
// (scope = 2, not 5) the fraction is 2/2 = 1.0 > 0.5, so the breaker
// fires and BOTH surviving workers are spared.
func TestAdversarial_BreakerDenominatorIsEligiblePopulation(t *testing.T) {
	var objs []runtime.Object
	// Three dead ghosts belonging to the plane host itself. Excluded
	// from candidacy AND from the breaker denominator by SelfHost.
	for _, s := range []string{"a", "b", "c"} {
		name := "plane-" + s
		objs = append(objs, staleAgentNode(name, "plane", types.UID(name), 200*time.Hour))
	}
	// The entire real worker population, partitioned at one instant.
	for _, s := range []string{"x", "y"} {
		name := "pi-" + s
		objs = append(objs, staleAgentNode(name, "pi", types.UID(name), 48*time.Hour))
	}

	cs := fake.NewSimpleClientset(objs...)
	c := collector(t, cs)
	c.SelfHost = "plane"
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}

	// scope = 2 (only the eligible workers), stale = 2.
	// 2 > 0.5*2 == 1.0 is TRUE, so the breaker fires and deletes nothing.
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v; 2 of 2 eligible workers (100%%) were stale — a "+
			"full-worker partition. The breaker must refuse the whole pass; "+
			"ineligible self-host ghosts must not inflate the denominator.", got)
	}
	// Refusal is durably recorded.
	events := ledgerEvents(t, c.Ledger)
	if len(events) != 1 || events[0] != EventRefusedMassStale {
		t.Fatalf("ledger events = %v, want [%s]", events, EventRefusedMassStale)
	}
}

// D2 (was a defect, now fixed) — the clock-skew guard is no longer
// one-directional. A FORWARD wall-clock jump — the dangerous direction,
// which produces no future timestamps — is caught by cross-checking
// wall-clock progression against the collector's monotonic uptime
// anchor between passes.
//
// Without the guard, a node NotReady for only five minutes (a worker
// rebooting, a laptop that just closed its lid) reads as 48 h stale
// after a +48 h jump and is reaped. MinUptime does not help — it is
// monotonic uptime since collector start, already satisfied on a
// long-running daemon when the wall clock later jumps (restored VM
// snapshot, resumed VM, dual-boot RTC, delayed NTP step); CLOCK_MONOTONIC
// does not advance across suspend, so uptime survives the jump intact.
// The breaker does not help either: only ONE node is briefly down, and
// breakerMinStale == 2 exempts a single stale node.
//
// The scenario is a LONG-RUNNING collector whose clock steps mid-life,
// so one collector runs both passes: pass 1 establishes the wall/
// monotonic baseline with a correct clock, then the wall clock jumps
// while monotonic uptime (fixed by the test harness) does not.
func TestAdversarial_ForwardClockJumpRefusesPass(t *testing.T) {
	objs := readyFleet("pi", 9)
	// Down for five minutes — nowhere near the 24 h grace.
	objs = append(objs, node("pi-rebooting", "u-reboot",
		withLabels(agentLabels("pi")),
		withReady(corev1.ConditionFalse, tickNow.Add(-5*time.Minute))))
	cs := fake.NewSimpleClientset(objs...)

	c := collector(t, cs)

	// Pass 1: correct clock. pi-rebooting is only 5 min down — safe. This
	// pass records the wall/monotonic baseline.
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("baseline pass: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("baseline pass deleted %v, want none", got)
	}

	// Pass 2: the wall clock jumps forward two days; monotonic uptime is
	// untouched. pi-rebooting now reads as 48 h stale — but wall time
	// raced ahead of monotonic time, so the observer's clock is the
	// suspect and the pass must be refused.
	c.Now = func() time.Time { return tickNow.Add(48 * time.Hour) }
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("jumped pass: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("a +48h wall-clock jump deleted %v, want none — a node down "+
			"5 minutes must survive a forward clock step; the observer's clock "+
			"is the suspect, not the fleet", got)
	}
	// The refusal is durably recorded as a clock-skew refusal.
	events := ledgerEvents(t, c.Ledger)
	if len(events) == 0 || events[len(events)-1] != EventRefusedClockSkew {
		t.Fatalf("ledger events = %v, want a trailing %s", events, EventRefusedClockSkew)
	}
}

// D3 (was a defect, now fixed) — a nil Ledger no longer degrades the
// delete budget to process memory (the pre-#38 "fresh budget per
// restart" defect). main.go reaches a nil-ledger state whenever
// conf.ResolveCacheDir() fails; a REAL pass with no durable ledger must
// now fail closed and delete nothing, because process memory resets on
// every self-restart and is no rate limit at all.
//
// Three "restarts" inside ONE rate window, each a fresh Collector with
// no ledger — precisely what a toggle storm produces — must delete zero.
func TestAdversarial_NilLedgerFailsClosed(t *testing.T) {
	objs := readyFleet("pi", 6)
	for _, s := range []string{"s1", "s2", "s3", "s4", "s5"} {
		name := "pi-" + s
		objs = append(objs, staleAgentNode(name, "pi", types.UID(name), 100*time.Hour))
	}
	cs := fake.NewSimpleClientset(objs...)

	for i := 0; i < 3; i++ {
		c := collector(t, cs)
		c.Ledger = nil // no durable rate limit
		if err := c.Once(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v with a nil ledger across three restarts; a real "+
			"pass with no durable rate limit must fail closed and delete "+
			"nothing — absence of a budget is never a licence to delete", got)
	}
}

// --- Scenarios that the implementation HANDLES CORRECTLY -------------
// Kept as regression pins: these are the adversarial cases the review
// probed and found sound.

// A cluster with no in-scope agent nodes at all must not divide by
// zero, panic, or trip anything — scope == 0 with stale == 0 returns
// before any fraction arithmetic.
func TestAdversarialEmptyScopeIsInert(t *testing.T) {
	cs := fake.NewSimpleClientset(
		// Control-plane-only single-node cluster.
		node("plane-solo", "u-solo",
			withLabels(agentLabels("plane")),
			withLabel(ControlPlaneRoleLabel, "true"),
			withReady(corev1.ConditionFalse, tickNow.Add(-500*time.Hour))),
		// A virtual-kubelet node: out of scope by runtime label.
		node("vk-1", "u-vk",
			withLabel(RuntimeLabel, "virtual"),
			withReady(corev1.ConditionUnknown, tickNow.Add(-500*time.Hour))),
		// A foreign, unlabelled node.
		node("someone-elses", "u-foreign",
			withReady(corev1.ConditionUnknown, tickNow.Add(-500*time.Hour))),
	)
	c := collector(t, cs)
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("deleted %v from an all-out-of-scope cluster, want none", got)
	}
}

// A genuine single-node worker cluster that partitions: 1 of 1 stale.
// breakerMinStale deliberately exempts it (bounded blast radius), and
// that is the documented, accepted trade — pinned here so a future
// change to breakerMinStale is a conscious one.
func TestAdversarialSingleNodeClusterIsExemptByDesign(t *testing.T) {
	cs := fake.NewSimpleClientset(staleAgentNode("pi-only", "pi", "u1", 48*time.Hour))
	if err := collector(t, cs).Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := deletedNames(cs); len(got) != 1 {
		t.Fatalf("deleted %v, want the single ghost reaped", got)
	}
}

// Exactly-at-threshold (2 of 4 == 0.50) proceeds; one more (3 of 4)
// refuses. Pins the strictly-greater comparison at a small-cluster
// boundary, complementing the 5/10 vs 6/10 case already covered.
func TestAdversarialBreakerBoundarySmallCluster(t *testing.T) {
	mk := func(staleN, readyN int) *fake.Clientset {
		objs := readyFleet("pi", readyN)
		for i := 0; i < staleN; i++ {
			name := "pi-dead" + string(rune('a'+i))
			objs = append(objs, staleAgentNode(name, "pi", types.UID(name), 48*time.Hour))
		}
		return fake.NewSimpleClientset(objs...)
	}

	cs := mk(2, 2) // 2/4 == 0.50, not > 0.50
	if err := collector(t, cs).Once(context.Background()); err != nil {
		t.Fatalf("at-threshold: %v", err)
	}
	if got := deletedNames(cs); len(got) != 2 {
		t.Fatalf("at-threshold deleted %v, want 2", got)
	}

	cs = mk(3, 1) // 3/4 == 0.75 > 0.50
	if err := collector(t, cs).Once(context.Background()); err != nil {
		t.Fatalf("above-threshold: %v", err)
	}
	if got := deletedNames(cs); len(got) != 0 {
		t.Fatalf("above-threshold deleted %v, want none", got)
	}
}

// A node that goes Ready again between the list snapshot and the
// delete must survive — re-GET plus FULL predicate re-evaluation, not
// just the UID precondition (a recovered node keeps its UID, so the
// precondition alone would not save it).
func TestAdversarialNodeRecoversBetweenListAndDelete(t *testing.T) {
	// Healthy majority so the mass-stale breaker does not pre-empt the
	// behaviour under test.
	objs := readyFleet("pi", 6)
	objs = append(objs,
		staleAgentNode("pi-flap", "pi", "u-flap", 48*time.Hour),
		staleAgentNode("pi-dead", "pi", "u-dead", 96*time.Hour),
	)
	cs := fake.NewSimpleClientset(objs...)
	// Flip pi-flap back to Ready the moment the collector re-GETs it,
	// i.e. after the list snapshot was taken.
	cs.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || get.GetName() != "pi-flap" {
			return false, nil, nil
		}
		return true, readyAgentNode("pi-flap", "pi", "u-flap"), nil
	})

	c := collector(t, cs)
	if err := c.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	got := deletedNames(cs)
	for _, n := range got {
		if n == "pi-flap" {
			t.Fatalf("deleted a node that recovered before the delete: %v", got)
		}
	}
	if len(got) != 1 || got[0] != "pi-dead" {
		t.Fatalf("deleted %v, want only [pi-dead]", got)
	}
}
