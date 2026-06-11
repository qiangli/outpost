//go:build windows

package shell

import "errors"

// openPTY hands out the pipe-backed virtual pair (see vpty.go) —
// Windows has no kernel PTY usable by the in-process runner. Matches
// the unix signature so NewSession is platform-agnostic.
func openPTY() (ptyFile, ptyFile, error) {
	master, slave, err := openVPTY()
	if err != nil {
		return nil, nil, err
	}
	return master, slave, nil
}

// setPTYSize handles real-PTY masters only; every Windows session is a
// virtual pair, which sessionSetSize (runner.go) intercepts first.
func setPTYSize(master ptyFile, cols, rows uint16) error {
	return errors.New("setPTYSize: no kernel PTY on windows")
}
