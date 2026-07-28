package vknode

import (
	"io"
	"strings"
)

// TerminationLogTailAnnotation opts a one-container virtual-node Pod into
// copying the bounded tail of its combined stdout/stderr into
// ContainerStateTerminated.Message. DKS jobs use this as their small,
// apiserver-visible result channel; larger diagnostics belong in an artifact
// store. Opt-in avoids exposing arbitrary workload logs through Pod status.
const TerminationLogTailAnnotation = "outpost.dhnt.io/termination-log-tail"

// maxTerminationMessageBytes matches the kubelet termination-message budget.
const maxTerminationMessageBytes = 4 * 1024

func terminationLogTail(r io.Reader) string {
	if r == nil {
		return ""
	}
	buf := make([]byte, maxTerminationMessageBytes)
	var total int64
	scratch := make([]byte, 32*1024)
	for {
		n, err := r.Read(scratch)
		for i := 0; i < n; i++ {
			buf[int(total%int64(len(buf)))] = scratch[i]
			total++
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return ""
		}
	}
	n := int(total)
	if n == 0 {
		return ""
	}
	if n > len(buf) {
		n = len(buf)
	}
	start := 0
	if total > int64(len(buf)) {
		start = int(total % int64(len(buf)))
	}
	out := make([]byte, 0, n)
	take := len(buf) - start
	if take > n {
		take = n
	}
	out = append(out, buf[start:start+take]...)
	if len(out) < n {
		out = append(out, buf[:n-len(out)]...)
	}
	msg := strings.ToValidUTF8(string(out), "")
	return strings.ReplaceAll(msg, "\x00", "\uFFFD")
}
