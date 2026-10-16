package runtime

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// PlaneHealth is what this host knows about whether the control plane its
// worker joined is reachable. Two existing signals feed it: the runtime
// container's own log stream (the CA fetch and frpc errors the entrypoint
// already prints, read by TailLogs) and, when the apiserver visitor is
// published on host loopback (APIBridgeHostPort), one ProbeAPIServer call.
// A running container says nothing about either.
type PlaneHealth struct {
	LastSuccess time.Time
	LastError   string
	LastErrorAt time.Time
	CheckedAt   time.Time
}

// planeErrorWindow is how long a logged failure keeps the plane "unreachable"
// without a later success to contradict it.
const planeErrorWindow = 2 * time.Minute

// Reachable classifies the plane: "yes", "no" or "unknown" (no signal).
func (h PlaneHealth) Reachable(now time.Time) string {
	switch {
	case !h.LastErrorAt.IsZero() && h.LastErrorAt.After(h.LastSuccess) && now.Sub(h.LastErrorAt) < planeErrorWindow:
		return "no"
	case !h.LastSuccess.IsZero() && !h.LastErrorAt.After(h.LastSuccess):
		return "yes"
	default:
		return "unknown"
	}
}

var (
	planeMu sync.RWMutex
	plane   PlaneHealth
)

// RecordPlaneProbe records the outcome of an apiserver readiness probe.
func RecordPlaneProbe(h ServerHealth) {
	planeMu.Lock()
	defer planeMu.Unlock()
	plane.CheckedAt = h.CheckedAt
	if h.Serving {
		plane.LastSuccess = h.CheckedAt
		return
	}
	plane.LastError, plane.LastErrorAt = h.Err, h.CheckedAt
}

var planeFailureLine = regexp.MustCompile(`(?i)(ca|cacerts|certificate|frpc|visitor|apiserver|tunnel).*(fail|error|refused|timeout|timed out|unreachable|restart)`)

// RecordPlaneLogLine inspects one runtime-container log line and records it
// when it reports a plane failure. Successes are not inferred from logs.
func RecordPlaneLogLine(line string, now time.Time) {
	if !planeFailureLine.MatchString(line) {
		return
	}
	line = strings.TrimSpace(line)
	if len(line) > 200 {
		line = line[:200]
	}
	planeMu.Lock()
	plane.LastError, plane.LastErrorAt = line, now
	planeMu.Unlock()
}

// LastPlaneHealth returns the current snapshot.
func LastPlaneHealth() PlaneHealth {
	planeMu.RLock()
	defer planeMu.RUnlock()
	return plane
}

// ResetPlaneHealth clears the snapshot (tests).
func ResetPlaneHealth() {
	planeMu.Lock()
	plane = PlaneHealth{}
	planeMu.Unlock()
}
