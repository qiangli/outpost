package runtime

import (
	"context"
	"io/fs"
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
	if err := os.WriteFile(state, []byte("up\nup\ndown\nup\nup\ndown\ndown\n"), 0o644); err != nil {
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countLines(starts) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(childPIDFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
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
	return len(strings.Fields(strings.TrimSpace(string(data)))) / 2
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
