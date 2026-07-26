//go:build windows

package localsock

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialPipe dials a Windows named pipe. winio takes a timeout rather than
// a context, so derive one from the deadline when the caller set one.
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	var timeout time.Duration
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
		if timeout <= 0 {
			return nil, fmt.Errorf("localsock: dial pipe %q: deadline already passed", path)
		}
	} else {
		timeout = 10 * time.Second
	}
	return winio.DialPipe(path, &timeout)
}

// statOK gates Probe before it spends a dial. Named pipes are not
// stat-able as files, so they always pass through to the dial. For an
// AF_UNIX socket, Windows reports a REGULAR FILE — there is no
// os.ModeSocket bit to assert, unlike on unix — so mere existence is the
// most we can check here.
func statOK(path string) bool {
	if IsPipe(path) {
		return true
	}
	_, err := os.Stat(path)
	return err == nil
}

// ListPipes returns the dialable paths of every named pipe whose name
// starts with prefix, newest-first is NOT meaningful here so the result
// is sorted for determinism.
//
// Enumerating the pipe namespace is what lets podman-socket detection
// work with an OPERATOR-CHOSEN machine name: podman publishes
// `\\.\pipe\podman-<machine>`, and `<machine>` is whatever the operator
// (or bashy) named the VM — "podman-machine-default" is only the
// upstream default. A glob can't help because filepath.Glob cannot walk
// the pipe namespace; a directory read can.
func ListPipes(prefix string) []string {
	entries, err := os.ReadDir(PipePrefix)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if prefix != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			continue
		}
		out = append(out, PipePath(name))
	}
	sort.Strings(out)
	return out
}
