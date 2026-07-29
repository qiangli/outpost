package runtime

import (
	"context"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEmbeddedEntrypointInvokesFRPCSupervisor(t *testing.T) {
	data, err := fs.ReadFile(imageFS, "image/entrypoint.sh")
	if err != nil {
		t.Fatalf("read embedded entrypoint: %v", err)
	}
	script := string(data)
	if !strings.Contains(script, "/usr/local/bin/frpc-supervisor.sh") {
		t.Fatal("entrypoint does not invoke the frpc supervisor helper")
	}
	if !strings.Contains(script, "FRPC_REQUIRED_PORTS") || !strings.Contains(script, "${OUTPOST_API_PORT}") {
		t.Fatal("entrypoint does not pass required visitor ports to the supervisor")
	}
}

func TestFRPCSupervisorRestartsAliveProcessWhenVisitorDisappears(t *testing.T) {
	tmp := t.TempDir()
	starts := filepath.Join(tmp, "starts")
	terms := filepath.Join(tmp, "terms")
	probes := filepath.Join(tmp, "probes")
	state := filepath.Join(tmp, "state")
	if err := os.WriteFile(state, []byte("up\nup\ndown\nup\nup\ndown\ndown\nup\nup\nup\nup\nup\nup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakes := filepath.Join(tmp, "bin")
	if err := os.Mkdir(fakes, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakes, "frpc"), `#!/bin/sh
echo start "$$" >> "$FRPC_TEST_STARTS"
trap 'echo term "$$" >> "$FRPC_TEST_TERMS"; exit 0' TERM INT
while true; do sleep 1; done
`)
	writeExecutable(t, filepath.Join(fakes, "probe"), `#!/bin/sh
count=0
[ -f "$FRPC_TEST_PROBES" ] && count=$(cat "$FRPC_TEST_PROBES")
count=$((count + 1))
printf '%s' "$count" > "$FRPC_TEST_PROBES"
line=$(sed -n "${count}p" "$FRPC_TEST_STATE")
[ "$line" = "up" ]
`)
	writeExecutable(t, filepath.Join(fakes, "sleep"), `#!/bin/sh
exit 0
`)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", embeddedImagePath(t, "frpc-supervisor.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+fakes+":"+os.Getenv("PATH"),
		"FRPC_BIN=frpc",
		"FRPC_CONFIG=/tmp/frpc.toml",
		"FRPC_LOG="+filepath.Join(tmp, "frpc.log"),
		"FRPC_REQUIRED_PORTS=6443,8092",
		"FRPC_STARTUP_GRACE_TICKS=2",
		"FRPC_MISS_THRESHOLD=2",
		"FRPC_PROBE_INTERVAL=1",
		"FRPC_RESTART_BACKOFF=1",
		"FRPC_PROBE_BIN=probe",
		"FRPC_TEST_STARTS="+starts,
		"FRPC_TEST_TERMS="+terms,
		"FRPC_TEST_PROBES="+probes,
		"FRPC_TEST_STATE="+state,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if countLines(starts) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("terminate supervisor: %v", err)
	}
	_ = cmd.Wait()

	if got := countLines(starts); got != 2 {
		t.Fatalf("frpc starts = %d, want exactly 2; transient miss should not flap", got)
	}
	if got := countLines(terms); got == 0 {
		t.Fatalf("supervisor did not terminate the old frpc child")
	}
	if got := readInt(t, probes); got < 7 {
		t.Fatalf("probe calls = %d, want at least 7", got)
	}
}

func TestFRPCSupervisorCleansChildOnCancellation(t *testing.T) {
	tmp := t.TempDir()
	childPIDFile := filepath.Join(tmp, "child.pid")
	fakes := filepath.Join(tmp, "bin")
	if err := os.Mkdir(fakes, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakes, "frpc"), `#!/bin/sh
echo "$$" > "$FRPC_TEST_CHILD_PID"
trap 'exit 0' TERM INT
while true; do sleep 1; done
`)
	writeExecutable(t, filepath.Join(fakes, "probe"), `#!/bin/sh
exit 0
`)
	writeExecutable(t, filepath.Join(fakes, "sleep"), `#!/bin/sh
/bin/sleep 0.01
`)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", embeddedImagePath(t, "frpc-supervisor.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+fakes+":"+os.Getenv("PATH"),
		"FRPC_BIN=frpc",
		"FRPC_CONFIG=/tmp/frpc.toml",
		"FRPC_LOG="+filepath.Join(tmp, "frpc.log"),
		"FRPC_REQUIRED_PORTS=6443",
		"FRPC_PROBE_INTERVAL=1",
		"FRPC_PROBE_BIN=probe",
		"FRPC_TEST_CHILD_PID="+childPIDFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(childPIDFile); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	pidBytes, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatalf("child pid not written: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("terminate supervisor: %v", err)
	}
	_ = cmd.Wait()
	time.Sleep(50 * time.Millisecond)
	if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err == nil {
		t.Fatalf("frpc child pid %d still exists after supervisor cancellation", pid)
	}
}

