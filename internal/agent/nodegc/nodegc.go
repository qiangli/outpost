// Package nodegc reclaims stale Kubernetes Node objects on a
// peer-hosted DKS control plane.
//
// WHY. A tunnelled worker that leaves the cluster (or simply dies)
// cannot delete its own Node object — `outpost cluster leave` holds
// only a k3s join token, deliberately not an admin credential for the
// peer apiserver (docs/cluster-peer.md, "Boundary: the Kubernetes Node
// object is not deleted by the worker"). And k3s agents register with
// `--with-node-id`, so every rejoin mints a NEW node name: without GC
// the plane accumulates NotReady ghosts that pollute `kubectl get
// nodes`, scheduling scores, and the status view forever.
//
// The collector is deliberately narrow and slow. It only ever deletes
// a node that is self-consistently OURS — exact runtime/backend labels,
// a nonempty host label, and a name carrying that host's prefix — and
// that has been NotReady/Unknown for longer than a 24 h grace period.
// Virtual-kubelet nodes (runtime=virtual), foreign or unlabelled nodes,
// renamed nodes, Ready nodes, recently-transitioned nodes, and nodes
// missing readiness evidence are never touched: absence of evidence is
// unknown, not staleness. Deletes are bounded per tick, re-verified by
// a fresh GET immediately beforehand, and guarded by a UID precondition
// so a same-name replacement registered between passes survives. Any
// API error stops the whole pass — a flaky apiserver is a reason to do
// nothing, not to guess.
//
// The same evidence rule holds at FLEET level: absence of evidence
// from ALL nodes at once is the strongest possible signal that the
// OBSERVER — this host's network, tunnel, or clock — is what broke,
// not the fleet. So beyond the per-node predicate:
//
//   - a pass is refused outright when the stale fraction of the
//     ELIGIBLE outpost-agent population crosses a threshold (a mass
//     partition must never drain the cluster at MaxDeletes/tick). The
//     denominator is the DELETABLE population — structurally-excluded
//     nodes (own host, control-plane role) are left out, so they cannot
//     dilute the fraction and let a full-worker partition slip under it;
//   - the delete budget is a wall-clock rate persisted in a JSONL
//     ledger, so outpost's frequent self-restarts (any builtin toggle)
//     cannot mint a fresh budget per restart. With NO ledger a real pass
//     fails closed and deletes nothing — an absent durable rate limit is
//     never a licence to delete;
//   - readiness evidence timestamped in our future refuses the pass, and
//     so does a wall clock that raced FORWARD of the collector's
//     monotonic uptime between passes (VM resume, restored snapshot,
//     delayed NTP step) — the direction the future-stamp check cannot
//     see. Deletions additionally require the collector to have been up —
//     measured monotonically — long enough for a boot-skewed RTC to
//     have been NTP-corrected before we trust wall-clock arithmetic;
//   - nodes claiming this host's own identity and nodes carrying a
//     control-plane role label are never candidates.
//
// SCOPE DECISION (ownership). The candidate filter trusts node LABELS,
// which are self-asserted by the joining kubelet — the plane holds no
// per-node owner record to check against, and this collector's charter
// is to work offline with no cloudbox feed. Under the dks tenancy
// model (dhnt/docs/dks-tenancy-model.md) a plane may admit workers
// joined by other owners; a dead node they labelled as an outpost k3s
// agent WILL eventually be reaped here. That is accepted and recorded
// in docs/cluster-peer.md: the plane host is the tenancy authority for
// its own plane, deletion additionally requires >grace of NotReady
// (a live tenant node can never be reaped via labels alone), and the
// Node object is cheap to re-mint by rejoining. What the filter must
// never do is delete the plane's OWN node or any control-plane node —
// both are excluded structurally, not by label trust.
//
// This runs ONLY where the plane it manages runs: the control-plane
// host. It needs no cloudbox peer list or liveness feed — the node's
// own recorded Ready transition is the staleness clock.
package nodegc

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// RuntimeLabel / BackendLabel / HostLabel are the node-identity
	// labels the k3s agent entrypoint stamps at registration
	// (internal/agent/runtime/image/entrypoint.sh), matching the
	// cloud-hosted control plane's vocabulary exactly.
	RuntimeLabel = "outpost.dhnt.io/runtime"
	BackendLabel = "outpost.dhnt.io/backend"
	HostLabel    = "outpost.dhnt.io/host"

	// RuntimeAgent / BackendK3s are the only label values GC acts on.
	// Virtual-kubelet nodes carry runtime=virtual and a vk-* backend;
	// they have no kubelet lease of their own worth reaping here.
	RuntimeAgent = "agent"
	BackendK3s   = "k3s"

	// ControlPlaneRoleLabel / MasterRoleLabel mark apiserver-hosting
	// nodes (k3s stamps the first on its server node). A node carrying
	// either is NEVER a GC candidate regardless of its other labels —
	// reaping the plane's own node is strictly worse than reaping a
	// worker, and role labels are at least plane-adjacent facts rather
	// than pure worker self-assertion.
	ControlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"
	MasterRoleLabel       = "node-role.kubernetes.io/master"

	// DefaultGrace is how long a node must have been NotReady/Unknown
	// (per its Ready condition's LastTransitionTime) before it is
	// stale. A day comfortably clears laptop sleeps, reboots, and
	// multi-hour network outages — the node of a machine that comes
	// back inside the window reconnects and goes Ready again.
	DefaultGrace = 24 * time.Hour

	// DefaultMaxDeletes bounds deletes per rate window (see
	// DefaultInterval). GC converges over several windows instead of
	// mass-deleting in one, so one bad pass can only do bounded damage.
	DefaultMaxDeletes = 3

	// DefaultInterval is the pass cadence AND the delete-rate window:
	// at most MaxDeletes deletions per Interval of wall clock, counted
	// from the persisted ledger — not per pass, and not per process
	// lifetime. Outpost self-restarts on any builtin toggle; a budget
	// that reset with the process would multiply by restart count.
	DefaultInterval = time.Hour

	// DefaultSettle delays the FIRST pass after Run starts. Boot is
	// exactly when evidence is least trustworthy: the tunnel may not
	// be up, NTP may not have corrected a wrong RTC yet, and restart
	// storms (10 toggles == 10 boots) would otherwise each get an
	// immediate pass.
	DefaultSettle = 5 * time.Minute

	// DefaultMinUptime is how long the collector must have been
	// running — measured monotonically, immune to wall-clock jumps —
	// before it is allowed to DELETE. Staleness is wall-clock
	// arithmetic against apiserver-recorded timestamps; a host whose
	// RTC was wrong at boot (consumer hardware, restored VM snapshot,
	// dual-boot) computes garbage elapsed times until NTP fixes the
	// clock, and boot is also when the settle-window pass fires. An
	// hour of uptime — a meaningful fraction of the 24 h grace — means
	// NTP had ample time to sync before we trust the math. Passes
	// before that run read-only.
	DefaultMinUptime = time.Hour

	// DefaultMaxStaleFraction is the cluster-wide unhealthy circuit
	// breaker (upstream's --unhealthy-zone-threshold analog): when
	// MORE than this fraction of the observed outpost-agent population
	// is stale at once, the pass deletes nothing. "Everything looks
	// dead" nearly always means the observer broke — a >24 h partition
	// of the control-plane host flips every worker to Unknown with the
	// same LastTransitionTime, and without this guard GC would drain
	// the cluster at MaxDeletes per window until empty.
	DefaultMaxStaleFraction = 0.5

	// breakerMinStale is the floor below which the fraction breaker
	// does not apply: a single stale node is bounded blast radius by
	// definition, and refusing it forever would mean a one- or
	// two-worker home cluster could never reap a decommissioned
	// worker (1/1 == 100% stale). Mass deletion requires mass.
	breakerMinStale = 2

	// futureSkewTolerance is how far in OUR future a recorded Ready
	// transition may sit before it proves the observer's clock and the
	// apiserver's records disagree. Generous against ordinary NTP
	// drift; tiny against a wrong RTC.
	futureSkewTolerance = 5 * time.Minute

	// clockJumpTolerance bounds how much MORE wall-clock time than
	// monotonic time may elapse between two consecutive passes before we
	// treat it as a forward clock jump (VM resume, restored snapshot,
	// dual-boot RTC-in-localtime, a delayed NTP step) rather than
	// ordinary scheduling jitter. This is the guard for the DANGEROUS
	// direction the future-stamp check cannot see: a forward jump leaves
	// no future timestamps — every past Ready transition merely looks
	// older, so a briefly-down node reads as long-dead and is reaped.
	// Only the forward direction is guarded: a backward or frozen wall
	// clock makes nodes look YOUNGER and suppresses deletion (the safe
	// direction), and a backward jump that surfaces as future-stamped
	// evidence is caught separately. Ten minutes swamps NTP slew
	// (~1.8 s/h) yet is tiny against a jump large enough to cross grace.
	clockJumpTolerance = 10 * time.Minute
)

