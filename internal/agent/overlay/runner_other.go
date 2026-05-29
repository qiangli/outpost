//go:build !linux

package overlay

import "context"

// Run on non-Linux platforms is a stub. tailscaled + kernel WireGuard
// integration is best-effort outside Linux for our CNI use case;
// macOS/Windows callers branch on ErrUnsupported and continue without
// the overlay (single-node clusters keep working).
func Run(_ context.Context, _ Options) error {
	return ErrUnsupported
}
