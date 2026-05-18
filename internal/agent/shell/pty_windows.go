//go:build windows

package shell

import (
	"errors"
	"os"
)

// errNoPTY is returned by the Windows stub. ConPTY support is a follow-up;
// the v1 Windows agent should fall back to a JS-side line editor (per the
// plan, this path is also a stub for now).
var errNoPTY = errors.New("PTY not supported on Windows v1")

func openPTY() (*os.File, *os.File, error)            { return nil, nil, errNoPTY }
func setPTYSize(_ *os.File, _ uint16, _ uint16) error { return errNoPTY }