// Collector deletes stale agent Node objects from the hosted plane.
type Collector struct {
	Client     kubernetes.Interface
	Grace      time.Duration    // 0 => DefaultGrace
	MaxDeletes int              // 0 => DefaultMaxDeletes (per Interval window, not per pass)
	Interval   time.Duration    // 0 => DefaultInterval (pass cadence AND rate window)
	Log        *slog.Logger     // nil => slog.Default
	Now        func() time.Time // nil => time.Now

	// DryRun computes, logs, and ledgers exactly what a real pass
	// would delete — including budget and breaker decisions — but
	// never calls Delete.
	DryRun bool

	// SelfHost, when set, excludes any node whose host label claims
	// this host's own identity (the control-plane host may itself run
	// an agent runtime joined to its own plane). Wired from
	// FileConfig.ClusterNodeName().
	SelfHost string

	// MaxStaleFraction overrides DefaultMaxStaleFraction when > 0.
	// A value >= 1 disables the breaker (every fraction is <= 1).
	MaxStaleFraction float64

	// Settle overrides DefaultSettle when > 0: how long Run waits
	// before the first pass.
	Settle time.Duration

	// MinUptime overrides DefaultMinUptime when > 0: monotonic
	// collector uptime required before deletions are allowed.
	MinUptime time.Duration

	// Uptime overrides the monotonic-uptime source (tests). nil =>
	// time.Since(first Run/Once), which Go measures monotonically.
	Uptime func() time.Duration

	// Ledger is the durable JSONL record of deletions and refusals,
	// and the restart-safe half of the rate budget. nil => in-memory
	// budget only (tests / degraded boot); real wiring always sets it.
	Ledger *Ledger

	// startedAt anchors the default Uptime source. Set once at the
	// first Run/Once; time.Since on it reads the monotonic clock, so
	// wall-clock corrections (NTP fixing a bad RTC) cannot inflate it.
	startedAt time.Time

	// lastWall / lastUptime / haveClockBaseline record the wall clock and
	// monotonic uptime observed at the previous pass. The forward-jump
	// guard compares this pass's deltas against them: if wall time raced
	// ahead of monotonic time between passes, the observer's clock stepped
	// and every staleness verdict this pass is suspect. Reset each pass so
	// the collector re-syncs after a jump instead of refusing forever.
	lastWall          time.Time
	lastUptime        time.Duration
	haveClockBaseline bool

	// lastRefusalSig deduplicates consecutive identical refusal ledger
	// lines. A partition that persists across many passes must still
	// REFUSE every pass (the behaviour), but appending an identical line
	// each time would grow the ledger unbounded while remainingBudget
	// re-reads the whole file — so a repeat of the same refusal is not
	// re-recorded. In-memory: a restart re-records once, which is bounded
	// by the settle window.
	lastRefusalSig string

	// recent holds this process's own delete times as a belt-and-braces
	// second budget source alongside a present ledger: if the ledger lags
	// its own writes, memory still bounds the current process. A real pass
	// with NO ledger is refused upstream (fail closed), so this is never
	// the sole budget. Never persisted; the ledger is the cross-restart
	// half.
	recent []time.Time
}

