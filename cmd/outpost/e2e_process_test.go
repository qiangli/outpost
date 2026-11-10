package main

import (
	"bytes"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// exec copies output concurrently with readiness checks and failure reporting.
type lockedTestBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedTestBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedTestBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

// All real-daemon launch paths must use this helper before polling readiness.
// Supervised mode makes restart requests exit instead of detaching a successor
// outside the process group owned by this test.
func startTestProcess(t *testing.T, cmd *exec.Cmd) *testProcess {
	t.Helper()
	configureTestProcess(cmd)
	cmd.Env = append(cmd.Environ(), envSupervised+"=1")
	// Bound pipe-copy waits even if a descendant escaped or inherited an fd.
	cmd.WaitDelay = time.Second
	p := &testProcess{cmd: cmd, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start test process: %v", err)
	}
	t.Cleanup(p.stop)
	go func() { _ = cmd.Wait(); close(p.done) }()
	// testing's timeout panics without running Cleanup. Stop ahead of it.
	if deadline, ok := t.Deadline(); ok {
		timer := time.AfterFunc(time.Until(deadline.Add(-time.Second)), p.stop)
		t.Cleanup(func() { timer.Stop() })
	}
	return p
}

func (p *testProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
func (p *testProcess) stop() {
	p.once.Do(func() {
		killTestProcessGroup(p.cmd)
		<-p.done
	})
}
