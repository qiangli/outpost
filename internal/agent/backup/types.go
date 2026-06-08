// Package backup is the outpost-side, app-opaque folder watcher that
// produces backup candidates on a cron schedule. It does NOT snapshot
// app internals — the cooperating app (classgo's nightly ZIP under
// ~/.classgo/data/backups/, kg's exported graph, …) is responsible
// for putting backup artifacts into a folder we watch. On each fire
// the worker scans every configured folder, picks the newest regular
// file by mtime, computes its sha256, and (if new) records a
// Candidate entry in the local JSONL ledger.
//
// Phase 2 (this commit) only records candidates and serves the admin
// UI's "what's pending backup right now" view. Phase 3 will read the
// pending candidates and ship them to peer outposts via cloudbox
// (age-encrypted, chunked, sha256-verified). The Candidate record is
// the contract between the two phases.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// sha256Sum is a tiny convenience so callers don't import crypto/sha256
// + encoding/hex everywhere. Returns the hex digest.
func sha256Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Candidate is one (folder, file, content-hash) triple a worker fire
// produced. Written as a single JSONL line to the ledger. The
// content-hash is what enables resume + dedup across fires: a folder
// that hasn't grown a new file since the last fire emits a fresh
// ledger line with the same SHA256 and the pusher will recognize it
// as already-shipped.
type Candidate struct {
	At      time.Time `json:"at"`
	Folder  string    `json:"folder"`
	Path    string    `json:"path"`            // absolute path of the picked file
	SHA256  string    `json:"sha256"`          // hex-encoded content hash
	Size    int64     `json:"size"`            // bytes
	Mtime   time.Time `json:"mtime"`           // file modtime, UTC
	Skipped bool      `json:"skipped,omitempty"` // true when SHA matched the previous candidate (no-op fire)
	Error   string    `json:"error,omitempty"`

	// Push status — populated when the manager has a Pusher wired
	// (cloudbox URL + access token present). Empty Pushed means the
	// candidate didn't go through a push attempt (worker only).
	Pushed       bool   `json:"pushed,omitempty"`
	ArtifactID   string `json:"artifact_id,omitempty"`
	CipherSHA256 string `json:"cipher_sha256,omitempty"`
	PushError    string `json:"push_error,omitempty"`
}

// shortSHA returns the first 16 hex chars of sha256(s). Used both as
// the artifact KeyID fingerprint (keys.go) and as a stable tag in
// log messages.
func shortSHA(s string) string {
	sum := sha256Sum([]byte(s))
	return sum[:16]
}
