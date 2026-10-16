//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether pid names a live process. Signal 0 is
// POSIX's "could I deliver a signal" no-op.
//
// This lives in a build-tagged file because the portable-looking
// os.Process.Signal form is WRONG on Windows: there, Signal returns
// EWINDOWS for anything but Kill, so a Signal(0) probe reports every
// process — including a live one — as dead. See pid_windows.go.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// processName returns the executable name of pid as ps reports it.
func processName(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// processArgs returns pid's full command line as ps reports it.
func processArgs(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// listProcesses returns every process's pid and command line.
func listProcesses() ([]procEntry, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		return nil, err
	}
	var procs []procEntry
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		procs = append(procs, procEntry{pid: pid, args: strings.Join(f[1:], " ")})
	}
	return procs, nil
}
