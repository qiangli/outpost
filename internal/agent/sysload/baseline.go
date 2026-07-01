package sysload

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// baseline is a rolling per-hour-of-day model of "normal" CPU load. Each
// of the 24 buckets holds an exponentially-weighted moving average of
// the effective CPU percentage observed during that hour, updated across
// days so the shape reflects the machine's habitual rhythm (busy in the
// afternoon, quiet overnight) rather than the last few minutes. Persisted
// to disk so the learned profile survives restarts.
type baseline struct {
	mu      sync.Mutex
	buckets [24]float64
	counts  [24]int
	updated time.Time
}

// baselineFile is the on-disk JSON shape. Separate from the in-memory
// struct so the mutex/time internals don't leak into the file format.
type baselineFile struct {
	Buckets   [24]float64 `json:"buckets"`
	Counts    [24]int     `json:"counts"`
	UpdatedAt time.Time   `json:"updated_at,omitzero"`
}

// loadBaseline reads a persisted baseline from path. A missing/corrupt
// file yields an empty baseline (learned fresh) — never an error, so a
// first boot or a schema change degrades gracefully.
func loadBaseline(path string) *baseline {
	b := &baseline{}
	if path == "" {
		return b
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	var bf baselineFile
	if err := json.Unmarshal(data, &bf); err != nil {
		return b
	}
	b.buckets = bf.Buckets
	b.counts = bf.Counts
	b.updated = bf.UpdatedAt
	return b
}

// update folds a reading into the hour's bucket via EWMA. The first
// reading for an empty bucket seeds it directly (so the baseline isn't
// dragged up from zero over the first day).
func (b *baseline) update(hour int, val float64) {
	if hour < 0 || hour > 23 || val < 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.counts[hour] == 0 {
		b.buckets[hour] = val
	} else {
		b.buckets[hour] = baselineAlpha*val + (1-baselineAlpha)*b.buckets[hour]
	}
	b.counts[hour]++
}

// get returns the learned baseline CPU % for the hour, or 0 when the
// bucket has never been observed (the relative-busy check treats 0 as
// "no baseline yet" and leans on the absolute thresholds instead).
func (b *baseline) get(hour int) float64 {
	if hour < 0 || hour > 23 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.counts[hour] == 0 {
		return 0
	}
	return b.buckets[hour]
}

// snapshot returns a copy of the current buckets for persistence.
func (b *baseline) snapshot() baselineFile {
	b.mu.Lock()
	defer b.mu.Unlock()
	return baselineFile{Buckets: b.buckets, Counts: b.counts, UpdatedAt: b.updated}
}
