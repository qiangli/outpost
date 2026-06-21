// Package upgrade carries the self-upgrade machinery shared by the CLI
// (`outpost upgrade`, `outpost rollback`) and the cloudbox-pushed
// daemon route (POST /admin/upgrade).
//
// The wire shape (Envelope) is what cloudbox sends. The worker
// (Worker) owns serialized in-process state — one upgrade at a time,
// last-release-id dedup, a JSONL ledger of every attempt. The Stage /
// Probe helpers are the deterministic file-level steps both the CLI
// and the worker reuse.
//
// Trust model: the daemon trusts cloudbox to push valid URLs and
// matching sha256s. The URL must be https, the sha256 must verify on
// download, and the candidate binary must self-identify as an outpost
// build via `version --json`. A future ArtifactVerifier hook (signed
// release manifests) is wired in but defaults to a no-op so the daemon
// can ship before signing infrastructure exists.
package upgrade

import (
	"errors"
	"fmt"
	"strings"
)

// Envelope is what cloudbox POSTs to /admin/upgrade. Every field
// except MinFrom is required; the daemon returns 400 if any of
// {release_id, url, sha256, commit} is empty.
type Envelope struct {
	// ReleaseID is cloudbox's opaque identifier for this artifact
	// (e.g. "v0.42.1-abc1234"). Used purely for dedup + ledger
	// correlation — the daemon doesn't parse it.
	ReleaseID string `json:"release_id"`
	// URL must be HTTPS. The daemon refuses http://, file://, or
	// anything else.
	URL string `json:"url"`
	// SHA256 of the artifact body (hex). Mandatory here even though
	// the CLI variant treats it as optional — a cloudbox push without
	// an integrity check is not acceptable.
	SHA256 string `json:"sha256"`
	// Commit is the 7-char short sha the candidate's BuildInfo must
	// expose. Probe rejects the candidate if its self-reported
	// commit doesn't match, defending against URL/SHA collisions
	// against an unrelated build.
	Commit string `json:"commit"`
	// MinFrom optionally constrains "this upgrade only applies to
	// hosts already running at least this commit." The daemon
	// returns 412 if its current commit is older. Useful when a
	// release requires migration state from a prior version.
	MinFrom string `json:"min_from,omitempty"`

	// Force bypasses the daemon's update_mode gate. Set by cloudbox
	// (or the local `outpost upgrade apply` CLI) when the OPERATOR
	// has explicitly chosen to apply this envelope right now — a
	// "manual" host that gets Force=true behaves like an "auto"
	// host for this one push. Never set by the GH-Action release
	// webhook (those are advisory, not operator-blessed). "never"
	// mode still refuses Force=true; the operator must flip the
	// mode first.
	Force bool `json:"force,omitempty"`
}

// Validate enforces non-empty required fields. Schema-level validity
// only — does not contact the network or filesystem.
func (e Envelope) Validate() error {
	if strings.TrimSpace(e.ReleaseID) == "" {
		return errors.New("release_id is required")
	}
	if strings.TrimSpace(e.URL) == "" {
		return errors.New("url is required")
	}
	if !strings.HasPrefix(strings.ToLower(e.URL), "https://") {
		return errors.New("url must be https://")
	}
	if strings.TrimSpace(e.SHA256) == "" {
		return errors.New("sha256 is required")
	}
	if strings.TrimSpace(e.Commit) == "" {
		return errors.New("commit is required")
	}
	return nil
}

// Status enumerates the outcomes Apply can return. The HTTP route
// layer maps these to status codes — keep the mapping in one place
// (see HTTPStatus below) so adding a new outcome doesn't require
// editing the gin handler.
type Status string

const (
	StatusAccepted      Status = "accepted"       // upgrade queued; worker goroutine running
	StatusReplay        Status = "replay"         // same release_id we just handled; idempotent no-op
	StatusInFlight      Status = "in_flight"      // another upgrade is currently running
	StatusSameCommit    Status = "same_commit"    // current daemon is already on this commit
	StatusDisabled      Status = "disabled"       // operator turned update_mode to "never"
	StatusMinFrom       Status = "min_from"       // current commit is older than envelope.min_from
	StatusPendingManual Status = "pending_manual" // envelope persisted; operator must apply via UI/CLI
	StatusQuarantined   Status = "quarantined"    // release was auto-reverted on this host; refuse re-apply
)

// HTTPStatus maps an Apply outcome to the wire HTTP status. 202
// Accepted covers both "work is happening async" (StatusAccepted)
// and "envelope was persisted, awaiting operator" (StatusPendingManual);
// the body's status field disambiguates. The rest are terminal
// refusals with a human-readable reason.
func (s Status) HTTPStatus() int {
	switch s {
	case StatusAccepted, StatusPendingManual:
		return 202
	case StatusReplay:
		return 200
	case StatusInFlight:
		return 409
	case StatusSameCommit:
		return 304
	case StatusDisabled:
		return 403
	case StatusMinFrom:
		return 412
	case StatusQuarantined:
		return 409
	}
	return 500
}

// Result is what Apply returns over the wire. Status carries the
// outcome; Detail is a short human-readable explanation; Commit (when
// set) lets cloudbox correlate the response against its release
// metadata.
type Result struct {
	Status    Status `json:"status"`
	Detail    string `json:"detail,omitempty"`
	ReleaseID string `json:"release_id,omitempty"`
	Commit    string `json:"commit,omitempty"`
}

// ErrShortCommit is returned by the Probe step when the candidate's
// self-reported commit doesn't match the envelope. This is the last
// defense against URL/SHA collisions or operator typos — we refuse
// to swap a binary that doesn't identify as the release we expected.
var ErrShortCommit = errors.New("candidate binary does not self-report the expected commit")

// ErrPlatformMismatch is returned by Probe when the candidate's
// self-reported os/arch (its compile-time runtime.GOOS/GOARCH, in
// version --json) doesn't match the running host's. This is the
// definitive cross-platform guard: it rejects a wrong-platform binary
// even when the arch can still EXEC (e.g. a darwin-amd64 build running
// under Rosetta on darwin-arm64), which the incidental "exec format
// error" check cannot. Both sides are compile-time-baked truth, so a
// genuine binary can never misreport.
var ErrPlatformMismatch = errors.New("candidate binary is built for a different platform than this host")

func shortCommit(full string) string {
	if len(full) > 7 {
		return full[:7]
	}
	return full
}

// envelopeMismatch builds the diagnostic shown when the envelope's
// Commit doesn't match the probed binary. The full sha is included
// so an operator reading the ledger can correlate.
func envelopeMismatch(want, got string) error {
	return fmt.Errorf("%w: envelope said %s, candidate says %s", ErrShortCommit, want, got)
}
