package postinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func BootCommand(platform string) Command {
	switch platform {
	case "linux":
		return Command{"cat", "/proc/sys/kernel/random/boot_id"}
	case "darwin":
		return Command{"sysctl", "-n", "kern.boottime"}
	case "windows":
		return ps("(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o')")
	}
	return nil
}
func RebootCommand(platform string) Command {
	if platform == "windows" {
		return Command{"shutdown.exe", "/r", "/t", "60"}
	}
	// Elevate only shutdown, keeping the report in the service user's cache.
	return Command{"sudo", "-n", "shutdown", "-r", "+1"}
}

func Schedule(ctx context.Context, dir string, r Report, run Runner) error {
	if r.Result != "PASS" {
		return fmt.Errorf("post-install checks failed; reboot was not scheduled")
	}
	b := run(ctx, BootCommand(r.Platform))
	r.Raw["boot_before"] = b
	if !b.OK() || b.Value() == "" {
		r.Result = "FAIL"
		_ = Write(dir, RebootReportName, r)
		return fmt.Errorf("cannot read boot identifier: %s %s", b.Error, b.Value())
	}
	r.Reboot = &RebootEvidence{RequestedAt: time.Now().UTC(), BootBefore: b.Value(), Phase: "awaiting_reboot"}
	r.Evaluate() // FAIL until a new boot and acknowledged server heartbeat exist.
	if err := Write(dir, RebootReportName, r); err != nil {
		return err
	}
	if err := Write(dir, PendingName, r); err != nil {
		return err
	}
	c := RebootCommand(r.Platform)
	result := run(ctx, c)
	r.Raw["schedule_reboot"] = result
	if !result.OK() {
		r.Reboot.Phase = "schedule_failed"
	}
	if err := Write(dir, RebootReportName, r); err != nil {
		return err
	}
	if !result.OK() {
		retry := c
		if r.Platform != "windows" {
			retry = Command{"sudo", "shutdown", "-r", "+1"}
		}
		return fmt.Errorf("reboot scheduling failed: %s %s\nRun with administrator rights:\n  %s", result.Error, result.Value(), ShellCommand(r.Platform, retry))
	}
	return nil
}

// noLogin records conservative snapshots. Unknown/error results never establish
// absence. It does not purport to reconstruct historical logins from OS logs.
func noLogin(ctx context.Context, platform string, run Runner, raw map[string]Readback, prefix string) bool {
	probe := func(key string, c Command) Readback { r := run(ctx, c); r.Command = c; raw[prefix+key] = r; return r }
	switch platform {
	case "darwin":
		w := probe("sessions", Command{"who"})
		// who alone misses some GUI logins. The console is owned by root at loginwindow.
		c := probe("console", Command{"stat", "-f", "%u", "/dev/console"})
		return w.OK() && w.Value() == "" && c.OK() && c.Value() == "0"
	case "linux":
		s := probe("sessions", Command{"loginctl", "list-sessions", "--no-legend", "--no-pager"})
		if !s.OK() {
			return false
		}
		for i, line := range strings.Split(s.Value(), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			c := probe(fmt.Sprintf("session_class_%d", i), Command{"loginctl", "show-session", fields[0], "--property=Class", "--value"})
			// Only explicitly noninteractive classes may be ignored.
			if !c.OK() {
				return false
			}
			switch c.Value() {
			case "manager", "manager-early", "background", "background-light", "greeter":
			default:
				return false
			}
		}
		return true
	case "windows":
		// Include interactive, remote-interactive and cached interactive logons. Filter only
		// the OS window-manager accounts, which exist before the sign-in screen.
		r := probe("sessions", ps(`$sessions=@(Get-CimInstance Win32_LogonSession -Filter 'LogonType=2 OR LogonType=10 OR LogonType=11 OR LogonType=12 OR LogonType=13'); $count=0; foreach($s in $sessions){$accounts=@(Get-CimAssociatedInstance -InputObject $s -Association Win32_LoggedOnUser); if($accounts.Count -eq 0){$count++}; foreach($a in $accounts){if($a.Domain -notin @('Window Manager','Font Driver Host')){$count++}}}; $count`))
		return r.OK() && r.Value() == "0"
	}
	return false
}

