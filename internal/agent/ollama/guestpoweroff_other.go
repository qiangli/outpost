//go:build !linux

package ollama

import (
	"fmt"
	"runtime"
)

// PowerOffHost is Linux-only: provisioned GPU workers are Linux instances.
// Elsewhere it fails loud rather than pretending.
func PowerOffHost() error {
	return fmt.Errorf("gpu worker power-off is not supported on %s", runtime.GOOS)
}
