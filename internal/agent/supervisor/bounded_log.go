package supervisor

import (
	"fmt"
	"log/slog"
	"os"
)

// Each supervised child retains at most two generations of redacted output.
const childLogMaxBytes int64 = 8 << 20

type boundedLog struct {
	path      string
	max, size int64
	file      *os.File
	rename    func(string, string) error
	warned    bool
}

func openBoundedLog(path string, max int64) (*boundedLog, error) {
	if max <= 0 {
		return nil, fmt.Errorf("invalid log limit")
	}
	b := &boundedLog{path: path, max: max, rename: os.Rename}
	// A previous version may have left an oversized active file. Do not retain
	// unbounded legacy data, including a pre-existing oversized backup.
	if info, err := os.Stat(path + ".1"); err == nil && info.Size() > max {
		if err := os.Remove(path + ".1"); err != nil {
			return nil, err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := b.open(); err != nil {
		return nil, err
	}
	if b.size > max {
		if err := b.file.Truncate(0); err != nil {
			b.file.Close()
			return nil, err
		}
		b.size = 0
	}
	return b, nil
}

func (b *boundedLog) open() error {
	f, err := os.OpenFile(b.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	b.file, b.size = f, info.Size()
	return nil
}

func (b *boundedLog) rotate() error {
	// Close before rename/remove: Windows cannot reliably rename an open file.
	if b.file != nil {
		err := b.file.Close()
		b.file = nil
		if err != nil {
			return err
		}
	}
	if err := os.Remove(b.path + ".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := b.rename(b.path, b.path+".1"); err != nil {
		return err
	}
	return b.open()
}

// Write always consumes input, even on disk errors, so a child cannot block
// or die from a broken output pipe. Failed writes are dropped and warned once.
func (b *boundedLog) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if b.file == nil {
			if err := b.open(); err != nil {
				b.warn(err)
				break
			}
		}
		if b.size >= b.max {
			if err := b.rotate(); err != nil {
				b.warn(err)
				break
			}
			continue
		}
		count := int64(len(p))
		if count > b.max-b.size {
			count = b.max - b.size
		}
		written, err := b.file.Write(p[:int(count)])
		b.size += int64(written)
		p = p[written:]
		if err != nil {
			b.warn(err)
			break
		}
		if written == 0 {
			b.warn(fmt.Errorf("short log write"))
			break
		}
	}
	return n, nil
}
func (b *boundedLog) warn(err error) {
	if !b.warned {
		slog.Warn("supervisor: child log output dropped", "err", err)
		b.warned = true
	}
}
func (b *boundedLog) Close() error {
	if b.file == nil {
		return nil
	}
	err := b.file.Close()
	b.file = nil
	return err
}
