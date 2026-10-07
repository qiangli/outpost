package main

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestDaemonAbsenceOnlyConnectionRefused(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	if !daemonConnectionRefused(refused) {
		t.Fatal("refused connection should prove no listener")
	}
	for _, err := range []error{context.DeadlineExceeded, os.ErrPermission, errors.New("malformed daemon response"), nil} {
		if daemonConnectionRefused(err) {
			t.Fatalf("ambiguous failure classified absent: %v", err)
		}
	}
}
