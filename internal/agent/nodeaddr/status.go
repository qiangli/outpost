package nodeaddr

import (
	"sync"
	"time"
)

// Process-wide liveness snapshot for the address reconciler.
//
// WHY A PACKAGE-LEVEL SNAPSHOT. The operator's question during the incident
// this was written for was exactly "is the nodeaddr reconciler running at
// all?", and the only way to answer it was to grep the daemon's log — which
// said nothing, because a reconciler that never started and one that started
// and failed every pass produced identical (empty) output. `outpost status`
// could not answer it either: nothing in the status surface described the
// reconcilers.
//
// So the reconciler publishes its own liveness here and the admincore status
// prober reads it, mirroring runtime.LastServerHealth() — the same shape the
// control-plane container's health already uses. It is deliberately a
// process-global rather than plumbed through Deps: there is exactly one
// control-plane reconciler set per daemon (running one copy per node would have
// every node racing to patch every other node), so a global is an honest model
// of a genuinely singleton fact, and it keeps the admin surface from having to
// thread a handle through boot just to display a boolean.
type Status struct {
	// Started is true once Run has entered its loop. False means the
	// reconciler was never started — the failure mode that produced a cluster
	// where no Node ever received an ExternalIP.
	Started bool
	// LastRunAt is when the most recent reconcile pass completed.
	LastRunAt time.Time
	// LastError is the most recent pass's error, empty when it succeeded.
	LastError string
	// ConsecutiveFailures counts passes that have failed in a row; zero when
	// the last pass succeeded.
	ConsecutiveFailures int
}

var (
	statusMu sync.RWMutex
	status   Status
)

// LastStatus returns the reconciler's liveness snapshot and whether one has
// ever been recorded. ok=false means Run was never called in this process.
func LastStatus() (Status, bool) {
	statusMu.RLock()
	defer statusMu.RUnlock()
	return status, status.Started
}

func setStatus(fn func(*Status)) {
	statusMu.Lock()
	defer statusMu.Unlock()
	fn(&status)
}

// ResetStatus clears the snapshot. Test-only: the state is process-global, so
// tests that assert on it must not inherit another test's residue.
func ResetStatus() {
	statusMu.Lock()
	defer statusMu.Unlock()
	status = Status{}
}
