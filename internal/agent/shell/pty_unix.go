//go:build !windows

package shell

import (
	"os"

	"github.com/creack/pty"
)

// openPTY allocates a master+slave PTY pair. The runner reads/writes the
// slave; the caller reads/writes the master.
func openPTY() (*os.File, *os.File, error) {
	return pty.Open()
}

// setPTYSize translates browser cols/rows into a TIOCSWINSZ on the master.
// Inside the runner this fires SIGWINCH and updates $COLUMNS / $LINES.
func setPTYSize(master *os.File, cols, rows uint16) error {
	return pty.Setsize(master, &pty.Winsize{Cols: cols, Rows: rows})
}
