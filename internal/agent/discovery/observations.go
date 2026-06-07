// Temporal topology observations: PRoPHET-lite presence prediction.
//
// Every ObsTickInterval (5 min default) the daemon snapshots the
// discovery cache as a set of (peer_id, reachable, hour_of_week)
// records. The Observations struct aggregates these into a rolling
// EWMA per (peer, hour-of-week) bucket.
//
// EWMA half-life is 14 days (ObsHalfLife). That converges fast enough
// to track a "started working from home Tuesdays" shift within ~two
// weeks, while smoothing out a single missed observation. The math:
//
//	alpha = 1 - exp(-tick / half_life * ln(2))
//	prob' = prob*(1-alpha) + present*alpha
//
// where `present` is 1 if the peer was observed in this tick, 0 if not.
//
// Storage: a flat JSON map on disk at <cacheDir>/outpost/peers_obs.json.
// Small enough (~5KB per peer per week of buckets) to load wholesale.
// Updated atomically (write to tmp + rename).
//
// Wave 3B.1 only RECORDS observations. The act-on-predictions surface
// (`outpost scan --predicted`, pre-warm SSH connections) lands in
// Wave 3B.2.
package discovery

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// ObsTickInterval is the cadence at which the daemon should
	// snapshot the current cache and feed it through the EWMA
	// update. Wave 3B.1 records-only; the daemon-side ticker lands
	// with the cache wiring in Wave 3B.2.
	ObsTickInterval = 5 * time.Minute

	// ObsBuckets is the number of (peer, hour-of-week) buckets we
	// track per peer. 7 days * 24 hours = 168.
	ObsBuckets = 7 * 24
)

// ObsHalfLifeObservations is the EWMA half-life expressed in
// *observation count* rather than wall-clock time. That's the right
// unit because each (peer, hour-of-week) bucket only updates when the
// tick happens to land in that hour — calibrating in wall-clock time
// gives wildly different effective half-lives for different tick
// cadences. With the default ObsTickInterval (5 min) each bucket sees
// 12 updates per occupied hour per week, so 24 observations cover
// roughly 2 weeks of production data — fast enough to track a
// schedule shift, slow enough to absorb the occasional missed tick.
// Exposed as a var so tests can shrink it for fast convergence.
var ObsHalfLifeObservations = 24.0

// Observations is the temporal-presence model. Safe for concurrent
// use. One global instance per daemon.
type Observations struct {
	path string

	mu     sync.Mutex
	byPeer map[PeerID]*peerObservation
}

// peerObservation is the per-peer EWMA table. Buckets[h] is
// P(reachable | hour-of-week=h) ∈ [0, 1].
type peerObservation struct {
	Buckets [ObsBuckets]float64 `json:"buckets"`
	// LastSeen is the most recent At we observed this peer (any
	// hour). Used to compute the staleness display in scan
	// --predicted.
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// OpenObservations loads the model from disk. Missing file => empty
// model. Corrupted file => empty model (we log silently rather than
// crash).
func OpenObservations(path string) (*Observations, error) {
	if path == "" {
		return nil, errors.New("discovery: empty observations path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	o := &Observations{
		path:   path,
		byPeer: make(map[PeerID]*peerObservation),
	}
	b, err := os.ReadFile(path)
	if err == nil && len(b) > 0 {
		var loaded map[PeerID]*peerObservation
		if jerr := json.Unmarshal(b, &loaded); jerr == nil {
			o.byPeer = loaded
		}
	}
	return o, nil
}

// Record applies one EWMA update for each peer present in `seen`.
// Peers NOT in `seen` get a "0" observation for the current hour
// bucket (we observed; they weren't there).
//
// `now` is configurable for tests; production callers pass time.Now().
func (o *Observations) Record(now time.Time, seen []PeerID) {
	if o == nil {
		return
	}
	hour := hourOfWeek(now)
	alpha := observationAlpha()

	seenSet := make(map[PeerID]bool, len(seen))
	for _, p := range seen {
		seenSet[p] = true
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	// Update every known peer's bucket: 1 if present this tick, 0 if not.
	for id, ob := range o.byPeer {
		present := 0.0
		if seenSet[id] {
			present = 1.0
			ob.LastSeen = now
		}
		ob.Buckets[hour] = ob.Buckets[hour]*(1-alpha) + present*alpha
	}
	// Insert newly-seen peers we hadn't tracked before.
	for _, id := range seen {
		if _, ok := o.byPeer[id]; ok {
			continue
		}
		ob := &peerObservation{LastSeen: now}
		ob.Buckets[hour] = alpha // first observation; 1*alpha + 0*(1-alpha)
		o.byPeer[id] = ob
	}
}

// Predict returns the EWMA presence probability for peer at the
// given hour-of-week. Returns 0 for never-seen peers.
func (o *Observations) Predict(peer PeerID, hour int) float64 {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	ob, ok := o.byPeer[peer]
	if !ok {
		return 0
	}
	if hour < 0 || hour >= ObsBuckets {
		return 0
	}
	return ob.Buckets[hour]
}

// PredictedView is the wire shape returned by `outpost scan --predicted`.
type PredictedView struct {
	PeerID      PeerID    `json:"peer_id"`
	Hour        int       `json:"hour_of_week"`
	Probability float64   `json:"probability"`
	LastSeenAt  time.Time `json:"last_seen_at,omitzero"`
}

// PredictedAt returns predictions for every known peer at the given
// hour, sorted by probability descending. Used by the CLI surface.
func (o *Observations) PredictedAt(hour int) []PredictedView {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]PredictedView, 0, len(o.byPeer))
	for id, ob := range o.byPeer {
		out = append(out, PredictedView{
			PeerID:      id,
			Hour:        hour,
			Probability: ob.Buckets[hour],
			LastSeenAt:  ob.LastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Probability > out[j].Probability
	})
	return out
}

// Save persists the model to disk atomically. Called by the daemon's
// observation-tick loop after each Record.
func (o *Observations) Save() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	snapshot := o.byPeer
	o.mu.Unlock()

	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}

// hourOfWeek returns the bucket index 0..167 for a given time.
// Monday-00:00 = 0; Sunday-23:00 = 167. Matches what `date +%u%H`
// roughly indicates and is stable across timezones (we use UTC).
func hourOfWeek(t time.Time) int {
	t = t.UTC()
	// time.Weekday: Sunday=0, ..., Saturday=6. Re-map so Monday=0.
	wd := int(t.Weekday())
	wd = (wd + 6) % 7
	return wd*24 + t.Hour()
}

// observationAlpha computes the EWMA smoothing factor such that the
// effect of one observation has half-weight after
// ObsHalfLifeObservations more observations of the same bucket.
//
//	alpha = 1 - 0.5^(1/N)
//
// At N = 24 (default), alpha ≈ 0.0285 — after 24 consistent observations
// of "peer present", the EWMA reaches ~50%; after 168 it reaches ~99%.
func observationAlpha() float64 {
	if ObsHalfLifeObservations <= 0 {
		return 1
	}
	return 1 - math.Pow(0.5, 1.0/ObsHalfLifeObservations)
}
