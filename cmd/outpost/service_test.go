package main

import (
	"net"
	"strings"
	"testing"
)

func TestDaemonRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_ADMIN_ADDR", ln.Addr().String())
	if !daemonRunning() {
		t.Error("daemonRunning() = false with a listener up; want true")
	}
	_ = ln.Close()
	if daemonRunning() {
		t.Error("daemonRunning() = true after listener closed; want false")
	}
}

func assertContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s missing %q\n%s", what, w, got)
		}
	}
}

func assertAbsent(t *testing.T, what, got string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(got, u) {
			t.Errorf("%s unexpectedly contains %q\n%s", what, u, got)
		}
	}
}

// ---- system renders: start at BOOT, run as the target user ---------------

func TestRenderLaunchDaemonPlist(t *testing.T) {
	got := renderLaunchDaemonPlist("/opt/outpost", "dev", "/Users/dev")
	assertContains(t, "launchd daemon plist", got,
		"<string>io.dhnt.outpost</string>",
		"<string>/opt/outpost</string>",
		"<string>supervisord</string>",       // supervisor, not start
		"<key>UserName</key><string>dev</string>", // drop to the regular user
		"<key>RunAtLoad</key><true/>",              // start at boot
		"<key>KeepAlive</key><true/>",              // auto-restart
		"<key>HOME</key><string>/Users/dev</string>", // HOME for config/cache resolution
	)
}

func TestRenderSystemdSystemUnit(t *testing.T) {
	got := renderSystemdSystemUnit("/opt/outpost", "dev")
	assertContains(t, "systemd system unit", got,
		"ExecStart=/opt/outpost supervisord",
		"User=dev",                  // drop to the regular user
		"Restart=on-failure",
		"WantedBy=multi-user.target", // boot, before any login
		"network-online.target",
	)
}

func TestRenderWindowsStartupTask(t *testing.T) {
	got := renderWindowsStartupTask(`C:\outpost.exe`, `EXAMPLE\Dev`)
	assertContains(t, "windows startup task", got,
		"Register-ScheduledTask",
		"-TaskName 'outpost'",
		`-Execute 'C:\outpost.exe'`,
		"-Argument 'supervisord'",
		"-AtStartup",            // boot, not logon
		`-UserId 'EXAMPLE\Dev'`,
		"-LogonType S4U",        // run as user, no stored password
		"-RunLevel Limited",     // regular user, not elevated
	)
	assertAbsent(t, "windows startup task", got, "-AtLogOn", "Interactive")
}

// ---- user renders (--user fallback): start at LOGIN ----------------------

func TestRenderLaunchAgentPlist(t *testing.T) {
	got := renderLaunchAgentPlist("/opt/outpost", "/Users/dev")
	assertContains(t, "launchd agent plist", got,
		"<string>io.dhnt.outpost</string>",
		"<string>supervisord</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
	)
	assertAbsent(t, "launchd agent plist", got, "<key>UserName</key>") // per-user: no UserName
}

func TestRenderSystemdUserUnit(t *testing.T) {
	got := renderSystemdUserUnit("/opt/outpost")
	assertContains(t, "systemd --user unit", got,
		"ExecStart=/opt/outpost supervisord",
		"Restart=on-failure",
		"WantedBy=default.target", // per-user: login scope
	)
	assertAbsent(t, "systemd --user unit", got, "User=") // per-user: no User=
}

func TestRenderWindowsLogonTask(t *testing.T) {
	got := renderWindowsLogonTask(`C:\outpost.exe`, `EXAMPLE\Dev`)
	assertContains(t, "windows logon task", got,
		"-Argument 'supervisord'",
		"-AtLogOn",
		`-User 'EXAMPLE\Dev'`,
		"-LogonType Interactive ", // cmdlet enum (NOT InteractiveToken)
		"-RunLevel Limited",
	)
	assertAbsent(t, "windows logon task", got, "InteractiveToken", "-AtStartup")
}
