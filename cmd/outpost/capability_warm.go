package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/sysinfo"
	"github.com/qiangli/outpost/internal/agent/sysload"
	"github.com/qiangli/outpost/internal/agent/warm"
)

// The considerate warm-serving plane as an in-process capability (unit
// `provider: internal`, `owner: outpost`, `capability: outpost.warm`).
//
// It keeps a small set of models resident so inference has no cold start,
// but YIELDS — unloads them — the moment the host is busy with the user's
// own work, and restores when idle. The judgement of "busy" comes from the
// sysload profiler, which is a SENSOR shared with the cluster placement
// path, so the capability borrows it from the host rather than owning it.
//
// `settings.budget_frac` is the fraction of usable memory dedicated to warm
// preload (the old warm_budget_frac). The desired warm SET is not a setting:
// it is runtime state cloudbox drives through POST /admin/warm and the
// executor persists, which is why it stays in the daemon's own config file
// next to the other daemon-internal state.
type warmCapability struct {
	// Load hands out the host's shared sysload profiler, starting it on
	// first use.
	Load func() *sysload.Profiler
	// Ollama resolves the local inference daemon the executor drives.
	Ollama func() string
	// Shard is the optional shard-control half: a model too big for one box
	// is warmed by forming a shard instead of loading locally.
	Shard func() warm.ShardControl
	// PersistDesired persists the updated desired set (threaded through
	// admincore so writes serialize against the config mutex).
	PersistDesired func([]string) error
	// Desired seeds the persisted desired set at activation.
	Desired func() []string
	// Paired reports whether this host has cloudbox credentials. Warm
	// serving is cloudbox-driven, so an unpaired host has nothing to serve.
	Paired func() bool

	idleState
	run goroutineRunner

	mu   sync.Mutex
	exec *warm.Executor
	prof *sysload.Profiler
}

// Executor is the live executor the /admin/warm route dispatches to, nil
// while the unit is not active.
func (c *warmCapability) Executor() *warm.Executor {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exec
}

// Status is the warm signal the pool push advertises: this host's live
// warm-preload budget (0 when busy) and its busy state. The second return
// reports whether the signal is available at all.
func (c *warmCapability) Status() (func() (int64, bool), bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	prof := c.prof
	c.mu.Unlock()
	if prof == nil {
		return nil, false
	}
	mem := sysinfo.Collect("").MemTotalBytes
	return func() (int64, bool) { return prof.WarmBudgetBytes(mem), prof.Busy() }, true
}

func (c *warmCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Paired != nil && !c.Paired() {
		c.set("warm serving", cfg, "warm serving: unavailable: this host is not paired — cloudbox is what asks a host to warm a model")
		return nil
	}
	c.clear()
	if c.Load == nil {
		return fmt.Errorf("warm serving: no load profiler on this host")
	}
	prof := c.Load()
	if prof == nil {
		return fmt.Errorf("warm serving: the load profiler could not be started")
	}
	// The declared budget fraction is the profiler's, and the profiler is
	// shared — so a changed fraction is applied to the one instance.
	frac := settingFloat(cfg, "budget_frac", sysload.WarmBudgetFracDefault)
	if frac <= 0 || frac > 1 {
		return fmt.Errorf("warm serving: settings.budget_frac must be in the range (0, 1], got %v", frac)
	}
	prof.SetWarmBudgetFrac(frac)

	target := defaultOllamaURL
	if c.Ollama != nil {
		if t := c.Ollama(); t != "" {
			target = t
		}
	}
	var shardCtl warm.ShardControl
	if c.Shard != nil {
		// Resolved through the closure so a nil *Manager never becomes a
		// non-nil interface value.
		shardCtl = c.Shard()
	}
	var desired []string
	if c.Desired != nil {
		desired = c.Desired()
	}
	mem := sysinfo.Collect("").MemTotalBytes
	exec := warm.New(warm.Config{
		Ollama:         warm.NewOllamaClient(target),
		Shard:          shardCtl,
		Gauge:          prof,
		UsableMem:      func() uint64 { return mem },
		Desired:        desired,
		PersistDesired: c.PersistDesired,
	})
	c.mu.Lock()
	c.exec, c.prof = exec, prof
	c.mu.Unlock()
	c.run.start(ctx, func(runCtx context.Context) error {
		if err := exec.RunSupervisor(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
	slog.Info("warm serving: supervising the considerate warm set", "target", target, "budget_frac", frac, "models", len(desired))
	return nil
}

// Configure rebuilds the executor (its target, budget and desired set are
// read once) — the supervisor is cheap to cycle and holds no connection.
func (c *warmCapability) Configure(ctx context.Context, cfg service.Config) error {
	return c.Activate(ctx, cfg)
}

func (c *warmCapability) Deactivate(ctx context.Context) error {
	c.run.stop(ctx)
	c.mu.Lock()
	c.exec, c.prof = nil, nil
	c.mu.Unlock()
	return nil
}

func (c *warmCapability) Health(ctx context.Context) error {
	if cfg, idle := c.pending(); idle {
		return c.Activate(ctx, cfg)
	}
	return c.run.health("warm serving")
}

// sysloadPlane owns the host's single load profiler: the CPU/memory/load
// sampler that learns a per-hour-of-day baseline and answers Busy() /
// WarmBudgetBytes().
//
// It is a sensor, not a service: two consumers need it for different
// reasons (the warm capability, and cluster placement for virtual
// runtimes), and running two samplers would mean two writers of one state
// file. So it is created and started ONCE, on first use, and both ask for
// it through Profiler().
type sysloadPlane struct {
	// ctx bounds the sampler. Set by the daemon to its errgroup context.
	ctx context.Context

	mu   sync.Mutex
	prof *sysload.Profiler
}

// Profiler returns the host's profiler, starting the sampler on first call.
func (p *sysloadPlane) Profiler() *sysload.Profiler {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prof != nil {
		return p.prof
	}
	var path string
	if cd, _ := conf.ResolveCacheDir(); cd != "" {
		path = filepath.Join(cd, "sysload.json")
	}
	p.prof = sysload.New(sysload.Config{Path: path, Frac: sysload.WarmBudgetFracDefault})
	ctx := p.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func(prof *sysload.Profiler) {
		if err := prof.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("sysload: profiler exited", "err", err)
		}
	}(p.prof)
	return p.prof
}

// Current is the last sample, zero when no consumer has asked for the
// profiler yet. Read-only: it never starts the sampler, so a caller that
// only reports load does not turn it on.
func (p *sysloadPlane) Current() (sysload.Sample, bool) {
	if p == nil {
		return sysload.Sample{}, false
	}
	p.mu.Lock()
	prof := p.prof
	p.mu.Unlock()
	if prof == nil {
		return sysload.Sample{}, false
	}
	return prof.Current(), true
}