// Run collects until ctx is cancelled. The first pass waits out the
// settle window — there is deliberately no immediate pass, so restart
// storms (outpost restarts on every builtin toggle) never reach a
// pass at all. Pass errors are logged, never fatal — the next tick
// retries from scratch.
func (c *Collector) Run(ctx context.Context) {
	c.ensureStarted()
	interval := c.interval()
	settle := c.Settle
	if settle <= 0 {
		settle = DefaultSettle
	}
	log := c.logger()

	settleTimer := time.NewTimer(settle)
	defer settleTimer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-settleTimer.C:
	}
	if err := c.Once(ctx); err != nil {
		log.Warn("nodegc: pass failed (will retry)", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Once(ctx); err != nil {
				log.Warn("nodegc: pass failed (will retry)", "err", err)
			}
		}
	}
}

func (c *Collector) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) grace() time.Duration {
	if c.Grace > 0 {
		return c.Grace
	}
	return DefaultGrace
}

func (c *Collector) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultInterval
}

func (c *Collector) maxStaleFraction() float64 {
	if c.MaxStaleFraction > 0 {
		return c.MaxStaleFraction
	}
	return DefaultMaxStaleFraction
}

func (c *Collector) minUptime() time.Duration {
	if c.MinUptime > 0 {
		return c.MinUptime
	}
	return DefaultMinUptime
}

