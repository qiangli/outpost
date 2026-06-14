package main

import (
	"strings"
	"testing"
)

func TestRenderLaunchdPlist(t *testing.T) {
	got := renderLaunchdPlist("/opt/outpost", "/Users/lern")
	for _, want := range []string{
		"<string>io.dhnt.outpost</string>",
		"<string>/opt/outpost</string>",
		"<string>supervisord</string>", // launches the supervisor, not `start`
		"<key>RunAtLoad</key><true/>",  // start at login
		"<key>KeepAlive</key><true/>",  // auto-restart
		"<string>/Users/lern</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("launchd plist missing %q\n%s", want, got)
		}
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	got := renderSystemdUnit("/opt/outpost")
	for _, want := range []string{
		"ExecStart=/opt/outpost supervisord",
		"Restart=on-failure",
		"WantedBy=default.target",
		"network-online.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("systemd unit missing %q\n%s", want, got)
		}
	}
}

func TestRenderWindowsRegisterCmd(t *testing.T) {
	got := renderWindowsRegisterCmd(`C:\Users\Lern\AppData\Local\outpost\outpost.exe`, `2IVY\Lern`)
	for _, want := range []string{
		"Register-ScheduledTask",
		"-TaskName 'outpost'",
		`-Execute 'C:\Users\Lern\AppData\Local\outpost\outpost.exe'`,
		"-Argument 'supervisord'", // supervisor, not start
		"-AtLogOn",
		`-User '2IVY\Lern'`,       // space-safe principal
		"-LogonType Interactive ", // cmdlet enum (NOT InteractiveToken); no password / no admin
		"-RunLevel Limited",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("windows register cmd missing %q\n%s", want, got)
		}
	}
}
