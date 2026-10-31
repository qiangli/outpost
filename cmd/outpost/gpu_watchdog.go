package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/qiangli/outpost/internal/agent/ollama"
)

// newGPUWorkerWatchdog builds the provisioned GPU worker's own idle/session
// stop, or nil on every ordinary host (no $OUTPOST_GPU_WORKER_REQUEST_ID).
// It is deliberately independent of the master's keep-alive stop and of the
// pool unit: activity is probed directly (/api/ps on the proxy's target, or
// the default daemon, plus the proxy's in-flight counter), so a disabled
// pool or a lost control plane still ends in a stopped GPU.
func newGPUWorkerWatchdog(ollamaCap *ollamaCapability, pool *ollamaPoolCapability) *ollama.GuestWatchdog {
	reqID := ollama.WorkerRequestID()
	if reqID == "" {
		return nil
	}
	idle, maxSession, warnings := ollama.GuestWatchdogDurations()
	for _, w := range warnings {
		slog.Warn("gpu worker watchdog: " + w)
	}
	return ollama.NewGuestWatchdog(ollama.GuestWatchdogConfig{
		RequestID:  reqID,
		Idle:       idle,
		MaxSession: maxSession,
		Active: func(ctx context.Context) (bool, error) {
			target := ""
			if ollamaCap != nil {
				target = ollamaCap.Target()
				if svc := ollamaCap.Service(); svc != nil && svc.Counter().Snapshot().InFlight > 0 {
					return true, nil
				}
			}
			if target == "" {
				target = ollama.DefaultURL
			}
			names, _, err := ollama.ProbeLoaded(ctx, nil, target)
			return len(names) > 0, err
		},
		FinalPush: func(ctx context.Context, _ string) error {
			if pool == nil {
				return errors.New("ollama pool not wired")
			}
			return pool.PushNow(ctx)
		},
	})
}