func (c *Collector) ensureStarted() {
	if c.startedAt.IsZero() {
		c.startedAt = time.Now() // real clock on purpose: monotonic anchor
	}
}

func (c *Collector) uptime() time.Duration {
	if c.Uptime != nil {
		return c.Uptime()
	}
	return time.Since(c.startedAt)
}

// eligible reports whether node is even ELIGIBLE to be a GC candidate:
// it self-consistently claims to be an outpost k3s agent node
// (ownIdentity) AND clears the structural exclusions that are Collector
// policy rather than node facts — this host's own identity and
// control-plane role labels. An ineligible node can NEVER be deleted, so
// it must not count toward the circuit breaker's denominator either
// (counting it would dilute the stale fraction and let a full-worker
// partition slip under the threshold — DEFECT 1).
func (c *Collector) eligible(n *corev1.Node) bool {
	if !ownIdentity(n) {
		return false
	}
	if c.SelfHost != "" && n.Labels[HostLabel] == c.SelfHost {
		return false
	}
	if _, ok := n.Labels[ControlPlaneRoleLabel]; ok {
		return false
	}
	if _, ok := n.Labels[MasterRoleLabel]; ok {
		return false
	}
	return true
}

// candidateSince is the FULL per-node predicate: structural eligibility
// (eligible) plus StaleSince's stale-readiness check. Used identically on
// the list snapshot and on the pre-delete re-GET.
func (c *Collector) candidateSince(n *corev1.Node, grace time.Duration, now time.Time) (time.Time, bool) {
	if !c.eligible(n) {
		return time.Time{}, false
	}
	return StaleSince(n, grace, now)
}

// observeClock records this pass's wall clock and monotonic uptime and
// reports whether wall time raced forward relative to monotonic time
// since the previous pass by more than clockJumpTolerance — a forward
// clock jump. The baseline is ALWAYS updated (even on a detected jump),
// so once the wall clock settles at its new value the collector re-syncs
// and resumes rather than refusing forever.
func (c *Collector) observeClock(now time.Time, up time.Duration) bool {
	jumped := false
	if c.haveClockBaseline {
		wallDelta := now.Sub(c.lastWall)
		monoDelta := up - c.lastUptime
		jumped = wallDelta-monoDelta > clockJumpTolerance
	}
	c.lastWall = now
	c.lastUptime = up
	c.haveClockBaseline = true
	return jumped
}

// ledgerRefusal durably records a whole-pass refusal, skipping a write
// that is identical to the immediately preceding refusal so a persistent
// partition cannot grow the ledger one line per pass (DEFECT 7). Append
// failures are logged, never fatal — a refusal already deleted nothing.
func (c *Collector) ledgerRefusal(log *slog.Logger, entry LedgerEntry) {
	sig := entry.Event + "|" + entry.Detail
	if sig == c.lastRefusalSig {
		return
	}
	c.lastRefusalSig = sig
	if err := c.Ledger.Append(entry); err != nil {
		log.Warn("nodegc: ledger append failed", "err", err)
	}
}

