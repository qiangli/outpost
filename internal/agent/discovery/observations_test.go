package discovery

import (
	"path/filepath"
	"testing"
	"time"
)

// TestObservationsConvergence feeds a synthetic schedule into the
// EWMA: peer present every tick for ~8 weeks at hour h. The
// probability for hour h should converge close to 1.0 and stay
// close to 0 at other hours.
//
// Production default ObsHalfLifeObservations=24 would take 192
// observations to fully saturate the bucket; the test shrinks the
// half-life to 4 observations so 8 weeks of weekly observations
// (8 hits per bucket) saturates above 0.5.
func TestObservationsConvergence(t *testing.T) {
	prev := ObsHalfLifeObservations
	ObsHalfLifeObservations = 4
	defer func() { ObsHalfLifeObservations = prev }()

	tmp := t.TempDir()
	o, err := OpenObservations(filepath.Join(tmp, "obs.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	peer := PeerID("SHA256:office")
	// Anchor at a Monday midnight UTC so hourOfWeek matches hour-of-day
	// for the first week and the test's mental model lines up with
	// the storage shape.
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday
	if hourOfWeek(start) != 0 {
		t.Fatalf("start is not Monday-midnight; got bucket %d", hourOfWeek(start))
	}
	targetHour := 17 // Monday at 5pm UTC -> bucket 17
	// Feed 8 weeks of observations:
	//   - at the target hour: present
	//   - at all other hours: absent (peer not in `seen`)
	for week := range 8 {
		for hr := range 168 {
			at := start.Add(time.Duration(week*168+hr) * time.Hour)
			if hr == targetHour {
				o.Record(at, []PeerID{peer})
			} else {
				o.Record(at, nil)
			}
		}
	}
	pAtTarget := o.Predict(peer, targetHour)
	pAtOther := o.Predict(peer, (targetHour+12)%168)
	if pAtTarget < 0.5 {
		t.Errorf("Predict at target hour = %.3f, want ≥ 0.5", pAtTarget)
	}
	if pAtOther > 0.1 {
		t.Errorf("Predict at off-hour = %.3f, want ≤ 0.1", pAtOther)
	}
}

// TestObservationsDecays confirms that a peer that USED to be present
// every Tuesday at 9am but stopped showing up converges toward 0
// within roughly the half-life window. Inverse of the convergence
// test; same machinery.
func TestObservationsDecays(t *testing.T) {
	prev := ObsHalfLifeObservations
	ObsHalfLifeObservations = 4
	defer func() { ObsHalfLifeObservations = prev }()

	tmp := t.TempDir()
	o, _ := OpenObservations(filepath.Join(tmp, "obs.json"))
	peer := PeerID("SHA256:old")
	hr := 5
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday midnight
	// Build up presence for 8 weeks (one observation per week at hour hr).
	for week := range 8 {
		at := start.Add(time.Duration(week*168+hr) * time.Hour)
		o.Record(at, []PeerID{peer})
	}
	high := o.Predict(peer, hr)
	if high < 0.5 {
		t.Fatalf("expected high baseline after 8w of presence; got %.3f", high)
	}
	// Then 8 weeks of absence — the daemon still ticks at hour hr but
	// peer isn't in the seen set, so present=0.
	for week := 8; week < 16; week++ {
		at := start.Add(time.Duration(week*168+hr) * time.Hour)
		_ = o.Record
		o.Record(at, nil)
	}
	low := o.Predict(peer, hr)
	if low >= high {
		t.Errorf("expected probability to decay (%.3f -> %.3f)", high, low)
	}
}

// TestObservationsSaveLoad confirms the on-disk persistence shape.
func TestObservationsSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "obs.json")
	o, _ := OpenObservations(path)
	o.Record(time.Now(), []PeerID{"SHA256:abc"})
	if err := o.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	reopened, err := OpenObservations(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	got := reopened.Predict("SHA256:abc", hourOfWeek(time.Now()))
	if got <= 0 {
		t.Errorf("predict after reload = %v, want > 0", got)
	}
}

// TestHourOfWeek pins the bucket index for a known date so future
// refactors don't silently shift it.
func TestHourOfWeek(t *testing.T) {
	// Monday 2026-01-05 00:00 UTC — Monday-00 should be bucket 0.
	mon := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	if got := hourOfWeek(mon); got != 0 {
		t.Errorf("Monday 00:00 hourOfWeek=%d, want 0", got)
	}
	// Sunday 2026-01-04 23:00 UTC — Sunday-23 should be bucket 167.
	sun23 := time.Date(2026, 1, 4, 23, 0, 0, 0, time.UTC)
	if got := hourOfWeek(sun23); got != 167 {
		t.Errorf("Sunday 23:00 hourOfWeek=%d, want 167", got)
	}
}

// TestObservationAlpha pins the smoothing-factor math: with
// ObsHalfLifeObservations=N, the alpha should be 1 - 0.5^(1/N).
// Spot-check the default (N=24): alpha ≈ 0.0285.
func TestObservationAlpha(t *testing.T) {
	prev := ObsHalfLifeObservations
	ObsHalfLifeObservations = 24
	defer func() { ObsHalfLifeObservations = prev }()
	a := observationAlpha()
	if a < 0.0280 || a > 0.0290 {
		t.Errorf("alpha(half-life=24) = %.5f, want ~0.0285", a)
	}
}