// serviceLaunch ties this daemon's parent to the OS service supervisor.
func serviceLaunch(ctx context.Context, platform string, parent int, run Runner, raw map[string]Readback) bool {
	probe := func(key string, c Command) Readback { r := run(ctx, c); r.Command = c; raw[key] = r; return r }
	switch platform {
	case "linux":
		r := probe("service_pid", Command{"systemctl", "show", "outpost.service", "--property=MainPID", "--value"})
		if r.OK() && r.Value() == strconv.Itoa(parent) {
			return true
		}
		r = probe("user_service_pid", Command{"systemctl", "--user", "show", "outpost.service", "--property=MainPID", "--value"})
		return r.OK() && r.Value() == strconv.Itoa(parent)
	case "darwin":
		r := probe("service_process", Command{"launchctl", "print", "system/io.dhnt.outpost"})
		m := regexp.MustCompile(`(?m)^\s*pid = ([0-9]+)\s*$`).FindStringSubmatch(r.Output)
		return r.OK() && len(m) == 2 && m[1] == strconv.Itoa(parent)
	case "windows":
		// S4U boot tasks run in noninteractive session 0; a manually launched
		// interactive supervisor cannot satisfy this check.
		r := probe("service_process", ps(fmt.Sprintf(`$p=Get-CimInstance Win32_Process -Filter 'ProcessId=%d'; if(!$p){throw 'parent missing'}; [pscustomobject]@{Session=$p.SessionId; Supervisor=[bool]($p.CommandLine -match '\bsupervisord\b')} | ConvertTo-Json -Compress`, parent)))
		var p struct {
			Session    *int
			Supervisor bool
		}
		return r.OK() && json.Unmarshal([]byte(r.Output), &p) == nil && p.Session != nil && *p.Session == 0 && p.Supervisor
	}
	return false
}

// carryLocal copies point-in-time local liveness from a previous report
// into a fresh re-read, so the reboot flow's verdict keeps the
// pre-reboot verify's supervisor/daemon/sshd/PATH evidence while
// sleep/service are re-read from the new boot.
func carryLocal(dst, src *Report) {
	dst.Supervisor = src.Supervisor
	dst.Daemon = src.Daemon
	dst.SSHD = src.SSHD
	dst.BootPath = src.BootPath
	dst.BootPathDetail = src.BootPathDetail
}

type Observer struct {
	mu                        sync.Mutex
	dir, platform, user, home string
	run                       Runner
	report                    Report
	done                      bool
}

// Resume runs at daemon startup, before serving. A same-boot process restart
// leaves the request pending. A different boot freezes the first observation;
// restarting after logout must never turn an observed login into success.
func Resume(ctx context.Context, dir, platform, user, home string, parent int, run Runner) (*Observer, error) {
	data, err := os.ReadFile(filepath.Join(dir, PendingName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending Report
	if err = json.Unmarshal(data, &pending); err != nil {
		return nil, err
	}
	if pending.Reboot == nil {
		return nil, fmt.Errorf("reboot request has no evidence")
	}
	boot := run(ctx, BootCommand(platform))
	if !boot.OK() || boot.Value() == "" {
		return nil, fmt.Errorf("read reboot identity: %s %s", boot.Error, boot.Value())
	}
	if boot.Value() == pending.Reboot.BootBefore {
		return nil, nil
	}
	r := pending
	if pending.Reboot.BootAfter == "" {
		r = Read(ctx, platform, user, home, run)
		// The re-read refreshes sleep/service from the new boot; the
		// point-in-time local liveness (supervisor/daemon/sshd/PATH)
		// carries over from the pre-reboot verify — the fresh
		// service-launch + server-ack evidence below is what proves
		// the new boot came back.
		carryLocal(&r, &pending)
		r.Reboot = pending.Reboot
		r.Raw["boot_before"] = pending.Raw["boot_before"]
		r.Raw["boot_after"] = boot
		r.Reboot.BootAfter = boot.Value()
		r.Reboot.BootChanged = true
		r.Reboot.NoLogin = noLogin(ctx, platform, run, r.Raw, "startup_")
		r.Reboot.ServiceLaunch = serviceLaunch(ctx, platform, parent, run, r.Raw)
		r.Reboot.Phase = "awaiting_server"
	} else if r.Reboot.BootAfter != boot.Value() {
		r.Reboot.Phase = "unexpected_additional_reboot"
		r.Reboot.NoLogin = false
	}
	r.Evaluate()
	if err = Write(dir, PendingName, r); err != nil {
		return nil, err
	}
	if err = Write(dir, RebootReportName, r); err != nil {
		return nil, err
	}
	return &Observer{dir: dir, platform: platform, user: user, home: home, run: run, report: r}, nil
}

// Acknowledge is called ONLY after the paired server accepted an authenticated
// heartbeat (HTTP 200). No host name, URL, user identifier or token is copied
// into the summary; command readbacks are local diagnostic evidence.
func (o *Observer) Acknowledge(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return nil
	}
	r := Read(ctx, o.platform, o.user, o.home, o.run)
	carryLocal(&r, &o.report)
	for k, v := range o.report.Raw {
		if _, exists := r.Raw[k]; exists {
			r.Raw["startup_"+k] = v
		} else {
			r.Raw[k] = v
		}
	}
	r.Reboot = o.report.Reboot
	r.Reboot.NoLogin = noLogin(ctx, o.platform, o.run, r.Raw, "ack_") && r.Reboot.NoLogin
	r.Reboot.ServerAcknowledged = true
	r.Raw["server_heartbeat"] = Readback{Output: "HTTP 200: authenticated heartbeat accepted by paired server"}
	now := time.Now().UTC()
	r.Reboot.AcknowledgedAt = &now
	r.Reboot.Phase = "complete"
	r.Evaluate()
	if err := Write(o.dir, RebootReportName, r); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(o.dir, PendingName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	o.done = true
	return nil
}