func TestFRPCSupervisorRestartsOnProcessExit(t *testing.T) {
	tmp := t.TempDir()
	starts := filepath.Join(tmp, "starts")
	runCount := filepath.Join(tmp, "run_count")
	fakes := filepath.Join(tmp, "bin")
	if err := os.Mkdir(fakes, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakes, "frpc"), `#!/bin/sh
echo start "$$" >> "$FRPC_TEST_STARTS"
count=0
[ -f "$FRPC_TEST_RUN_COUNT" ] && count=$(cat "$FRPC_TEST_RUN_COUNT")
count=$((count + 1))
printf '%s' "$count" > "$FRPC_TEST_RUN_COUNT"
if [ "$count" -eq 1 ]; then
    sleep 1
    exit 1
fi
trap 'exit 0' TERM INT
while true; do sleep 1; done
`)
	writeExecutable(t, filepath.Join(fakes, "probe"), `#!/bin/sh
exit 0
`)
	writeExecutable(t, filepath.Join(fakes, "sleep"), `#!/bin/sh
exit 0
`)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", embeddedImagePath(t, "frpc-supervisor.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+fakes+":"+os.Getenv("PATH"),
		"FRPC_BIN=frpc",
		"FRPC_CONFIG=/tmp/frpc.toml",
		"FRPC_LOG="+filepath.Join(tmp, "frpc.log"),
		"FRPC_REQUIRED_PORTS=6443",
		"FRPC_PROBE_INTERVAL=1",
		"FRPC_RESTART_BACKOFF=1",
		"FRPC_PROBE_BIN=probe",
		"FRPC_TEST_STARTS="+starts,
		"FRPC_TEST_RUN_COUNT="+runCount,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if countLines(starts) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("terminate supervisor: %v", err)
	}
	_ = cmd.Wait()

	if got := countLines(starts); got < 2 {
		t.Fatalf("frpc starts = %d, want at least 2 (exit + restart)", got)
	}
	pids := extractPIDs(t, starts)
	if len(pids) < 2 {
		t.Fatalf("frpc pids = %d, want at least 2 distinct PIDs tracking the new child", len(pids))
	}
	if pids[0] == pids[1] {
		t.Fatalf("first and second frpc PID are the same (%s); expected a new tracked child", pids[0])
	}
}

func TestFRPCSupervisorProbesRealListenerViaDevTCP(t *testing.T) {
	bashBin := findDevTCPBash(t)
	tmp := t.TempDir()
	starts := filepath.Join(tmp, "starts")
	terms := filepath.Join(tmp, "terms")
	fakes := filepath.Join(tmp, "bin")
	if err := os.Mkdir(fakes, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakes, "frpc"), `#!/bin/sh
echo start "$$" >> "$FRPC_TEST_STARTS"
trap 'echo term "$$" >> "$FRPC_TEST_TERMS"; exit 0' TERM INT
while true; do sleep 1; done
`)
	writeExecutable(t, filepath.Join(fakes, "sleep"), `#!/bin/sh
exit 0
`)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start tcp listener: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bashBin, embeddedImagePath(t, "frpc-supervisor.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+fakes+":"+os.Getenv("PATH"),
		"FRPC_BIN=frpc",
		"FRPC_CONFIG=/tmp/frpc.toml",
		"FRPC_LOG="+filepath.Join(tmp, "frpc.log"),
		"FRPC_REQUIRED_PORTS="+strconv.Itoa(port),
		"FRPC_STARTUP_GRACE_TICKS=2",
		"FRPC_MISS_THRESHOLD=3",
		"FRPC_PROBE_INTERVAL=1",
		"FRPC_RESTART_BACKOFF=1",
		"FRPC_PROBE_BIN=",
		"FRPC_TEST_STARTS="+starts,
		"FRPC_TEST_TERMS="+terms,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}

	dl := time.Now().Add(10 * time.Second)
	for time.Now().Before(dl) {
		if countLines(starts) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if countLines(starts) < 1 {
		t.Fatalf("supervisor did not start frpc")
	}

	time.Sleep(5 * time.Second)

	if countLines(starts) > 1 {
		listener.Close()
		t.Fatalf("supervisor restarted frpc while listener was still up (unexpected probe failure)")
	}

	listener.Close()

	dl2 := time.Now().Add(10 * time.Second)
	for time.Now().Before(dl2) {
		if countLines(starts) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("terminate supervisor: %v", err)
	}
	_ = cmd.Wait()

	if got := countLines(starts); got < 2 {
		t.Fatalf("frpc starts = %d, want at least 2 (initial + restart after listener closed)", got)
	}
	if got := countLines(terms); got == 0 {
		t.Fatalf("supervisor did not terminate the old frpc child")
	}
}

func findDevTCPBash(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open test listener: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	defer ln.Close()

	for _, candidate := range []string{
		"/opt/homebrew/bin/bash",
		"/usr/local/bin/bash",
	} {
		if _, err := os.Stat(candidate); err == nil {
			testScript := "echo >/dev/tcp/127.0.0.1/" + strconv.Itoa(port) + " 2>/dev/null"
			cmd := exec.Command(candidate, "-c", testScript)
			if err := cmd.Run(); err == nil {
				return candidate
			}
		}
	}
	t.Skip("no bash with /dev/tcp support found on this host; skipping production-default probe test")
	return ""
}

func embeddedImagePath(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(imageFS, "image/"+name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0
	}
	return len(strings.Split(text, "\n"))
}

func readInt(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return n
}

func extractPIDs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var pids []string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			pids = append(pids, fields[1])
		}
	}
	return pids
}
