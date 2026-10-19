package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The hosted control plane died on first boot because metrics-server hit an
// RBAC forbidden error: the apiserver had accepted the bindings the supervisor
// applies, but its authorizer had not yet seen them. metrics-server treats the
// 403 as fatal and exits, the supervisor exited 1 on any child death, and
// server-entrypoint.sh then tore down the whole container. These tests pin the
// bootstrap race down: wait for the authorizer before starting, and survive a
// transient forbidden exit instead of failing the plane.

// outpostStub stands in for the outpost binary's certgen / netaddr / tlsproxy /
// metrics-exec verbs. METRICS_STUB_FAIL_RUNS makes the first N metrics-exec
// runs exit 1 with METRICS_STUB_FAIL_MESSAGE on stderr.
const outpostStub = `#!/bin/sh
verb="$1"
case "$verb" in
certgen)
    key=""; cert=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --key) key="$2"; shift 2 ;;
            --cert) cert="$2"; shift 2 ;;
            *) shift ;;
        esac
    done
    echo stub-key > "$key"
    echo stub-cert > "$cert"
    ;;
netaddr)
    echo "netaddr" >> "$METRICS_STUB_EVENTS"
    ;;
tlsproxy)
    echo "tlsproxy" >> "$METRICS_STUB_EVENTS"
    trap 'exit 0' TERM INT
    while kill -0 "$PPID" 2>/dev/null; do sleep 0.2; done
    ;;
metrics-exec)
    n=0
    [ -f "$METRICS_STUB_RUNS" ] && n=$(cat "$METRICS_STUB_RUNS")
    n=$((n + 1))
    printf '%s' "$n" > "$METRICS_STUB_RUNS"
    echo "metrics-exec:$n" >> "$METRICS_STUB_EVENTS"
    if [ "$n" -le "${METRICS_STUB_FAIL_RUNS:-0}" ]; then
        echo "$METRICS_STUB_FAIL_MESSAGE" >&2
        exit 1
    fi
    trap 'exit 0' TERM INT
    while kill -0 "$PPID" 2>/dev/null; do sleep 0.2; done
    ;;
*)
    echo "unexpected outpost verb: $verb" >&2
    exit 2
    ;;
esac
exit 0
`

// kubectlStub answers the three calls the supervisor makes. The first
// METRICS_STUB_CANI_NO `auth can-i` calls answer "no" the way the apiserver
// does while its authorizer is still catching up.
const kubectlStub = `#!/bin/sh
args="$*"
case "$args" in
*"auth can-i"*)
    n=0
    [ -f "$METRICS_STUB_CANI" ] && n=$(cat "$METRICS_STUB_CANI")
    n=$((n + 1))
    printf '%s' "$n" > "$METRICS_STUB_CANI"
    if [ "$n" -le "${METRICS_STUB_CANI_NO:-0}" ]; then
        echo "can-i:no" >> "$METRICS_STUB_EVENTS"
        echo no
        exit 1
    fi
    echo "can-i:yes" >> "$METRICS_STUB_EVENTS"
    echo yes
    ;;
*" apply "*)
    echo "apply" >> "$METRICS_STUB_EVENTS"
    ;;
*"get service"*)
    echo 10.43.0.10
    ;;
*delete*)
    ;;
*)
    echo "unexpected kubectl call: $args" >&2
    exit 2
    ;;
esac
exit 0
`

type metricsFixture struct {
	bash   string
	script string
	tmp    string
	bin    string
	work   string
	events string
	runs   string
	cani   string
	out    string
}

