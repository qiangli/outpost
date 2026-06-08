package telemetry

import (
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// SlogHandler returns an slog.Handler that forwards records into the
// OTEL LoggerProvider so log entries emitted with a span in their
// context automatically carry the matching trace_id + span_id.
//
// Outpost mostly uses slog throughout, so callers can wrap their
// existing handler chain with this one to get trace correlation
// without changing log emission sites.
//
// Returns nil when no LoggerProvider is configured (no-op mode);
// callers should keep their stderr handler in that case.
func (p *Provider) SlogHandler() slog.Handler {
	if p == nil || p.LoggerProvider == nil {
		return nil
	}
	return otelslog.NewHandler(DefaultServiceName, otelslog.WithLoggerProvider(p.LoggerProvider))
}
