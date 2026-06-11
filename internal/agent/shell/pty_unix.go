//go:build !windows

package shell

import (
	"fmt"
	"os"

	"github.com/creack/pty"
)

// openPTY allocates a master+slave PTY pair. The runner reads/writes the
// slave; the caller reads/writes the master.
func openPTY() (ptyFile, ptyFile, error) {
	ptm, pts, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	return ptm, pts, nil
}

// setPTYSize translates browser cols/rows into a TIOCSWINSZ on the master.
// Inside the runner this fires SIGWINCH and updates $COLUMNS / $LINES.
func setPTYSize(master ptyFile, cols, rows uint16) error {
	f, ok := master.(*os.File)
	if !ok {
		return fmt.Errorf("setPTYSize: not a PTY file (%T)", master)
	}
	return pty.Setsize(f, &pty.Winsize{Cols: cols, Rows: rows})
}