func newMetricsFixture(t *testing.T) *metricsFixture {
	t.Helper()
	bash := findSupervisorBash(t)
	tmp := t.TempDir()
	f := &metricsFixture{
		bash:   bash,
		script: embeddedImagePath(t, "metrics-server-supervisor.sh"),
		tmp:    tmp,
		bin:    filepath.Join(tmp, "bin"),
		work:   filepath.Join(tmp, "work"),
		events: filepath.Join(tmp, "events"),
		runs:   filepath.Join(tmp, "runs"),
		cani:   filepath.Join(tmp, "cani"),
		out:    filepath.Join(tmp, "supervisor.log"),
	}
	tlsDir := filepath.Join(tmp, "tls")
	for _, dir := range []string{f.bin, tlsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"client-ca.crt", "client-ca.key", "server-ca.crt"} {
		if err := os.WriteFile(filepath.Join(tlsDir, name), []byte("stub\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "k3s-internal.yaml"), []byte("stub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(f.bin, "outpost"), outpostStub)
	writeExecutable(t, filepath.Join(f.bin, "kubectl"), kubectlStub)
	return f
}

// env builds the supervisor environment with tiny budgets, overridden by the
// caller's map. Every absolute path the script needs comes in through a seam.
func (f *metricsFixture) env(overrides map[string]string) []string {
	vars := map[string]string{
		"PATH":                               f.bin + ":" + os.Getenv("PATH"),
		"OUTPOST_METRICS_ADDRESS":            "10.88.0.7",
		"OUTPOST_METRICS_WORK_DIR":           f.work,
		"OUTPOST_METRICS_KUBECONFIG":         filepath.Join(f.tmp, "k3s-internal.yaml"),
		"OUTPOST_METRICS_TLS_DIR":            filepath.Join(f.tmp, "tls"),
		"OUTPOST_METRICS_OWNER":              "",
		"OUTPOST_METRICS_TICK_SECONDS":       "0.2",
		"OUTPOST_METRICS_ROTATE_TICKS":       "300",
		"OUTPOST_METRICS_AUTH_WAIT_TRIES":    "5",
		"OUTPOST_METRICS_AUTH_WAIT_INTERVAL": "1",
		"OUTPOST_METRICS_RETRY_BUDGET":       "2",
		"OUTPOST_METRICS_RETRY_BACKOFF":      "1",
		"OUTPOST_METRICS_RETRY_BACKOFF_MAX":  "1",
		"METRICS_STUB_EVENTS":                f.events,
		"METRICS_STUB_RUNS":                  f.runs,
		"METRICS_STUB_CANI":                  f.cani,
		"METRICS_STUB_FAIL_RUNS":             "0",
		"METRICS_STUB_FAIL_MESSAGE":          "",
		"METRICS_STUB_CANI_NO":               "0",
	}
	for k, v := range overrides {
		vars[k] = v
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

func (f *metricsFixture) start(t *testing.T, ctx context.Context, overrides map[string]string) *exec.Cmd {
	t.Helper()
	sink, err := os.Create(f.out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	cmd := exec.CommandContext(ctx, f.bash, f.script)
	cmd.Env = f.env(overrides)
	cmd.Stdout = sink
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		t.Fatalf("start metrics supervisor: %v", err)
	}
	return cmd
}

func (f *metricsFixture) output() string {
	data, _ := os.ReadFile(f.out)
	return string(data)
}

func (f *metricsFixture) eventLines() []string {
	data, err := os.ReadFile(f.events)
	if err != nil {
		return nil
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func readCounter(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

func waitForCounter(path string, want int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if readCounter(path) >= want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// A control plane must not die because metrics-server lost a bootstrap race
// with its own RBAC bindings.
func TestMetricsSupervisorSurvivesTransientForbidden(t *testing.T) {
	f := newMetricsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := f.start(t, ctx, map[string]string{
		"METRICS_STUB_FAIL_RUNS": "1",
		"METRICS_STUB_FAIL_MESSAGE": `E1019 00:00:00.1 pathrecordermux.go:1 "Failed to get configmap" err=` +
			`"configmaps \"extension-apiserver-authentication\" is forbidden: User ` +
			`\"system:serviceaccount:kube-system:metrics-server\" cannot get resource"`,
	})
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	if !waitForCounter(f.runs, 2, 10*time.Second) {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited (%v) instead of retrying a transient forbidden error:\n%s", err, f.output())
		default:
			t.Fatalf("supervisor never restarted metrics-server:\n%s", f.output())
		}
	}
	select {
	case err := <-done:
		t.Fatalf("supervisor exited (%v) after restarting metrics-server:\n%s", err, f.output())
	case <-time.After(500 * time.Millisecond):
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("terminate supervisor: %v", err)
	}
	<-done
	if out := f.output(); !strings.Contains(out, "retry 1/2") {
		t.Errorf("supervisor did not report the bounded retry:\n%s", out)
	}
}

// The retry budget is bounded: a persistent failure must still take the
// container down so the host supervisor recreates it.
func TestMetricsSupervisorFailsAfterRetryBudget(t *testing.T) {
	t.Run("persistent-forbidden", func(t *testing.T) {
		f := newMetricsFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := f.start(t, ctx, map[string]string{
			"METRICS_STUB_FAIL_RUNS":    "99",
			"METRICS_STUB_FAIL_MESSAGE": `nodes is forbidden: User "x" cannot list resource "nodes"`,
		})
		err := cmd.Wait()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() == 0 {
			t.Fatalf("persistent forbidden failure must exit non-zero, got %v:\n%s", err, f.output())
		}
		// Initial start plus the whole retry budget (2), and no more.
		if got := readCounter(f.runs); got != 3 {
			t.Fatalf("metrics-server runs = %d, want 3 (start + 2 retries):\n%s", got, f.output())
		}
		if out := f.output(); !strings.Contains(out, "FATAL: metrics-server exited unexpectedly") {
			t.Errorf("missing fatal diagnostic:\n%s", out)
		}
	})

	t.Run("non-forbidden-fails-immediately", func(t *testing.T) {
		f := newMetricsFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := f.start(t, ctx, map[string]string{
			"METRICS_STUB_FAIL_RUNS":    "99",
			"METRICS_STUB_FAIL_MESSAGE": "failed to listen on 10.88.0.7:10250: address already in use",
		})
		err := cmd.Wait()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("persistent non-forbidden failure must exit 1, got %v:\n%s", err, f.output())
		}
		if got := readCounter(f.runs); got != 1 {
			t.Fatalf("metrics-server runs = %d, want 1: a non-RBAC failure is not retried:\n%s", got, f.output())
		}
	})
}

// metrics-server must not be started until the authorizer actually answers for
// its identity, and the wait must be bounded.
func TestMetricsSupervisorWaitsForRBACAuthorizer(t *testing.T) {
	t.Run("waits-until-yes", func(t *testing.T) {
		f := newMetricsFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := f.start(t, ctx, map[string]string{"METRICS_STUB_CANI_NO": "2"})
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		if !waitForCounter(f.runs, 1, 10*time.Second) {
			t.Fatalf("metrics-server never started:\n%s", f.output())
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done

		events := f.eventLines()
		var sawYes bool
		for _, ev := range events {
			switch {
			case ev == "can-i:yes":
				sawYes = true
			case strings.HasPrefix(ev, "metrics-exec:") && !sawYes:
				t.Fatalf("metrics-server started before the authorizer admitted it: %v", events)
			}
		}
		if !sawYes {
			t.Fatalf("supervisor never polled the authorizer: %v", events)
		}
		if got := readCounter(f.cani); got < 3 {
			t.Fatalf("auth can-i calls = %d, want at least 3 (two denials then a grant)", got)
		}
	})

	t.Run("bounded-when-never-yes", func(t *testing.T) {
		f := newMetricsFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := f.start(t, ctx, map[string]string{
			"METRICS_STUB_CANI_NO":               "99",
			"OUTPOST_METRICS_AUTH_WAIT_TRIES":    "2",
			"OUTPOST_METRICS_AUTH_WAIT_INTERVAL": "1",
		})
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		if !waitForCounter(f.runs, 1, 10*time.Second) {
			t.Fatalf("bounded authorizer wait never gave up and started metrics-server:\n%s", f.output())
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
		if got := readCounter(f.cani); got > 4 {
			t.Fatalf("auth can-i calls = %d, want the wait bounded by OUTPOST_METRICS_AUTH_WAIT_TRIES", got)
		}
	})
}
