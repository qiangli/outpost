// Package sysload is the outpost's considerate system-load profiler. It
// samples CPU utilization, memory availability, and the load average on
// a slow cadence (~30s), learns a per-hour-of-day baseline of "normal"
// load over days, and answers two questions the warm-serving supervisor
// asks continuously:
//
//   - Busy() — is the machine currently doing meaningful non-LLM work?
//     True when the current load is well above this hour's learned
//     baseline OR above absolute safety thresholds (CPU sustained >60%,
//     or available memory <25%). Debounced so a brief spike doesn't
//     flap the answer.
//   - WarmBudgetBytes(usableMem) — how much memory the host is willing
//     to dedicate to keeping LLM models warm. A conservative fraction of
//     usable memory (default 0.33 — leaves ~2/3 for the OS and the
//     user's own apps), and exactly zero while Busy() so the host fully
//     yields to the user's work.
//
// Pure Go, cgo-free, cross-compile clean. Per-platform probes live in
// sample_{darwin,linux,windows,other}.go behind build tags; each is
// best-effort and degrades to "unknown" (a negative sentinel) rather
// than erroring, so a probe that can't run on some host never breaks
// the profiler — it just leans on whichever signals are available.
package sysload

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Default thresholds. The CPU / memory numbers are research-backed
// "considerate" defaults: a host is treated as busy once sustained CPU
// crosses 60% (leaving clear headroom before the user notices
// contention) or free memory drops below 25% (the point where paging
// pressure starts to hurt interactive work). The debounce windows keep
// a transient spike from yanking warm models: a condition must hold for
// ~2 min to enter Busy and the machine must be quiet for ~5 min to
// leave it. warmBudgetFracDefault (0.33) dedicates ~a third of usable
// memory to warm preload — enough for a small conservative resident set
// while leaving the majority for everything else.
const (
	sampleIntervalDefault  = 30 * time.Second
	cpuBusyPctDefault      = 60.0
	minMemAvailFracDefault = 0.25
	enterDelayDefault      = 2 * time.Minute
	exitDelayDefault       = 5 * time.Minute
	baselineFactorDefault  = 1.5
	warmBudgetFracDefault  = 0.33

	// baselineAlpha is the EWMA weight for a fresh reading folded into
	// the hour bucket. Small so the baseline reflects days of history,
	// not the last few minutes (~one day of 30s samples nudges a bucket
	// most of the way to a new steady state).
	baselineAlpha = 0.05

	// minMeaningfulCPU gates the RELATIVE (above-baseline) busy signal:
	// at 3am the baseline may be ~2%, and a 6% blip is 3× baseline but
	// not remotely "busy". We only treat above-baseline as busy once the
	// absolute figure is at least this high, so the relative signal
	// catches genuine daytime load without flapping on idle-hour noise.
	minMeaningfulCPU = 20.0

	// persistEverySamples bounds how often the learned baseline is
	// flushed to disk (every ~5 min at the default cadence) so the
	// profiler isn't doing a tiny write every single tick.
	persistEverySamples = 10
)

// Sample is one instantaneous reading, plus the debounced Busy verdict
// in effect after it was folded in. Negative fields mean "this host
// couldn't measure that signal" (e.g. no load average on Windows).
type Sample struct {
	At           time.Time `json:"at"`
	CPUPercent   float64   `json:"cpu_percent"`    // effective system CPU %, 0..100; -1 unknown
	MemAvailFrac float64   `json:"mem_avail_frac"` // available/total memory, 0..1; -1 unknown
	Load1        float64   `json:"load1"`          // 1-min load average; -1 unknown
	Busy         bool      `json:"busy"`
}

