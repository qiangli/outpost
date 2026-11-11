package main

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Guard all OS implementations, including platforms this host cannot run.
// Direct exec calls bypass TestMain's injected service runner and are unsafe.
func TestServiceCommandBoundary(t *testing.T) {
	for _, name := range []string{"service_darwin.go", "service_linux.go", "service_windows.go", "run.go", "verify.go", "service.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "exec" && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext") {
				t.Errorf("%s bypasses injected service runner", name)
			}
			if ok && pkg.Name == "postinstall" && sel.Sel.Name == "Exec" {
				t.Errorf("%s bypasses injected postinstall runner", name)
			}
			return true
		})
	}
	b, err := os.ReadFile("e2e_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"-tags", "outpost_testisolated"`) {
		t.Error("E2E binary lacks service isolation build tag")
	}
}

// Fail the test process before exec, even when a caller ignores Run errors.
func rejectTestServiceCommand(name string, args ...string) *exec.Cmd {
	panic(fmt.Sprintf("test attempted host service command without injected fake: %s %q", name, args))
}

// Every real-binary test has its own service identity and audit file. A CLI
// failure expected by a test must not hide a forbidden service-manager call.
func testServiceEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	audit := filepath.Join(dir, "service-attempts")
	t.Cleanup(func() {
		b, err := os.ReadFile(audit)
		if err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
		if len(b) != 0 {
			t.Errorf("test attempted host service access:\n%s", b)
		}
	})
	return []string{"OUTPOST_TEST_SERVICE_LABEL=" + fmt.Sprintf("outpost-test-%x", sha256.Sum256([]byte(dir))), "OUTPOST_TEST_SERVICE_AUDIT=" + audit}
}

func TestServiceRunnerRejectsUnmockedCall(t *testing.T) {
	if os.Getenv("OUTPOST_GUARD_CHILD") == "1" {
		// Construction must fail before any executable lookup or process start.
		serviceCommand("launchctl", "bootout", "gui/501/io.dhnt.outpost")
		t.Fatal("unmocked service command was allowed")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestServiceRunnerRejectsUnmockedCall$")
	cmd.Env = append(os.Environ(), "OUTPOST_GUARD_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "host service command") {
		t.Fatalf("expected fail-closed service guard: %v\n%s", err, out)
	}
}

// Exercise the same platform uninstaller reached by `unpair --yes`, with an
// injected executable, isolated paths, and a unique test service identity.
func TestServiceUninstallUsesFake(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	oldRunner := serviceCommand
	oldLaunchd, oldSystemd, oldWindows := launchdLabel, systemdUnit, windowsTask
	label := fmt.Sprintf("outpost-test-%x", sha256.Sum256([]byte(home)))
	launchdLabel, systemdUnit, windowsTask = "io.dhnt.test."+label, label+".service", label
	t.Cleanup(func() {
		serviceCommand = oldRunner
		launchdLabel, systemdUnit, windowsTask = oldLaunchd, oldSystemd, oldWindows
	})
	calls := 0
	targeted := false
	serviceCommand = func(name string, args ...string) *exec.Cmd {
		calls++
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "io.dhnt.outpost") || strings.Contains(joined, "outpost.service") {
			t.Fatalf("real host label reached fake: %s %s", name, joined)
		}
		targeted = targeted || strings.Contains(joined, label)
		cmd := exec.Command(os.Args[0], "-test.run=^TestServiceRunnerFixture$")
		cmd.Env = append(os.Environ(), "OUTPOST_SERVICE_FIXTURE=1")
		return cmd
	}
	if err := (platformUserServiceManager{}).Uninstall(); err != nil {
		t.Fatal(err)
	}
	if calls == 0 || !targeted {
		t.Fatalf("uninstaller bypassed fake or test label: calls=%d targeted=%v", calls, targeted)
	}
}

func TestServiceRunnerFixture(t *testing.T) {
	if os.Getenv("OUTPOST_SERVICE_FIXTURE") == "1" {
		os.Exit(0)
	}
}

// Reproduce register's service activation path without a local fake. The
// suite-wide runner must stop takeover before its first bootout/disable.
func TestRegisterServiceGuard(t *testing.T) {
	if os.Getenv("OUTPOST_REGISTER_GUARD_CHILD") == "1" {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		t.Setenv("OUTPOST_ADMIN_ADDR", "127.0.0.1:0")
		if err := activatePairedService(false, false); err != nil {
			t.Fatal(err)
		}
		t.Fatal("register was allowed to manage a host service")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegisterServiceGuard$")
	cmd.Env = append(os.Environ(), "OUTPOST_REGISTER_GUARD_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "host service command") {
		t.Fatalf("expected registration takeover to fail closed: %v\n%s", err, out)
	}
}

func TestServicePowerGuard(t *testing.T) {
	if os.Getenv("OUTPOST_POWER_GUARD_CHILD") == "1" {
		cmd := serviceInstallCmd()
		cmd.SetArgs([]string{"--user", "--power"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("service install was allowed to change host power policy")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestServicePowerGuard$")
	cmd.Env = append(os.Environ(), "OUTPOST_POWER_GUARD_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "host service command") {
		t.Fatalf("expected power policy to fail closed: %v\n%s", err, out)
	}
}
