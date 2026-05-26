// End-to-end tests that spawn the real `outpost` binary as a black-box
// subprocess. Existing tests under internal/* exercise individual
// surfaces in-process (httptest servers, mock SDK clients, etc.); this
// file fills the remaining gap — does the actual binary boot, bind its
// listener, accept MCP requests, persist state, restart cleanly, and
// survive the migration path?
//
// Isolation:
//   - $XDG_CONFIG_HOME and $XDG_CACHE_HOME point at t.TempDir() so the
//     daemon's agent.json and pidfile/log/cookies don't touch the dev
//     machine's real state.
//   - OUTPOST_ADMIN_ADDR uses a random free port so we never collide
//     with a running outpost on the standard 127.0.0.1:17777.
//   - $MATRIX_* are blanked so the daemon never accidentally dials a
//     real cloudbox.
//
// The binary is built once in TestMain into a per-package tempdir. If
// the build fails the whole suite skips with a clear message rather
// than failing one-test-at-a-time.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// e2eBinary is set by TestMain to the absolute path of the built
// outpost binary used by every E2E test in this package. Empty when
// the build failed (every test skips in that case).
var e2eBinary string

// e2eBuildErr is the error message we surface to skipping tests when
// the build failed at suite setup. Helpful so the skip message says
// *why* rather than just "binary unavailable".
var e2eBuildErr string

func TestMain(m *testing.M) {
	// Build once for the whole package. We do this here rather than
	// from each test so multi-test runs amortize the ~3 s build cost.
	dir, err := os.MkdirTemp("", "outpost-e2e-bin-")
	if err != nil {
		e2eBuildErr = "mktempdir: " + err.Error()
		os.Exit(m.Run())
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "outpost")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		e2eBuildErr = "go build: " + err.Error()
	} else {
		e2eBinary = bin
	}
	os.Exit(m.Run())
}

// requireE2E skips the calling test when the binary isn't available
// (build failed) or when we're on a platform that can't run subprocess
// tests in the usual way. Tests should call this as their first line.
func requireE2E(t *testing.T) {
	t.Helper()
	if e2eBinary == "" {
		t.Skipf("e2e binary unavailable: %s", e2eBuildErr)
	}
	if runtime.GOOS == "windows" {
		// Subprocess signaling + pidfile semantics differ on Windows;
		// the production code handles it (see detach_windows.go) but
		// the test harness here is POSIX-shaped. Leave Windows e2e
		// for follow-up work.
		t.Skip("e2e tests are POSIX-shaped; skipping on Windows")
	}
}

// freePort returns a TCP port that net.Listen could bind on
// 127.0.0.1 right now. There's a small race between the Listen+Close
// here and the actual bind by the daemon, but it's tight enough that
// flakes in practice would only show under heavy parallel CI.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// daemon represents one in-flight `outpost start` subprocess and the
// isolated config/cache dirs that back it. Bury all the bookkeeping
// here so individual tests read as "spawn → exercise → stop".
type daemon struct {
	t         *testing.T
	cmd       *exec.Cmd
	addr      string // "127.0.0.1:<port>"
	configDir string // -> XDG_CONFIG_HOME
	cacheDir  string // -> XDG_CACHE_HOME
	stdout    *strings.Builder
	stderr    *strings.Builder
	stopped   atomic.Bool
}

// spawnDaemon starts an `outpost start` subprocess against an isolated
// XDG layout on a random loopback port. Waits up to ~5 s for /healthz
// to respond. Caller is responsible for d.stop() (typically via
// t.Cleanup).
func spawnDaemon(t *testing.T) *daemon {
	t.Helper()
	port := freePort(t)
	d := &daemon{
		t:         t,
		addr:      fmt.Sprintf("127.0.0.1:%d", port),
		configDir: filepath.Join(t.TempDir(), "config"),
		cacheDir:  filepath.Join(t.TempDir(), "cache"),
		stdout:    &strings.Builder{},
		stderr:    &strings.Builder{},
	}
	for _, dir := range []string{d.configDir, d.cacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	d.cmd = exec.Command(e2eBinary, "start")
	d.cmd.Env = append([]string{},
		"HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+d.configDir,
		"XDG_CACHE_HOME="+d.cacheDir,
		"OUTPOST_ADMIN_ADDR="+d.addr,
		"PATH="+os.Getenv("PATH"),
	)
	d.cmd.Stdout = d.stdout
	d.cmd.Stderr = d.stderr
	if err := d.cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() { d.stop() })

	// Wait for /healthz to flip OK.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + d.addr + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return d
			}
		}
		// Daemon may have died early — surface that fast rather than
		// timeout.
		if d.cmd.ProcessState != nil {
			t.Fatalf("daemon exited before /healthz responded.\nstdout:\n%s\nstderr:\n%s",
				d.stdout.String(), d.stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon /healthz never responded.\nstdout:\n%s\nstderr:\n%s",
		d.stdout.String(), d.stderr.String())
	return nil
}