// Config tunes the profiler. Zero values fall back to the package
// defaults, so `New(Config{})` is a sensible always-on profiler. The
// operator-facing knob is Frac (warm_budget_frac); the thresholds are
// exposed for tests and future config surfaces.
type Config struct {
	// Path is where the learned per-hour baseline persists across
	// restarts (typically <cacheDir>/outpost/sysload.json). Empty
	// disables persistence (the baseline is learned fresh each boot).
	Path string

	// Frac is warm_budget_frac — the fraction of usable memory the host
	// will dedicate to warm preload. <=0 or >1 falls back to 0.33.
	Frac float64

	SampleInterval  time.Duration // 0 → 30s
	CPUBusyPct      float64       // 0 → 60
	MinMemAvailFrac float64       // 0 → 0.25
	EnterDelay      time.Duration // 0 → 2m
	ExitDelay       time.Duration // 0 → 5m
	BaselineFactor  float64       // 0 → 1.5

	Logger *slog.Logger

	// now / probe are test hooks. When nil they default to time.Now and
	// the platform probeLoad(). Tests inject a fake clock + a scripted
	// probe to exercise the debounce + baseline logic deterministically.
	now   func() time.Time
	probe func() (cpuPct, memAvailFrac, load1 float64)
}

// Profiler samples system load, learns the daily baseline, and answers
// Busy() / WarmBudgetBytes(). Safe for concurrent use: Run() is the
// single writer; Current()/Busy()/WarmBudgetBytes() read under the mutex.
type Profiler struct {
	cfg Config
	log *slog.Logger

	mu   sync.Mutex
	cur  Sample
	busy bool

	// condSince/lastRaw drive the debounce: condSince is when the raw
	// (undebounced) busy condition last flipped, lastRaw is that raw
	// value. busy only flips once the condition has held past the
	// enter/exit delay.
	condSince time.Time
	lastRaw   bool
	seeded    bool

	base    *baseline
	sampleN int
}

// New builds a Profiler, loading any persisted baseline from cfg.Path.
func New(cfg Config) *Profiler {
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = sampleIntervalDefault
	}
	if cfg.CPUBusyPct <= 0 {
		cfg.CPUBusyPct = cpuBusyPctDefault
	}
	if cfg.MinMemAvailFrac <= 0 {
		cfg.MinMemAvailFrac = minMemAvailFracDefault
	}
	if cfg.EnterDelay <= 0 {
		cfg.EnterDelay = enterDelayDefault
	}
	if cfg.ExitDelay <= 0 {
		cfg.ExitDelay = exitDelayDefault
	}
	if cfg.BaselineFactor <= 0 {
		cfg.BaselineFactor = baselineFactorDefault
	}
	if cfg.Frac <= 0 || cfg.Frac > 1 {
		cfg.Frac = warmBudgetFracDefault
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.probe == nil {
		cfg.probe = probeLoad
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	p := &Profiler{cfg: cfg, log: log, base: loadBaseline(cfg.Path)}
	return p
}

// Run samples immediately, then on every SampleInterval, until ctx is
// cancelled. Always returns nil (a slow load probe is never fatal).
func (p *Profiler) Run(ctx context.Context) error {
	p.sampleOnce()
	t := time.NewTicker(p.cfg.SampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.persist()
			return nil
		case <-t.C:
			p.sampleOnce()
		}
	}
}

// sampleOnce probes the host, folds the reading into the hour baseline,
// updates the debounced Busy verdict, and periodically persists.
func (p *Profiler) sampleOnce() {
	now := p.cfg.now()
	cpuRaw, memFrac, load1 := p.cfg.probe()

	// Effective CPU: prefer a real system-wide CPU %; when the platform
	// can't measure it (e.g. macOS without cgo) fall back to a
	// load-average estimate so the busy logic still has a signal.
	cpu := cpuRaw
	if cpu < 0 {
		cpu = loadToPct(load1)
	}

	// Learn the baseline from the effective CPU (skip unknown readings so
	// a signal-less host doesn't poison the buckets with zeros).
	if cpu >= 0 {
		p.base.update(now.Hour(), cpu)
	}

	raw := p.rawBusy(cpu, memFrac, now)

	p.mu.Lock()
	if !p.seeded {
		p.condSince = now
		p.lastRaw = raw
		p.seeded = true
	} else if raw != p.lastRaw {
		p.condSince = now
		p.lastRaw = raw
	}
	held := now.Sub(p.condSince)
	if raw && !p.busy && held >= p.cfg.EnterDelay {
		p.busy = true
		p.log.Info("sysload: entering busy (yielding warm budget)", "cpu_pct", cpu, "mem_avail_frac", memFrac)
	} else if !raw && p.busy && held >= p.cfg.ExitDelay {
		p.busy = false
		p.log.Info("sysload: leaving busy (restoring warm budget)", "cpu_pct", cpu, "mem_avail_frac", memFrac)
	}
	p.cur = Sample{At: now, CPUPercent: cpu, MemAvailFrac: memFrac, Load1: load1, Busy: p.busy}
	p.mu.Unlock()

	p.sampleN++
	if p.sampleN%persistEverySamples == 0 {
		p.persist()
	}
}

