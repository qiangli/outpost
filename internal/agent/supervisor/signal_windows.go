//go:build windows

package supervisor

import "os"

// stopSignal: Windows has no SIGTERM deliverable to a non-console child, so
// the only portable stop is Kill. (v1 gap — a graceful per-app stop command
// can come in a later pass for managed routed apps.)
func stopSignal(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