// remainingBudget returns how many deletions are still allowed inside
// the trailing rate window ending at now. Counts the ledger (survives
// restarts) and this process's memory (survives a broken ledger) and
// honors whichever has seen MORE — the two sources overlap, never sum.
func (c *Collector) remainingBudget(now time.Time, window time.Duration, maxDeletes int) (int, error) {
	cutoff := now.Add(-window)
	used := 0
	for _, t := range c.recent {
		if t.After(cutoff) {
			used++
		}
	}
	if c.Ledger != nil {
		n, err := c.Ledger.CountDeletesSince(cutoff)
		if err != nil {
			return 0, fmt.Errorf("nodegc: read delete ledger (refusing to delete blind): %w", err)
		}
		if n > used {
			used = n
		}
	}
	return maxDeletes - used, nil
}

// Once performs a single bounded GC pass. Any List/Get/Delete error —
// and any failure to durably record a deletion — aborts the pass and
// is returned, never swallowed. Fleet-level refusals (mass-stale
// breaker, clock skew, insufficient uptime) are DECISIONS, not errors:
// they log, ledger where durable evidence matters, and return nil.
func (c *Collector) Once(ctx context.Context) error {
	if c.Client == nil {
		return fmt.Errorf("nodegc: Client is required")
	}
	c.ensureStarted()
	log := c.logger()
	grace := c.grace()
	maxDeletes := c.MaxDeletes
	if maxDeletes <= 0 {
		maxDeletes = DefaultMaxDeletes
	}

	// One wall-clock reading for the whole pass, cross-checked against the
	// monotonic uptime anchor. clockJumped is consumed below only if there
	// is something this pass would otherwise delete.
	now := c.now()
	clockJumped := c.observeClock(now, c.uptime())

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	nodes, err := c.Client.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("nodegc: list nodes: %w", err)
	}

	// Filtering happens HERE, in code, not in a server-side selector:
	// every exclusion rule is then a tested code path, and the pre-delete
	// re-evaluation below runs the identical predicate on the fresh GET.
	type candidate struct {
		name       string
		uid        types.UID
		transition time.Time
	}
	var stale []candidate
	scope := 0        // observed outpost-agent population (breaker denominator)
	futureStamps := 0 // in-scope Ready transitions recorded in OUR future
	for i := range nodes.Items {
		n := &nodes.Items[i]
		// Only ELIGIBLE nodes (ownIdentity minus the structural
		// exclusions) count toward scope — the breaker denominator — so a
		// node that can never be deleted cannot dilute the stale fraction
		// (DEFECT 1). candidateSince re-applies the same eligibility.
		if !c.eligible(n) {
			continue
		}
		scope++
		if t, ok := readyTransition(n); ok && t.After(now.Add(futureSkewTolerance)) {
			futureStamps++
			log.Warn("nodegc: node readiness evidence is from the future — observer clock suspect",
				"node", n.Name, "transition", t.UTC().Format(time.RFC3339))
			continue
		}
		if t, ok := c.candidateSince(n, grace, now); ok {
			stale = append(stale, candidate{name: n.Name, uid: n.UID, transition: t})
		}
	}
	if len(stale) == 0 && futureStamps == 0 {
		return nil
	}

	// Forward-clock-jump refusal: wall time raced ahead of monotonic
	// uptime since the last pass, so every staleness verdict this pass was
	// computed against a clock that just stepped forward — the same
	// "observer is the suspect" logic the future-stamp check applies to a
	// backward step. Checked first because a jumped clock invalidates the
	// breaker's own timestamps too (DEFECT 2).
	if clockJumped {
		log.Error("nodegc: refusing pass — wall clock jumped forward relative to monotonic uptime",
			"stale", len(stale), "scope", scope)
		c.ledgerRefusal(log, LedgerEntry{At: now, Event: EventRefusedClockSkew,
			Stale: len(stale), Scope: scope,
			Detail: "wall clock advanced faster than monotonic uptime since last pass (forward jump)"})
		return nil
	}

	// Clock-skew refusal: a future timestamp proves our clock and the
	// apiserver's records disagree, and every staleness verdict this
	// pass was computed with that same suspect clock. Refuse them all.
	if futureStamps > 0 {
		log.Error("nodegc: refusing pass — readiness evidence from the future",
			"future_stamps", futureStamps, "stale", len(stale), "scope", scope)
		c.ledgerRefusal(log, LedgerEntry{At: now, Event: EventRefusedClockSkew,
			Stale: len(stale), Scope: scope,
			Detail: fmt.Sprintf("%d in-scope node(s) with Ready transition in the future", futureStamps)})
		return nil
	}

	// Cluster-wide unhealthy circuit breaker. Absence of evidence from
	// ALL nodes at once must never authorize mass deletion: when most
	// of the observed fleet looks dead, the observer is the suspect.
	if len(stale) >= breakerMinStale &&
		float64(len(stale)) > c.maxStaleFraction()*float64(scope) {
		log.Error("nodegc: refusing pass — stale fraction exceeds threshold (mass partition?)",
			"stale", len(stale), "scope", scope, "max_fraction", c.maxStaleFraction())
		c.ledgerRefusal(log, LedgerEntry{At: now, Event: EventRefusedMassStale,
			Stale: len(stale), Scope: scope,
			Detail: fmt.Sprintf("stale %d/%d exceeds max fraction %.2f", len(stale), scope, c.maxStaleFraction())})
		return nil
	}

	// Uptime gate: wall-clock staleness arithmetic is only trusted
	// after the collector has been up (monotonically) long enough for
	// a boot-skewed RTC to have been corrected. Until then the pass is
	// read-only.
	if up := c.uptime(); up < c.minUptime() {
		log.Info("nodegc: read-only pass — collector uptime below minimum for deletions",
			"uptime", up.Round(time.Second), "min_uptime", c.minUptime(), "candidates", len(stale))
		return nil
	}

	// Fail closed on a missing durable ledger. The ledger is the
	// cross-restart half of the rate budget; without it the only bound is
	// this process's memory, which every self-restart (a builtin toggle)
	// resets — reviving the pre-#38 "fresh budget per restart" defect and
	// deleting with no durable rate limit at all (DEFECT 3). Destructive
	// unattended code must not silently degrade its own rate limiter: no
	// ledger, no deletions. Dry run needs no budget (it never deletes) and
	// is allowed through to log/ledger what a real pass would do.
	if !c.DryRun && c.Ledger == nil {
		log.Error("nodegc: read-only pass — no durable delete ledger configured (fail closed)",
			"candidates", len(stale))
		return nil
	}

	// Restart-safe rate budget: MaxDeletes per Interval of WALL CLOCK,
	// counted from the persisted ledger — not per pass, and not per
	// process. Ten toggle-restarts in ten minutes share one budget.
	remaining, err := c.remainingBudget(now, c.interval(), maxDeletes)
	if err != nil {
		return err
	}
	if remaining <= 0 {
		log.Info("nodegc: delete budget for the current window already spent", "window", c.interval(), "candidates", len(stale))
		return nil
	}

	// Deterministic oldest-first: the longest-dead node goes first, and
	// equal timestamps fall back to name order so two passes over the
	// same state always pick the same bounded subset.
	sort.Slice(stale, func(i, j int) bool {
		if !stale[i].transition.Equal(stale[j].transition) {
			return stale[i].transition.Before(stale[j].transition)
		}
		return stale[i].name < stale[j].name
	})

	deleted := 0
	for _, cand := range stale {
		if deleted >= remaining {
			log.Info("nodegc: delete budget reached", "deleted", deleted, "remaining_candidates", len(stale)-deleted)
			break
		}

		// Re-GET and fully re-evaluate immediately before deleting: the
		// list snapshot may be minutes stale, and a node that recovered
		// (or was replaced) in the meantime must survive.
		getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		fresh, err := c.Client.CoreV1().Nodes().Get(getCtx, cand.name, metav1.GetOptions{})
		cancel()
		if err != nil {
			return fmt.Errorf("nodegc: re-get node %s: %w", cand.name, err)
		}
		if fresh.UID != cand.uid {
			// Same name, different object: a replacement registered
			// since the list. Not ours to judge this pass.
			log.Info("nodegc: node replaced since list, skipping", "node", cand.name)
			continue
		}
		if _, ok := c.candidateSince(fresh, grace, c.now()); !ok {
			log.Info("nodegc: node no longer stale, skipping", "node", cand.name)
			continue
		}

		since := cand.transition.UTC().Format(time.RFC3339)
		if c.DryRun {
			deleted++
			log.Info("nodegc: DRY RUN — would delete stale node", "node", cand.name, "not_ready_since", since)
			if err := c.Ledger.Append(LedgerEntry{At: c.now(), Event: EventDryRunDeleted,
				Node: cand.name, UID: string(cand.uid), NotReadySince: since}); err != nil {
				log.Warn("nodegc: ledger append failed", "err", err)
			}
			continue
		}

		uid := fresh.UID
		delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = c.Client.CoreV1().Nodes().Delete(delCtx, cand.name, metav1.DeleteOptions{
			// UID precondition: the apiserver refuses the delete if the
			// object was replaced between our GET and this call.
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		cancel()
		if err != nil {
			return fmt.Errorf("nodegc: delete node %s: %w", cand.name, err)
		}
		deleted++
		c.recent = append(c.recent, c.now())
		log.Info("nodegc: deleted stale node", "node", cand.name, "not_ready_since", since)
		if err := c.Ledger.Append(LedgerEntry{At: c.now(), Event: EventDeleted,
			Node: cand.name, UID: string(cand.uid), NotReadySince: since}); err != nil {
			// The ledger is the cross-restart half of the rate limit.
			// If deletions can't be counted durably, stop deleting.
			return fmt.Errorf("nodegc: record deletion of %s (stopping pass): %w", cand.name, err)
		}
	}
	c.pruneRecent()
	return nil
}

// pruneRecent drops in-memory delete times older than the rate window
// so the slice stays bounded over a long-lived daemon.
func (c *Collector) pruneRecent() {
	cutoff := c.now().Add(-c.interval())
	kept := c.recent[:0]
	for _, t := range c.recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.recent = kept
}

// ownIdentity reports whether node self-consistently claims to be an
// outpost k3s agent node: exact runtime/backend labels, a nonempty
// host label, and a name carrying that host's prefix (k3s
// `--with-node-id` names the node <host>-<id>).
func ownIdentity(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	if node.Labels[RuntimeLabel] != RuntimeAgent || node.Labels[BackendLabel] != BackendK3s {
		return false
	}
	host := node.Labels[HostLabel]
	if host == "" {
		return false
	}
	return strings.HasPrefix(node.Name, host+"-")
}

// readyTransition returns the node's Ready condition transition time,
// if the node carries a Ready condition with a nonzero timestamp.
func readyTransition(node *corev1.Node) (time.Time, bool) {
	for _, cond := range node.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}
		if cond.LastTransitionTime.IsZero() {
			return time.Time{}, false
		}
		return cond.LastTransitionTime.Time, true
	}
	return time.Time{}, false
}

