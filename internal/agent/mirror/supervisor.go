// Package mirror supervises mobility-aware continuous directory mirrors. Each job
// mirrors a local directory to a peer's mesh service, but ONLY while that peer is
// reachable (and same-LAN when LANOnly): it PAUSES when the mirrored pair becomes
// remote/absent and RESUMES — catching up via the engine's initial full sync —
// when the pair is local again (e.g. a laptop returns to the LAN). The transfer
// engine is coreutils/pkg/mirror (recursive watch + rclone sync, all permissive);
// the mesh integration is injected as a Linker so this package stays free of the
// mesh/peerplane import graph.
package mirror

import (
	"context"
	"log/slog"
	"sync"
	"time"

	enginemirror "github.com/qiangli/coreutils/pkg/mirror"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// PollInterval is how often each job re-checks the pair's reachability/locality.
var PollInterval = 15 * time.Second

// Linker is the mesh integration the supervisor needs (implemented in main.go).
type Linker interface {
	// Reachable reports whether a peer offering service is reachable now — and,
	// when lanOnly, on the same LAN. This is the resume(true)/pause(false) signal.
	Reachable(ctx context.Context, service string, lanOnly bool) bool
	// Open resolves a peer offering service and opens a forward to it, returning
	// an rclone dest target (pointing at the local forward) + a stop func.
	Open(ctx context.Context, service string) (dest string, stop func(), err error)
}

// Supervisor runs the configured mobility-aware mirror jobs.
type Supervisor struct {
	Link   Linker
	Logger *slog.Logger
}

// Run blocks until ctx is cancelled, supervising one goroutine per job.
func (s *Supervisor) Run(ctx context.Context, jobs []conf.MirrorJob) {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		if j.Source == "" || j.Service == "" {
			continue
		}
		wg.Add(1)
		job := j
		go func() {
			defer wg.Done()
			s.runJob(ctx, job, log)
		}()
	}
	wg.Wait()
}

func (s *Supervisor) runJob(ctx context.Context, job conf.MirrorJob, log *slog.Logger) {
	t := time.NewTicker(PollInterval)
	defer t.Stop()

	running := false
	var mirrorCancel context.CancelFunc
	var stopFwd func()
	pause := func(reason string) {
		if !running {
			return
		}
		mirrorCancel()
		if stopFwd != nil {
			stopFwd()
		}
		running = false
		log.Info("mirror: paused", "reason", reason, "source", job.Source, "service", job.Service)
	}
	defer pause("shutdown")

	check := func() {
		ok := s.Link.Reachable(ctx, job.Service, job.LANOnly)
		switch {
		case ok && !running:
			dest, sf, err := s.Link.Open(ctx, job.Service)
			if err != nil {
				log.Debug("mirror: open forward failed", "service", job.Service, "err", err)
				return
			}
			mctx, mc := context.WithCancel(ctx)
			go func() {
				// Run does an initial full sync (the catch-up) then watches.
				if e := enginemirror.Run(mctx, enginemirror.Options{
					Source: job.Source, Dest: dest, Logger: log,
				}); e != nil && mctx.Err() == nil {
					log.Warn("mirror: engine exited", "source", job.Source, "err", e)
				}
			}()
			mirrorCancel, stopFwd, running = mc, sf, true
			log.Info("mirror: resumed (pair local) — catching up",
				"source", job.Source, "service", job.Service, "dest", dest)
		case !ok && running:
			pause("pair remote/away")
		}
	}

	check() // immediate, don't wait a full interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