// stop sends SIGTERM and waits up to 5 s for graceful exit, then
// SIGKILLs. Idempotent — calling twice (once from a test, once from
// the t.Cleanup) is fine.
func (d *daemon) stop() {
	if d.stopped.Swap(true) {
		return
	}
	if d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- d.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = d.cmd.Process.Kill()
		<-done
	}
}

// cli runs `outpost <args...>` against the same isolated env as the
// daemon and returns combined stdout+stderr. Errors propagate via
// (out, err) so callers can assert both.
func (d *daemon) cli(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e2eBinary, args...)
	cmd.Env = d.cmd.Env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustCLI is the assertive variant — fatals on error. Most test
// steps don't expect failure, so this makes the assertion site less
// noisy than wrapping every call in an if.
func (d *daemon) mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := d.cli(ctx, args...)
	if err != nil {
		t.Fatalf("`outpost %s` failed: %v\noutput:\n%s",
			strings.Join(args, " "), err, out)
	}
	return out
}

// statusJSON parses `outpost status --json` into a generic map for
// field-level assertions.
func (d *daemon) statusJSON(t *testing.T) map[string]any {
	t.Helper()
	out := d.mustCLI(t, "status", "--json")
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("parse status JSON: %v\nraw: %s", err, out)
	}
	return raw
}

// agentJSONPath returns the path the daemon should be writing
// agent.json to, given the isolated XDG_CONFIG_HOME.
func (d *daemon) agentJSONPath() string {
	return filepath.Join(d.configDir, "matrix", "agent.json")
}

// readAgentJSON reads + parses agent.json off the isolated config
// directory. Useful to assert the daemon's actual on-disk state
// matches what the API surface reports.
func (d *daemon) readAgentJSON(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(d.agentJSONPath())
	if err != nil {
		t.Fatalf("read agent.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("parse agent.json: %v\nraw: %s", err, string(b))
	}
	return raw
}

// ---- the tests ----

// TestE2E_DaemonLifecycle covers: the binary builds, the daemon binds
// /healthz on the isolated port, the MCP bearer auto-generates into
// agent.json on first boot, and /mcp/* rejects unauthenticated
// requests with 401.
func TestE2E_DaemonLifecycle(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)

	// /healthz — already asserted by spawnDaemon, but re-check to
	// document that the listener is genuinely up after our wait loop.
	resp, err := http.Get("http://" + d.addr + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("/healthz: status=%v err=%v", resp, err)
	}
	resp.Body.Close()

	// agent.json should now contain a non-empty mcp_bearer_token —
	// EnsureMCPBearerToken auto-generates on first boot.
	fc := d.readAgentJSON(t)
	tok, _ := fc["mcp_bearer_token"].(string)
	if len(tok) < 32 {
		t.Errorf("expected mcp_bearer_token (32+ hex chars), got %q", tok)
	}

	// /mcp/* unauthenticated → 401.
	r, err := http.Post("http://"+d.addr+"/mcp/", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /mcp/: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Errorf("/mcp/ unauthenticated = %d, want 401", r.StatusCode)
	}

	// /mcp/* with a bad bearer → 401.
	req, _ := http.NewRequest("POST", "http://"+d.addr+"/mcp/", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("Content-Type", "application/json")
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp/ wrong bearer: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Errorf("/mcp/ wrong bearer = %d, want 401", r.StatusCode)
	}
}

// TestE2E_StatusUnpaired verifies the CLI ↔ MCP wiring: a freshly
// booted daemon reports unpaired via `outpost status --json`, and the
// CLI's bearer-from-file authentication works end-to-end.
func TestE2E_StatusUnpaired(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)
	st := d.statusJSON(t)
	cfg, ok := st["config"].(map[string]any)
	if !ok {
		t.Fatalf("status missing config block: %v", st)
	}
	if got, _ := cfg["agent_name"].(string); got != "" {
		t.Errorf("agent_name = %q, want empty (unpaired)", got)
	}
	if got, _ := cfg["has_token"].(bool); got {
		t.Errorf("has_token = true on a fresh boot; expected false")
	}
}

