package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// localDaemonPresence distinguishes an absent daemon from a running but
// inaccessible daemon. Only an explicit refused connection proves absence;
// timeout, permission failure and malformed PID state remain errors.
func localDaemonPresence() (bool, string, error) {
	addr := daemonAdminAddr()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, addr, err
	}
	if host == "0.0.0.0" {
		_, port, _ := net.SplitHostPort(addr)
		addr = net.JoinHostPort("127.0.0.1", port)
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false, addr, errors.New("installer presence probe requires a loopback admin address")
	}
	pidPath, err := pidFilePath()
	if err != nil {
		return false, addr, err
	}
	data, err := os.ReadFile(pidPath)
	if err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			return false, addr, fmt.Errorf("invalid daemon PID file %s", pidPath)
		}
		if processAlive(pid) {
			return true, addr, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, addr, err
	}
	conn, err := net.DialTimeout("tcp", addr, 600*time.Millisecond)
	if err == nil {
		conn.Close()
		return true, addr, nil
	}
	if daemonConnectionRefused(err) {
		return false, addr, nil
	}
	return false, addr, fmt.Errorf("cannot establish local daemon absence: %w", err)
}
func daemonConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10061))
}
