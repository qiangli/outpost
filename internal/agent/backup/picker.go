package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoFiles is returned by PickLatest when the directory exists but
// holds no eligible regular files. Distinct from "directory missing"
// (os.ErrNotExist) so the admin UI can render "no backups yet" vs
// "you typed a bad path."
var ErrNoFiles = errors.New("backup: directory has no eligible files")

// PickLatest returns the absolute path of the newest regular file
// directly inside dir (no recursion). Eligibility filter:
//   - regular files only (no directories, symlinks, sockets, fifos)
//   - hidden files (basename starting with ".") are skipped — they
//     are typically lock files, partial writes from the cooperating
//     app, or editor backups
//   - zero-byte files are skipped — most likely "I am writing to you
//     right now" placeholders; we want the previous complete file
//
// The tie-break for files with identical mtimes is lexicographic
// (filename ascending) so the result is deterministic across reruns
// — matters for the dedup check (same file picked twice in a row
// must produce the same Candidate.SHA256).
func PickLatest(dir string) (string, fs.FileInfo, error) {
	if strings.TrimSpace(dir) == "" {
		return "", nil, fmt.Errorf("backup: empty folder path")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, fmt.Errorf("backup: resolve %q: %w", dir, err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", nil, err
	}
	var (
		bestName string
		bestInfo fs.FileInfo
	)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// File vanished between ReadDir and Info — skip silently
			// rather than fail the whole pick.
			continue
		}
		if info.Size() == 0 {
			continue
		}
		if bestInfo == nil {
			bestName, bestInfo = name, info
			continue
		}
		if info.ModTime().After(bestInfo.ModTime()) {
			bestName, bestInfo = name, info
			continue
		}
		// Same mtime: lexicographic tie-break.
		if info.ModTime().Equal(bestInfo.ModTime()) && name < bestName {
			bestName, bestInfo = name, info
		}
	}
	if bestInfo == nil {
		return "", nil, ErrNoFiles
	}
	return filepath.Join(abs, bestName), bestInfo, nil
}