// StaleSince reports whether node is a stale GC candidate at time now,
// and if so since when (its Ready condition's LastTransitionTime).
//
// A candidate must be self-consistently an outpost k3s agent node:
// exact runtime/backend labels, a nonempty host label, and a name
// prefixed by that host — anything else (virtual, foreign, unlabelled,
// renamed) is out of scope. It must carry a Ready condition that is
// False or Unknown with a nonzero LastTransitionTime strictly older
// than grace; a missing condition or timestamp is unknown, not stale.
//
// This is the pure per-node predicate. The Collector layers its
// structural exclusions (own host, control-plane role) and the
// fleet-level guards (mass-stale breaker, clock skew, rate budget)
// on top — see candidateSince and Once.
func StaleSince(node *corev1.Node, grace time.Duration, now time.Time) (time.Time, bool) {
	if node == nil {
		return time.Time{}, false
	}
	if !ownIdentity(node) {
		return time.Time{}, false
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}
		if cond.Status != corev1.ConditionFalse && cond.Status != corev1.ConditionUnknown {
			return time.Time{}, false
		}
		if cond.LastTransitionTime.IsZero() {
			return time.Time{}, false
		}
		if now.Sub(cond.LastTransitionTime.Time) <= grace {
			return time.Time{}, false
		}
		return cond.LastTransitionTime.Time, true
	}
	// No Ready condition at all: no evidence either way. Skip.
	return time.Time{}, false
}
