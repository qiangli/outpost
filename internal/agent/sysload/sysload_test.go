package sysload

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// scriptedProbe returns whatever the pointed-to values currently hold,
// so a test can change the "measurement" between samples.
func scriptedProbe(cpu, mem, load *float64) func() (float64, float64, float64) {
	return func() (float64, float64, float64) { return *cpu, *mem, *load }
}

func newTestProfiler(t *testing.T, cpu, mem, load *float64, clk *time.Time) *Profiler {
	t.Helper()
	return New(Config{
		Frac:            0.33,
		SampleInterval:  30 * time.Second,
		CPUBusyPct:      60,
		MinMemAvailFrac: 0.25,
		EnterDelay:      2 * time.Minute,
		ExitDelay:       5 * time.Minute,
		BaselineFactor:  1.5,
		now:             func() time.Time { return *clk },
		probe:           scriptedProbe(cpu, mem, load),
	})
}

func TestBusyDebounceEnterAndExit(t *testing.T) {
	cpu, mem, load := 10.0, 0.9, 1.0
	clk := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)
	p := newTestProfiler(t, &cpu, &mem, &load, &clk)

	// Baseline quiet: not busy.
	p.sampleOnce()
	if p.Busy() {
		t.Fatal("should not be busy on first quiet sample")
	}

	// CPU jumps well over the absolute threshold, but debounce holds:
	// must sustain ~2 min before entering Busy.
	cpu = 85.0
	p.sampleOnce() // t=0 of the busy condition
	if p.Busy() {
		t.Fatal("should not enter busy immediately (debounce)")
	}
	clk = clk.Add(90 * time.Second)
	p.sampleOnce()
	if p.Busy() {
		t.Fatal("should still be within enter-debounce at 90s")
	}
	clk = clk.Add(60 * time.Second) // total 150s > 2m
	p.sampleOnce()
	if !p.Busy() {
		t.Fatal("should have entered busy after sustained >2m high CPU")
	}

	// Load drops; must be quiet ~5 min before leaving Busy.
	cpu = 5.0
	clk = clk.Add(30 * time.Second)
	p.sampleOnce() // t=0 of the idle condition
	if !p.Busy() {
		t.Fatal("should stay busy immediately after load drops (exit debounce)")
	}
	clk = clk.Add(4 * time.Minute)
	p.sampleOnce()
	if !p.Busy() {
		t.Fatal("should still be busy within exit-debounce at 4m idle")
	}
	clk = clk.Add(90 * time.Second) // total 5m30s > 5m
	p.sampleOnce()
	if p.Busy() {
		t.Fatal("should have left busy after sustained >5m idle")
	}
}

func TestBusyOnLowMemory(t *testing.T) {
	cpu, mem, load := 5.0, 0.5, 0.2
	clk := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	p := newTestProfiler(t, &cpu, &mem, &load, &clk)
	p.sampleOnce()
	if p.Busy() {
		t.Fatal("healthy memory should not be busy")
	}
	// Available memory drops below 25%: raw-busy, but debounced.
	mem = 0.10
	p.sampleOnce()
	if p.Busy() {
		t.Fatal("should debounce the memory-pressure signal")
	}
	clk = clk.Add(3 * time.Minute)
	p.sampleOnce()
	if !p.Busy() {
		t.Fatal("should be busy after sustained low memory")
	}
}

func TestWarmBudgetBytes(t *testing.T) {
	cpu, mem, load := 5.0, 0.9, 0.1
	clk := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	p := newTestProfiler(t, &cpu, &mem, &load, &clk)
	p.sampleOnce()

	const gib = uint64(1) << 30
	total := 32 * gib
	got := p.WarmBudgetBytes(total)
	want := int64(float64(total) * 0.33)
	if got != want {
		t.Fatalf("idle warm budget = %d, want %d", got, want)
	}
	// ~10 GB on a 32 GB host — the documented conservative default.
	if got < 10*int64(gib) || got > 11*int64(gib) {
		t.Fatalf("expected ~10-11 GiB on a 32 GiB host, got %d bytes", got)
	}

	// Force busy → budget must be exactly 0 (full yield).
	cpu = 95.0
	p.sampleOnce()
	clk = clk.Add(3 * time.Minute)
	p.sampleOnce()
	if !p.Busy() {
		t.Fatal("precondition: profiler should be busy")
	}
	if b := p.WarmBudgetBytes(total); b != 0 {
		t.Fatalf("busy warm budget = %d, want 0", b)
	}
}

func TestBaselineRelativeBusy(t *testing.T) {
	// Learn a low baseline for this hour, then a modest-but-well-above-
	// baseline spike should read busy even though it's under 60%.
	cpu, mem, load := 25.0, 0.9, 0.5
	clk := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	p := newTestProfiler(t, &cpu, &mem, &load, &clk)
	// Fold in many quiet-hour samples to establish a ~25% baseline.
	for i := 0; i < 50; i++ {
		p.sampleOnce()
		clk = clk.Add(30 * time.Second)
	}
	if base := p.base.get(10); base < 20 || base > 30 {
		t.Fatalf("learned baseline = %.1f, expected ~25", base)
	}
	// 45% is < 60% absolute but > 1.5× the 25% baseline → raw busy.
	cpu = 45.0
	if !p.rawBusy(cpu, 0.9, clk) {
		t.Fatal("45%% CPU vs a 25%% baseline should be raw-busy")
	}
	// 30% is above baseline but under 1.5× and modest → not busy.
	if p.rawBusy(30.0, 0.9, clk) {
		t.Fatal("30%% CPU vs a 25%% baseline should not be busy")
	}
}

func TestBaselinePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysload.json")
	cpu, mem, load := 40.0, 0.9, 0.5
	clk := time.Date(2026, 1, 2, 14, 0, 0, 0, time.UTC)
	p := New(Config{
		Path:  path,
		Frac:  0.33,
		now:   func() time.Time { return clk },
		probe: scriptedProbe(&cpu, &mem, &load),
	})
	for i := 0; i < 5; i++ {
		p.sampleOnce()
		clk = clk.Add(30 * time.Second)
	}
	p.persist()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("baseline file not written: %v", err)
	}

	// Reload: the learned bucket for hour 14 should carry over.
	p2 := New(Config{Path: path, now: func() time.Time { return clk }, probe: scriptedProbe(&cpu, &mem, &load)})
	if base := p2.base.get(14); base < 35 || base > 45 {
		t.Fatalf("reloaded baseline = %.1f, expected ~40", base)
	}
}

func TestLoadToPct(t *testing.T) {
	if loadToPct(-1) != -1 {
		t.Fatal("unknown load must map to -1")
	}
	if p := loadToPct(0); p != 0 {
		t.Fatalf("zero load → 0%%, got %v", p)
	}
	// Never exceed 100% regardless of core count.
	if p := loadToPct(9999); p != 100 {
		t.Fatalf("huge load must cap at 100, got %v", p)
	}
}
