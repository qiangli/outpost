//go:build !linux

package k3sagent

import "context"

// Run on non-Linux platforms is a stub. k3s agent (and its bundled
// kubelet + containerd) require Linux. Callers should branch on
// ErrUnsupported and fall back to vkpodman or surface a clear
// "use a Linux VM" message to the operator.
func Run(_ context.Context, _ Options) error {
	return ErrUnsupported
}