// TestE2E_AppCRUD walks the round-trip an operator would: add an app
// via the CLI (which routes through MCP → admincore → file), list it
// via both `outpost apps list` and direct file read, then remove it
// and confirm it's gone everywhere. Locks in the "all surfaces stay
// in sync" invariant.
func TestE2E_AppCRUD(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)

	// Add via CLI → MCP.
	d.mustCLI(t, "apps", "add", "testapp",
		"--url", "http://127.0.0.1:9000",
		"--require-login")

	// Visible in `apps list`.
	out := d.mustCLI(t, "apps", "list")
	if !strings.Contains(out, "testapp") {
		t.Errorf("apps list missing testapp:\n%s", out)
	}

	// Visible in agent.json with the right fields.
	fc := d.readAgentJSON(t)
	apps, _ := fc["apps"].([]any)
	if len(apps) != 1 {
		t.Fatalf("agent.json apps count = %d, want 1", len(apps))
	}
	app := apps[0].(map[string]any)
	if got, _ := app["name"].(string); got != "testapp" {
		t.Errorf("app name = %q, want testapp", got)
	}
	if got, _ := app["host"].(string); got != "127.0.0.1" {
		t.Errorf("app host = %q, want 127.0.0.1", got)
	}
	if got, _ := app["port"].(float64); got != 9000 {
		t.Errorf("app port = %v, want 9000", got)
	}
	if got, _ := app["require_login"].(bool); !got {
		t.Errorf("app require_login = false, want true")
	}

	// Remove and confirm gone everywhere.
	d.mustCLI(t, "apps", "rm", "testapp")
	out = d.mustCLI(t, "apps", "list")
	if strings.Contains(out, "testapp") {
		t.Errorf("apps list still shows testapp after rm:\n%s", out)
	}
	fc = d.readAgentJSON(t)
	apps, _ = fc["apps"].([]any)
	if len(apps) != 0 {
		t.Errorf("agent.json apps count = %d after rm, want 0", len(apps))
	}
}

// TestE2E_BuiltinToggleRoundTrip flips a builtin via CLI and confirms
// the change persists in agent.json. On an unpaired host the save
// doesn't trigger a restart (RestartPending=false), so we don't need
// to wait for the daemon to re-exec — just verify the file flipped.
func TestE2E_BuiltinToggleRoundTrip(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)

	// Default: ssh_enabled is absent (treated as true). Explicitly
	// turn it off and confirm.
	d.mustCLI(t, "builtins", "set", "--ssh=off")
	fc := d.readAgentJSON(t)
	if got, _ := fc["ssh_enabled"].(bool); got {
		t.Errorf("ssh_enabled = true after --ssh=off, want false")
	}

	// Flip back on. The field should now appear as true in JSON.
	d.mustCLI(t, "builtins", "set", "--ssh=on")
	fc = d.readAgentJSON(t)
	if got, _ := fc["ssh_enabled"].(bool); !got {
		t.Errorf("ssh_enabled = false after --ssh=on, want true")
	}
}

// TestE2E_DeprecatedFlagWarning confirms that the old short-form SSH
// flags survive as deprecated aliases — cobra prints a warning to
// stderr but the operation still completes.
func TestE2E_DeprecatedFlagWarning(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := d.cli(ctx, "builtins", "set", "--ssh-local-fwd=on")
	if err != nil {
		t.Fatalf("CLI failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "deprecated") {
		t.Errorf("expected 'deprecated' warning in output:\n%s", out)
	}
	// Confirm the underlying field still got flipped.
	fc := d.readAgentJSON(t)
	if got, _ := fc["ssh_allow_local_forward"].(bool); !got {
		t.Errorf("ssh_allow_local_forward did not flip; deprecated alias should still work")
	}
}

// TestE2E_DocsCommand confirms `outpost docs` lists topics and
// renders each one. Catches drift between docsManifest, the embedded
// markdown, and the canonical docs/ sources.
func TestE2E_DocsCommand(t *testing.T) {
	requireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// No need for a running daemon — `docs` reads only from the
	// embedded FS and the on-disk FileConfig isn't touched.
	d := &daemon{cmd: exec.Command(e2eBinary)}
	d.cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	out, err := d.cli(ctx, "docs")
	if err != nil {
		t.Fatalf("`outpost docs`: %v\n%s", err, out)
	}
	for _, want := range []string{"settings", "mcp"} {
		if !strings.Contains(out, want) {
			t.Errorf("docs listing missing topic %q:\n%s", want, out)
		}
	}
	// Each topic renders.
	for _, topic := range []string{"settings", "mcp"} {
		out, err := d.cli(ctx, "docs", topic)
		if err != nil {
			t.Fatalf("`outpost docs %s`: %v\n%s", topic, err, out)
		}
		if len(out) < 200 {
			t.Errorf("docs %s body too short (%d bytes); did the embed break?", topic, len(out))
		}
	}
}

// TestE2E_StopGraceful verifies `outpost stop` shuts the daemon down
// gracefully via SIGTERM (no SIGKILL fallback). The MCP server has
// to drop its long-lived SSE sessions on context cancel — otherwise
// http.Server.Shutdown waits the full 5 s for those connections to
// drain and stop falls back to SIGKILL. See mcpapi.Server.Close and
// adminui.Deps.OnShutdown.
func TestE2E_StopGraceful(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)

	// Open an MCP session before stopping so we exercise the SSE
	// teardown path. Without this the test would pass even if Close()
	// were a no-op.
	d.mustCLI(t, "status", "--json")

	out := d.mustCLI(t, "stop")
	if !strings.Contains(out, "Stopped outpost") {
		t.Errorf("expected 'Stopped outpost' (graceful), got:\n%s", out)
	}
	if strings.Contains(out, "Force-killed") {
		t.Errorf("daemon fell back to SIGKILL — graceful shutdown regressed:\n%s", out)
	}
	// Belt-and-braces: /healthz should be dead within ~2 s. Graceful
	// shutdown completes in tens of milliseconds when Close() works;
	// the timeout here is just a safety net against the test going
	// off the rails.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := http.Get("http://" + d.addr + "/healthz")
		if err != nil && (errors.Is(err, syscall.ECONNREFUSED) ||
			strings.Contains(err.Error(), "connection refused")) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("daemon still serving /healthz 2s after graceful stop returned")
}

