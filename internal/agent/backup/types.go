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

import "time"

// Candidate is one (folder, file, content-hash) triple a worker fire
// produced. Written as a single JSONL line to the ledger. The
// content-hash is what enables resume + dedup across fires: a folder
// that hasn't grown a new file since the last fire emits a fresh
// ledger line with the same SHA256 and the Phase 3 pusher will
// recognize it as already-shipped.
type Candidate struct {
	At      time.Time `json:"at"`
	Folder  string    `json:"folder"`
	Path    string    `json:"path"`            // absolute path of the picked file
	SHA256  string    `json:"sha256"`          // hex-encoded content hash
	Size    int64     `json:"size"`            // bytes
	Mtime   time.Time `json:"mtime"`           // file modtime, UTC
	Skipped bool      `json:"skipped,omitempty"` // true when SHA matched the previous candidate (no-op fire)
	Error   string    `json:"error,omitempty"`
}
