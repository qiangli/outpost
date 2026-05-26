package upgrade

import "github.com/qiangli/outpost/internal/agent"

// ArtifactVerifier is the seam for adding signed-release-manifest
// verification later. Today the trust path is "HTTPS to cloudbox +
// sha256-in-envelope + Probe(commit-match)" — good enough while
// cloudbox itself is the root of trust for the fleet. When third-party
// release channels become a thing, an implementation will check a
// cryptographic signature (e.g. cosign / minisign / minimaCert) over
// the {commit, sha256, url} tuple before letting the worker swap the
// binary in.
//
// Verify is called after Probe succeeds but before the os.Rename. A
// non-nil error aborts the swap (the candidate file is removed; one
// ledger entry is emitted with step="verify_failed" and the cause).
//
// Default: NoopVerifier — always returns nil. The Worker.verifier
// field defaults to this when Options.Verifier is left zero, so
// today's cloudbox-as-root-of-trust model continues to work without
// callers having to opt in.
type ArtifactVerifier interface {
	Verify(env Envelope, candidatePath string, candidate agent.BuildInfo) error
}

// NoopVerifier is the zero-trust-cost default. Returns nil for every
// envelope. Replaced by a signature-checker when signed manifests
// land.
type NoopVerifier struct{}

// Verify always returns nil.
func (NoopVerifier) Verify(_ Envelope, _ string, _ agent.BuildInfo) error { return nil }
