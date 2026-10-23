package svcunit

import "errors"

// DegradedError marks a capability condition that is NOT a failure: the
// capability serves, but with a known limitation the operator must hear
// about (the sshd unit's missing Windows firewall rule, which leaves the
// listener reachable only where the OS default already allows it).
//
// The reconciler maps it to state Degraded with the message as the reason —
// published, not withdrawn (Degraded is an exposed state), and NOT cycled:
// tearing the listener down and rebinding every pass would flap a serving
// capability for a condition a restart cannot fix. Any other error keeps
// the existing failed/cycle behavior.
type DegradedError struct{ Reason string }

func (e *DegradedError) Error() string { return e.Reason }

// NewDegraded reports a serving-but-limited condition.
func NewDegraded(reason string) *DegradedError { return &DegradedError{Reason: reason} }

// isDegraded reports whether err (or anything it wraps) is a degraded
// condition rather than a failure.
func isDegraded(err error) bool {
	var deg *DegradedError
	return errors.As(err, &deg)
}
