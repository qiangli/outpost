//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stopStalePrimaForInstall reaps ranks left by a previous daemon before their
// executable is opened for replacement. The image path is checked on the open
// process handle, so a recycled PID or an unrelated prima process is untouched.
func stopStalePrimaForInstall(binPath string) error {
	target, err := filepath.Abs(binPath)
	if err != nil {
		return err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil
		}
		return err
	}
	for {
		// The snapshot's basename is only a cheap filter. It is never enough
		// authority to terminate a process.
		name := windows.UTF16ToString(entry.ExeFile[:])
		if strings.EqualFold(name, filepath.Base(target)) && entry.ProcessID != uint32(os.Getpid()) {
			if err := stopMatchingPrimaProcess(entry.ProcessID, target); err != nil {
				return err
			}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return nil
			}
			return err
		}
	}
}

func stopMatchingPrimaProcess(pid uint32, target string) error {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil // exited between the snapshot and OpenProcess
	}
	if err != nil {
		return fmt.Errorf("inspect candidate pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	name := make([]uint16, 32768)
	size := uint32(len(name))
	if err := windows.QueryFullProcessImageName(h, 0, &name[0], &size); err != nil {
		return fmt.Errorf("identify candidate pid %d: %w", pid, err)
	}
	if !strings.EqualFold(filepath.Clean(windows.UTF16ToString(name[:size])), filepath.Clean(target)) {
		return nil
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return fmt.Errorf("check prima pid %d: %w", pid, err)
	}
	if code != stillActive {
		return nil
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("stop prima pid %d: %w", pid, err)
	}
	state, err := windows.WaitForSingleObject(h, 5000)
	if err != nil {
		return fmt.Errorf("wait for prima pid %d: %w", pid, err)
	}
	if state != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("prima pid %d did not exit (wait state %d)", pid, state)
	}
	return nil
}