// rawBusy is the undebounced verdict for one reading: busy when memory
// is critically low, when absolute CPU crosses the safety threshold, or
// when CPU is meaningfully above this hour's learned baseline.
func (p *Profiler) rawBusy(cpu, memFrac float64, now time.Time) bool {
	// Absolute memory safety: paging pressure hurts the user's work.
	if memFrac >= 0 && memFrac < p.cfg.MinMemAvailFrac {
		return true
	}
	if cpu < 0 {
		return false // no CPU signal at all — don't guess busy
	}
	// Absolute CPU safety.
	if cpu > p.cfg.CPUBusyPct {
		return true
	}
	// Relative: above the learned baseline for this hour, but only once
	// the absolute figure is high enough to matter.
	if cpu >= minMeaningfulCPU {
		if base := p.base.get(now.Hour()); base > 0 && cpu > base*p.cfg.BaselineFactor {
			return true
		}
	}
	return false
}

// Current returns the most recent sample. Zero-value At means the
// profiler hasn't sampled yet.
func (p *Profiler) Current() Sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// Busy reports the current debounced busy verdict.
func (p *Profiler) Busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busy
}

// WarmBudgetFrac returns the configured warm-budget fraction.
func (p *Profiler) WarmBudgetFrac() float64 { return p.cfg.Frac }

// WarmBudgetBytes is the conservative memory the host will dedicate to
// warm preload: Frac × usableMem, or exactly 0 while Busy() (full
// yield). usableMem is the host's usable memory in bytes (typically
// total physical RAM; on unified-memory GPUs that's the shared pool).
func (p *Profiler) WarmBudgetBytes(usableMem uint64) int64 {
	if p.Busy() {
		return 0
	}
	b := float64(usableMem) * p.cfg.Frac
	if b <= 0 {
		return 0
	}
	return int64(b)
}

// persist flushes the learned baseline to cfg.Path (atomic temp+rename).
// Best-effort — a write failure is logged at debug and never fatal.
func (p *Profiler) persist() {
	if p.cfg.Path == "" {
		return
	}
	snap := p.base.snapshot()
	snap.UpdatedAt = p.cfg.now().UTC()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.cfg.Path), 0o755); err != nil {
		p.log.Debug("sysload: mkdir for baseline failed", "err", err)
		return
	}
	tmp := p.cfg.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		p.log.Debug("sysload: write baseline failed", "err", err)
		return
	}
	if err := os.Rename(tmp, p.cfg.Path); err != nil {
		p.log.Debug("sysload: rename baseline failed", "err", err)
		_ = os.Remove(tmp)
	}
}

// loadToPct maps a 1-minute load average onto a rough system CPU
// percentage (load per core, capped at 100). Returns -1 when the load
// average is unavailable.
func loadToPct(load1 float64) float64 {
	if load1 < 0 {
		return -1
	}
	n := runtime.NumCPU()
	if n <= 0 {
		n = 1
	}
	pct := load1 / float64(n) * 100
	if pct > 100 {
		pct = 100
	}
	return pct
}
