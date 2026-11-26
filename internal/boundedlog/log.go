// Package boundedlog appends local diagnostic output to a single capped file.
// A full file starts a new generation in place. Rotation needs no scratch space.
package boundedlog

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/rogpeppe/go-internal/lockedfile"
)

const MaxBytes = 16 << 20

type Writer struct{ Path string }

func (w Writer) Write(p []byte) (int, error) { return appendBounded(w.Path, p, MaxBytes) }

func appendBounded(path string, p []byte, limit int64) (int, error) {
	if int64(len(p)) > limit {
		return 0, fmt.Errorf("log record exceeds %d bytes", limit)
	}
	// A separate stable lock file lets Windows readers tail the log during
	// normal appends. All managed writers use this lock, including CLI traces.
	unlock, err := lockedfile.MutexAt(path + ".lock").Lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if size > limit-int64(len(p)) {
		keep := min(limit/2, limit-int64(len(p)))
		tail := make([]byte, keep)
		if _, err = f.ReadAt(tail, size-keep); err != nil {
			return 0, err
		}
		// Discard the first potentially incomplete record when compacting.
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		} else {
			tail = nil
		}
		if err = f.Truncate(0); err != nil {
			return 0, err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		if _, err = f.Write(tail); err != nil {
			return 0, err
		}
	}
	return f.Write(p)
}
