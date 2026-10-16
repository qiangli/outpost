//go:build windows

package main

// reexecInitImpl: Windows has no exec-in-place, so release the pidfile and
// start a detached successor; the caller then exits.
func reexecInitImpl() error {
	removeSupervisordPidFile()
	if err := spawnInit(initSelf); err != nil {
		return err
	}
	return errInitHandedOff
}