// TestE2E_XDGMigrationAutoMoves pre-populates the legacy
// os.UserConfigDir()-based location with an agent.json, boots the
// daemon, and confirms the file migrated to the canonical
// $XDG_CONFIG_HOME/matrix/ location (and the legacy is now empty).
//
// The migration helpers themselves are unit-tested in
// internal/agent/conf/paths_test.go; this test confirms `outpost start`
// actually invokes them.
func TestE2E_XDGMigrationAutoMoves(t *testing.T) {
	requireE2E(t)

	// Set up an isolated HOME so os.UserConfigDir() resolves into a
	// place we control. On Linux, os.UserConfigDir() defaults to
	// $HOME/.config when $XDG_CONFIG_HOME is unset — which equals the
	// canonical path, so no migration ever fires. The test only
	// produces a meaningful divergence when legacy != canonical;
	// fake that by setting XDG_CONFIG_HOME to <home>/xdg-config and
	// pre-populating <home>/.config/matrix as the "legacy" path.
	if runtime.GOOS == "linux" {
		t.Skip("on Linux $HOME/.config IS the XDG default; legacy and canonical resolve to the same path, no migration to test here")
	}

	home := t.TempDir()
	canonicalCfg := filepath.Join(home, "xdg-config")
	cacheDir := filepath.Join(home, "xdg-cache")
	if err := os.MkdirAll(canonicalCfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Pre-populate the legacy path with a recognizable agent.json.
	// On macOS, os.UserConfigDir() = $HOME/Library/Application Support.
	legacyDir := filepath.Join(home, "Library", "Application Support", "matrix")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyFile := filepath.Join(legacyDir, "agent.json")
	const marker = `{"agent_name":"legacy-host"}`
	if err := os.WriteFile(legacyFile, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cmd := exec.Command(e2eBinary, "start")
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + canonicalCfg,
		"XDG_CACHE_HOME=" + cacheDir,
		"OUTPOST_ADMIN_ADDR=" + addr,
		"PATH=" + os.Getenv("PATH"),
	}
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	// Wait for /healthz to flip OK (means migration already ran).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if cmd.ProcessState != nil {
			t.Fatalf("daemon exited before healthz.\nstdout:\n%s\nstderr:\n%s",
				stdout.String(), stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The canonical path now holds the agent.json (with the same
	// agent_name marker we wrote into legacy + auto-generated
	// tokens added on first boot).
	canonFile := filepath.Join(canonicalCfg, "matrix", "agent.json")
	b, err := os.ReadFile(canonFile)
	if err != nil {
		t.Fatalf("canonical agent.json missing after migration: %v", err)
	}
	var fc map[string]any
	if err := json.Unmarshal(b, &fc); err != nil {
		t.Fatalf("parse canonical agent.json: %v", err)
	}
	if got, _ := fc["agent_name"].(string); got != "legacy-host" {
		t.Errorf("agent_name = %q, want legacy-host (migration didn't preserve it)", got)
	}

	// Legacy path is gone (or at least no longer the agent.json file
	// itself — the migration renames legacy → canonical).
	if _, err := os.Stat(legacyFile); err == nil {
		t.Errorf("legacy agent.json still exists at %s; migration should have moved it", legacyFile)
	}
}
