//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exercise the same cleanup as the real-daemon tests with harmless stand-ins,
// including a TERM-ignoring descendant and testing's hard timeout panic.
func TestDaemonProcessCleanup(t *testing.T) {
	if mode := os.Getenv("OUTPOST_CLEANUP_GUARD"); mode != "" {
		cmd := exec.Command("/bin/sh", "-c", `trap '' TERM; sleep 600 & echo "$$ $!" > "$1"; wait`, "guard", os.Getenv("OUTPOST_CLEANUP_PIDS"))
		output := &lockedTestBuffer{}
		cmd.Stdout, cmd.Stderr = output, output
		p := startTestProcess(t, cmd)
		deadline := time.Now().Add(time.Second)
		for {
			if b, _ := os.ReadFile(os.Getenv("OUTPOST_CLEANUP_PIDS")); strings.Contains(string(b), " ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("stand-in never became ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
		switch mode {
		case "failure":
			t.Fatal("intentional cleanup guard failure")
		case "timeout":
			select {}
		case "explicit":
			p.stop()
			p.stop()
			if !p.exited() || cmd.ProcessState == nil {
				t.Fatal("stop did not wait")
			}
		}
		return
	}
	for _, mode := range []string{"normal", "explicit", "failure", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "pids")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonProcessCleanup$", "-test.timeout=3s")
			cmd.Env = append(os.Environ(), "OUTPOST_CLEANUP_GUARD="+mode, "OUTPOST_CLEANUP_PIDS="+pidfile)
			output, err := cmd.CombinedOutput()
			b, readErr := os.ReadFile(pidfile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var leader, child int
			if n, err := fmt.Sscanf(string(b), "%d %d", &leader, &child); n != 2 || err != nil {
				t.Fatalf("invalid pids: %q", b)
			}
			// Emergency cleanup if a future regression breaks the helper.
			defer syscall.Kill(-leader, syscall.SIGKILL)
			if ctx.Err() != nil {
				t.Fatalf("guard hung: %s", output)
			}
			if mode == "normal" || mode == "explicit" {
				if err != nil {
					t.Fatalf("guard failed: %v\n%s", err, output)
				}
			} else if err == nil {
				t.Fatal("guard unexpectedly succeeded")
			}
			if mode == "failure" && !strings.Contains(string(output), "intentional cleanup guard failure") {
				t.Fatalf("wrong failure: %s", output)
			}
			if mode == "timeout" && !strings.Contains(string(output), "test timed out") {
				t.Fatalf("wrong timeout: %s", output)
			}
			for _, pid := range []int{leader, child} {
				deadline := time.Now().Add(time.Second)
				for {
					err := syscall.Kill(pid, 0)
					if err == syscall.ESRCH {
						break
					}
					// A killed orphan can briefly be a zombie awaiting init's reap.
					state, _ := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
					if strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("process %d survived %s cleanup", pid, mode)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		})
	}
}

// Exercise supervisor replacement without building or starting outpost.
func TestDaemonProcessRestart(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `test "$OUTPOST_SUPERVISED" = 1; exit $?`)
	cmd.Env = append(os.Environ(), testServiceEnv(t)...)
	output := &lockedTestBuffer{}
	cmd.Stdout, cmd.Stderr = output, output
	first := startTestProcess(t, cmd)
	next := restartTestProcess(t, first)
	if next == first || next.cmd.Process.Pid == first.cmd.Process.Pid {
		t.Fatal("restart did not replace the supervised process")
	}
	<-next.done
	if !first.exited() || !first.cmd.ProcessState.Success() || !next.cmd.ProcessState.Success() {
		t.Fatalf("supervised processes did not exit successfully: %s", output.String())
	}
	if strings.Join(first.cmd.Environ(), "\n") != strings.Join(next.cmd.Environ(), "\n") {
		t.Fatal("restart lost isolated service identity or environment")
	}
	next.stop()
}
