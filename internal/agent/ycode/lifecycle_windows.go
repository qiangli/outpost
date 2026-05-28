//go:build windows

package ycode

import (
	"os/exec"
	"syscall"
)

// platformDetach uses Windows' CREATE_NEW_PROCESS_GROUP +
// DETACHED_PROCESS flags so the spawned ycode survives outpost
// exit. SysProcAttr.CreationFlags is uint32 — both values OR'd
// together.
//
// Constants live in syscall on Windows:
//   - CREATE_NEW_PROCESS_GROUP (0x0200) — new process group;
//     console Ctrl-C in outpost doesn't propagate.
//   - DETACHED_PROCESS (0x0008) — child has no console; survives
//     parent exit.
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func platformDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
		HideWindow:    true,
	}
}

func startDetached(bin string) error {
	return spawn(bin)
}
