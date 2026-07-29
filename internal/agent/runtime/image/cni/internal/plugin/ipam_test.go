package plugin

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAllocateIPConcurrentDistinctContainers(t *testing.T) {
	dir := t.TempDir()
	const n = 64

	start := make(chan struct{})
	ips := make(chan net.IP, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ip, err := allocateIP(dir, "10.42.1.0/24", fmt.Sprintf("container-%d", i))
			if err != nil {
				errs <- err
				return
			}
			ips <- ip
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(ips)

	for err := range errs {
		t.Fatalf("AllocateIP: %v", err)
	}
	seen := make(map[string]bool, n)
	for ip := range ips {
		if seen[ip.String()] {
			t.Fatalf("duplicate allocation: %s", ip)
		}
		seen[ip.String()] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique IPs, want %d", len(seen), n)
	}
}

func TestAllocateIPConcurrentProcesses(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	const n = 16

	type child struct {
		cmd *exec.Cmd
		id  string
	}
	children := make([]child, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("process-%d", i)
		cmd := exec.Command(os.Args[0], "-test.run=^TestAllocateIPProcessHelper$")
		cmd.Env = append(os.Environ(),
			"OUTPOST_IPAM_TEST_HELPER=1",
			"OUTPOST_IPAM_TEST_DIR="+dir,
			"OUTPOST_IPAM_TEST_ID="+id,
			"OUTPOST_IPAM_TEST_GATE="+gate,
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %s: %v", id, err)
		}
		children = append(children, child{cmd: cmd, id: id})
	}
	t.Cleanup(func() {
		for _, child := range children {
			if child.cmd.ProcessState == nil {
				_ = child.cmd.Process.Kill()
				_, _ = child.cmd.Process.Wait()
			}
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := 0
		for _, child := range children {
			if _, err := os.Stat(filepath.Join(dir, child.id+".ready")); err == nil {
				ready++
			}
		}
		if ready == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d allocation processes reached the barrier", ready, n)
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool, n)
	for _, child := range children {
		if err := child.cmd.Wait(); err != nil {
			t.Fatalf("child %s: %v", child.id, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, child.id+".ip"))
		if err != nil {
			t.Fatalf("read child %s allocation: %v", child.id, err)
		}
		ip := string(b)
		if seen[ip] {
			t.Fatalf("duplicate cross-process allocation: %s", ip)
		}
		seen[ip] = true
	}
}

func TestAllocateIPProcessHelper(t *testing.T) {
	if os.Getenv("OUTPOST_IPAM_TEST_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	dir := os.Getenv("OUTPOST_IPAM_TEST_DIR")
	id := os.Getenv("OUTPOST_IPAM_TEST_ID")
	gate := os.Getenv("OUTPOST_IPAM_TEST_GATE")
	if err := os.WriteFile(filepath.Join(dir, id+".ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for allocation gate")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := allocateIP(dir, "10.42.5.0/24", id); err != nil {
		t.Fatal(err)
	}
}

func TestAllocateIPConcurrentRetriesStable(t *testing.T) {
	dir := t.TempDir()
	const n = 64

	start := make(chan struct{})
	ips := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ip, err := allocateIP(dir, "10.42.2.0/24", "same-container")
			if err != nil {
				errs <- err
				return
			}
			ips <- ip.String()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(ips)

	for err := range errs {
		t.Fatalf("AllocateIP retry: %v", err)
	}
	var want string
	for ip := range ips {
		if want == "" {
			want = ip
		}
		if ip != want {
			t.Fatalf("retry returned %s, want stable %s", ip, want)
		}
	}
}

func TestReleaseIPIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := allocateIP(dir, "10.42.3.0/24", "container"); err != nil {
		t.Fatal(err)
	}
	if err := releaseIP(dir, "container"); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := releaseIP(dir, "container"); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

func TestMalformedAndStaleFilesAreSafe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stale.ip"), []byte("10.42.4.2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "malformed.ip"), []byte("10.42."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".allocation.tmp"), []byte("10.42.4.3"), 0o600); err != nil {
		t.Fatal(err)
	}

	ip, err := allocateIP(dir, "10.42.4.0/24", "new-container")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ip.String(), "10.42.4.3"; got != want {
		t.Fatalf("allocated %s, want %s (valid stale ledger must remain claimed)", got, want)
	}

	outside := filepath.Join(filepath.Dir(dir), "outside.ip")
	if err := os.WriteFile(outside, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := releaseIP(dir, "../outside"); err == nil {
		t.Fatal("ReleaseIP accepted path-traversing container ID")
	}
	if _, err := allocateIP(dir, "10.42.4.0/24", "../outside"); err == nil {
		t.Fatal("AllocateIP accepted path-traversing container ID")
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "sentinel" {
		t.Fatalf("outside file changed: content=%q err=%v", b, err)
	}
}

func TestDuplicateAndOutOfRangeLedgersAreReassignedSafely(t *testing.T) {
	dir := t.TempDir()
	for name, address := range map[string]string{
		"first.ip":        "10.42.6.2",
		"duplicate.ip":    "10.42.6.2",
		"out-of-range.ip": "10.99.0.2",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(address), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first, err := allocateIP(dir, "10.42.6.0/24", "first")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := allocateIP(dir, "10.42.6.0/24", "duplicate")
	if err != nil {
		t.Fatal(err)
	}
	outOfRange, err := allocateIP(dir, "10.42.6.0/24", "out-of-range")
	if err != nil {
		t.Fatal(err)
	}
	if first.Equal(duplicate) || first.Equal(outOfRange) || duplicate.Equal(outOfRange) {
		t.Fatalf("reassigned ledgers are not unique: %s, %s, %s", first, duplicate, outOfRange)
	}
	for _, name := range []string{"first.ip", "duplicate.ip", "out-of-range.ip"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("allocation %s was destructively removed: %v", name, err)
		}
	}
}
