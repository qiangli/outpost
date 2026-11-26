// Package postinstall reads OS boot and power policy and persists installation
// evidence. Command plans are platform independent so every OS is fake-tested.
package postinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rogpeppe/go-internal/lockedfile"
)

type Command []string
type Readback struct {
	Command  Command `json:"command,omitempty"`
	Output   string  `json:"output"`
	ExitCode int     `json:"exit_code"`
	Error    string  `json:"error,omitempty"`
}
type Runner func(context.Context, Command) Readback

func Exec(ctx context.Context, c Command) Readback {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c[0], c[1:]...)
	out, err := cmd.CombinedOutput()
	r := Readback{Command: c, Output: string(out)}
	if err != nil {
		r.Error = err.Error()
		r.ExitCode = -1
		if cmd.ProcessState != nil {
			r.ExitCode = cmd.ProcessState.ExitCode()
		}
	}
	return r
}
func (r Readback) OK() bool      { return r.ExitCode == 0 && r.Error == "" }
func (r Readback) Value() string { return strings.TrimSpace(r.Output) }

var sleepTargets = []string{"sleep.target", "suspend.target", "hibernate.target", "hybrid-sleep.target"}

func PowerCommands(platform string) []Command {
	switch platform {
	case "windows":
		return []Command{{"powercfg", "/change", "standby-timeout-ac", "0"}, {"powercfg", "/change", "standby-timeout-dc", "0"}, {"powercfg", "/change", "hibernate-timeout-ac", "0"}, {"powercfg", "/change", "hibernate-timeout-dc", "0"}, {"powercfg", "/hibernate", "off"}}
	case "darwin":
		return []Command{{"pmset", "-a", "sleep", "0", "disksleep", "0", "womp", "1"}}
	case "linux":
		return []Command{append(Command{"systemctl", "mask"}, sleepTargets...)}
	}
	return nil
}

// ShellCommand returns a pasteable command for the platform's administrator
// shell. It is presentation only: execution always uses an argv, never a shell.
func ShellCommand(platform string, c Command) string {
	q := make([]string, len(c))
	for i, s := range c {
		if regexp.MustCompile(`^[a-zA-Z0-9_./:=+-]+$`).MatchString(s) {
			q[i] = s
			continue
		}
		if platform == "windows" {
			q[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
		} else {
			q[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	line := strings.Join(q, " ")
	if platform == "windows" && strings.HasPrefix(q[0], "'") {
		line = "& " + line
	}
	return line
}
func ApplyPower(ctx context.Context, platform string, run Runner, dry bool, w io.Writer) error {
	commands := PowerCommands(platform)
	if len(commands) == 0 {
		return fmt.Errorf("unsupported power platform %q", platform)
	}
	for _, c := range commands {
		line := ShellCommand(platform, c)
		if dry {
			if w != nil {
				fmt.Fprintln(w, "#", line)
			}
			continue
		}
		r := run(ctx, c)
		if !r.OK() {
			prefix := "sudo "
			where := ""
			if platform == "windows" {
				prefix = ""
				where = " in an elevated PowerShell"
			}
			return fmt.Errorf("power step failed: %s: %s %s\nRun%s:\n  %s%s", line, r.Error, r.Value(), where, prefix, line)
		}
	}
	return nil
}

func ps(body string) Command {
	return Command{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; " + body}
}

func ReadCommands(platform, user, home string) map[string]Command {
	switch platform {
	case "windows":
		return map[string]Command{
			"standby":           {"powercfg", "/qh", "SCHEME_CURRENT", "SUB_SLEEP", "STANDBYIDLE"},
			"hibernate":         {"powercfg", "/qh", "SCHEME_CURRENT", "SUB_SLEEP", "HIBERNATEIDLE"},
			"hibernate_enabled": ps("(Get-ItemProperty 'HKLM:\\SYSTEM\\CurrentControlSet\\Control\\Power' -Name HibernateEnabled).HibernateEnabled"),
			"task":              ps("$t=Get-ScheduledTask -TaskName 'outpost'; [pscustomobject]@{Enabled=[bool]$t.Settings.Enabled; Boot=[bool](@($t.Triggers | Where-Object {$_.CimClass.CimClassName -eq 'MSFT_TaskBootTrigger' -and $_.Enabled}).Count); Logon=[string]$t.Principal.LogonType} | ConvertTo-Json -Compress"),
		}
	case "darwin":
		return map[string]Command{
			"power":             {"pmset", "-g", "custom"},
			"system_service":    {"plutil", "-convert", "json", "-o", "-", "/Library/LaunchDaemons/io.dhnt.outpost.plist"},
			"disabled_services": {"launchctl", "print-disabled", "system"},
			"user_service":      {"plutil", "-convert", "json", "-o", "-", filepath.Join(home, "Library", "LaunchAgents", "io.dhnt.outpost.plist")},
		}
	case "linux":
		m := map[string]Command{
			"system_service": {"systemctl", "is-enabled", "outpost.service"},
			"user_service":   {"systemctl", "--user", "is-enabled", "outpost.service"},
			"linger":         {"loginctl", "show-user", user, "--property=Linger", "--value"},
		}
		for _, s := range sleepTargets {
			m[s] = Command{"systemctl", "is-enabled", s}
		}
		return m
	}
	return nil
}

// Read never turns missing tools, unrecognized output, or permission errors
// into PASS. All raw command results remain available to the operator.
func Read(ctx context.Context, platform, user, home string, run Runner) Report {
	r := Report{Version: 1, Platform: platform, Time: time.Now().UTC(), Raw: map[string]Readback{}}
	for key, c := range ReadCommands(platform, user, home) {
		v := run(ctx, c)
		v.Command = c
		r.Raw[key] = v
	}
	r.Sleep = "enabled"
	if PowerDisabled(platform, r.Raw) {
		r.Sleep = "disabled"
	}
	r.Service = ServiceState(platform, r.Raw)
	r.Evaluate()
	return r
}

var hexIndex = regexp.MustCompile(`(?mi):\s*(0x[0-9a-f]+)\s*$`)

func PowerDisabled(platform string, raw map[string]Readback) bool {
	return SleepPolicy(platform, raw) == "disabled"
}

// SleepPolicy distinguishes a setting that permits sleep from a failed readback.
func SleepPolicy(platform string, raw map[string]Readback) string {
	switch platform {
	case "windows":
		unknown := false
		for _, key := range []string{"standby", "hibernate"} {
			r := raw[key]
			// /qh for one setting ends in exactly two hex indices (AC and DC).
			indexes := hexIndex.FindAllStringSubmatch(r.Output, -1)
			if !r.OK() || len(indexes) != 2 {
				unknown = true
				continue
			}
			for _, m := range indexes {
				n, err := strconv.ParseUint(m[1][2:], 16, 32)
				if err != nil {
					unknown = true
				} else if n != 0 {
					return "enabled"
				}
			}
		}
		h := raw["hibernate_enabled"]
		if h.OK() && h.Value() == "1" {
			return "enabled"
		}
		if !unknown && h.OK() && h.Value() == "0" {
			return "disabled"
		}
	case "darwin":
		r := raw["power"]
		if !r.OK() {
			break
		}
		known := true
		sources := 0
		var settings map[string]string
		check := func() bool {
			if settings == nil {
				return false
			}
			for _, key := range []string{"sleep", "disksleep", "womp"} {
				if _, err := strconv.Atoi(settings[key]); err != nil {
					known = false
					return false
				}
			}
			return settings["sleep"] != "0" || settings["disksleep"] != "0" || settings["womp"] != "1"
		}
		for _, line := range strings.Split(r.Output, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasSuffix(line, ":") {
				if check() {
					return "enabled"
				}
				sources++
				settings = map[string]string{}
				continue
			}
			if fields := strings.Fields(line); settings != nil && len(fields) >= 2 {
				settings[fields[0]] = fields[1]
			}
		}
		if check() {
			return "enabled"
		}
		if known && sources > 0 {
			return "disabled"
		}
	case "linux":
		unknown := false
		for _, target := range sleepTargets {
			r := raw[target]
			if r.ExitCode != 0 && r.ExitCode != 1 {
				unknown = true
				continue
			}
			switch r.Value() {
			case "masked":
			case "enabled", "enabled-runtime", "static", "disabled", "indirect", "generated", "alias", "linked", "linked-runtime":
				return "enabled"
			default:
				unknown = true
			}
		}
		if !unknown {
			return "disabled"
		}
	}
	return "unknown"
}

func ServiceState(platform string, raw map[string]Readback) string {
	value := func(k string) string {
		r, ok := raw[k]
		if !ok || !r.OK() {
			return ""
		}
		return r.Value()
	}
	switch platform {
	case "linux":
		if value("system_service") == "enabled" {
			return "boot"
		}
		if value("user_service") == "enabled" && value("linger") == "yes" {
			return "boot"
		}
		if value("user_service") == "enabled" || value("system_service") == "enabled-runtime" || value("user_service") == "enabled-runtime" {
			return "session"
		}
	case "darwin":
		plistOK := func(s string) bool {
			var p struct {
				Label     string
				RunAtLoad bool
				KeepAlive any
				Disabled  bool
			}
			if json.Unmarshal([]byte(s), &p) != nil {
				return false
			}
			return p.Label == "io.dhnt.outpost" && p.RunAtLoad && p.KeepAlive == true && !p.Disabled
		}
		d, ok := raw["disabled_services"]
		if plistOK(value("system_service")) && ok && d.OK() && !regexp.MustCompile(`"io\.dhnt\.outpost"\s*=>\s*true`).MatchString(d.Output) {
			return "boot"
		}
		if plistOK(value("user_service")) {
			return "session"
		}
	case "windows":
		var task struct {
			Enabled, Boot bool
			Logon         string
		}
		if json.Unmarshal([]byte(value("task")), &task) == nil && task.Enabled {
			if task.Boot && (task.Logon == "S4U" || task.Logon == "Password" || task.Logon == "ServiceAccount") {
				return "boot"
			}
			return "session"
		}
	}
	return "none"
}

// LocalChecks are the same-on-every-OS liveness probes `outpost verify
// --post-install` fills in around Read. They need loopback dials and
// process discovery — not shell commands — so they live outside the
// Runner abstraction and are applied via ApplyLocal (injectable in
// tests, wired to the real probes in the CLI).
type LocalChecks struct {
	SupervisorRunning bool
	DaemonHealthy     bool
	SSHDListening     bool
	BootPathSane      bool
	// BootPathDetail names what's wrong when BootPathSane is false
	// (e.g. the missing PATH entries), for the evidence JSON.
	BootPathDetail string
}

type Report struct {
	Version  int       `json:"version"`
	Platform string    `json:"platform"`
	Time     time.Time `json:"time"`
	Result   string    `json:"result"`
	Sleep    string    `json:"sleep"`
	Service  string    `json:"service"`
	// Supervisor is "running" when the outpost init (supervisord) the
	// OS service owns is alive; Daemon is "healthy" when the admin
	// endpoint answers; SSHD is "listening" when the LAN sshd unit's
	// port accepts loopback TCP; BootPath is "sane" when the boot
	// session PATH carries the platform's expected prefixes.
	Supervisor     string              `json:"supervisor"`
	Daemon         string              `json:"daemon"`
	SSHD           string              `json:"sshd"`
	BootPath       string              `json:"boot_path"`
	BootPathDetail string              `json:"boot_path_detail,omitempty"`
	Raw            map[string]Readback `json:"raw"`
	Reboot         *RebootEvidence     `json:"reboot,omitempty"`
}

// ApplyLocal records the local liveness probes into the report and
// re-evaluates the verdict. The same six checks gate PASS on macOS,
// Linux and Windows.
func (r *Report) ApplyLocal(c LocalChecks) {
	if c.SupervisorRunning {
		r.Supervisor = "running"
	} else {
		r.Supervisor = "not-running"
	}
	if c.DaemonHealthy {
		r.Daemon = "healthy"
	} else {
		r.Daemon = "down"
	}
	if c.SSHDListening {
		r.SSHD = "listening"
	} else {
		r.SSHD = "closed"
	}
	if c.BootPathSane {
		r.BootPath = "sane"
	} else {
		r.BootPath = "broken"
	}
	r.BootPathDetail = c.BootPathDetail
	r.Evaluate()
}

type RebootEvidence struct {
	RequestedAt        time.Time  `json:"requested_at"`
	BootBefore         string     `json:"boot_before"`
	BootAfter          string     `json:"boot_after,omitempty"`
	BootChanged        bool       `json:"boot_changed"`
	NoLogin            bool       `json:"no_login_observed"`
	ServiceLaunch      bool       `json:"service_launch"`
	ServerAcknowledged bool       `json:"server_acknowledged"`
	AcknowledgedAt     *time.Time `json:"acknowledged_at,omitempty"`
	Phase              string     `json:"phase"`
}

func (r *Report) Evaluate() {
	pass := r.Sleep == "disabled" && r.Service == "boot" &&
		r.Supervisor == "running" && r.Daemon == "healthy" &&
		r.SSHD == "listening" && r.BootPath == "sane"
	if b := r.Reboot; b != nil {
		pass = pass && b.BootChanged && b.NoLogin && b.ServiceLaunch && b.ServerAcknowledged
	}
	r.Result = "FAIL"
	if pass {
		r.Result = "PASS"
	}
}

const maxReportBytes = 1 << 20
const ReportName = "post-install.json"
const RebootReportName = "post-install-reboot.json"
const PendingName = "post-install-reboot.pending.json"

// Write uses a same-directory rename so a power loss cannot expose partial JSON.
func Write(dir, name string, r Report) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if len(b)+1 > maxReportBytes {
		return fmt.Errorf("post-install evidence exceeds %d bytes", maxReportBytes)
	}
	switch name {
	case ReportName, RebootReportName, PendingName:
	default:
		return errors.New("unknown post-install report")
	}
	unlock, err := lockedfile.MutexAt(filepath.Join(dir, ".post-install.lock")).Lock()
	if err != nil {
		return err
	}
	defer unlock()
	// One scratch slot across all three reports, even if the process died
	// before publishing. O_EXCL avoids following a pre-existing link.
	tmp := filepath.Join(dir, ".post-install.next")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
