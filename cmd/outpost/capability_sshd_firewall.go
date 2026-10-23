package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Windows firewall management for the `outpost.sshd` unit.
//
// On activation on Windows the unit ensures an inbound allow rule for its
// TCP port, scoped to the outpost program, via `netsh advfirewall` — pure
// subprocess, no PowerShell, no COM, no new dependency. On any other OS
// the firewall is nil (no-op): nothing else needs an allow rule installed
// by the daemon.
//
// Rule shape (idempotent, converged with set-rule-new rather than
// delete+add so a re-probe never flaps the live rule):
//
//   - "outpost sshd" — profile=private,domain, remoteip=any. Always.
//   - "outpost sshd (public)" — profile=public, remoteip=localsubnet. Only
//     while the active network profile is Public (or unknown — a host that
//     cannot say which profile it is on keeps subnet-scoped reachability
//     rather than going silently dark).
//
// Removed on unit disable. When netsh refuses (no admin rights), Ensure
// returns the exact reason and the unit reports degraded instead of
// silently being unreachable.
const (
	sshdFirewallRuleName       = "outpost sshd"
	sshdFirewallPublicRuleName = "outpost sshd (public)"
)

// sshdFirewall owns the unit's inbound allow rules.
type sshdFirewall interface {
	// Ensure converges the rules for port scoped to program. Idempotent.
	Ensure(port int, program string) error
	// Remove deletes the rules. Idempotent (absent rules are success).
	Remove() error
}

// defaultSSHDFirewall selects the platform firewall: netsh on Windows,
// nil (no-op) elsewhere.
func defaultSSHDFirewall() sshdFirewall {
	if runtime.GOOS != "windows" {
		return nil
	}
	return &netshFirewall{}
}

// networkProfile is the host's active firewall profile.
type networkProfile int

const (
	profileUnknown networkProfile = iota
	profileDomain
	profilePrivate
	profilePublic
)

// netshRunner runs one netsh invocation, returning combined output.
type netshRunner func(ctx context.Context, args ...string) (string, error)

// netshFirewall implements sshdFirewall through netsh advfirewall.
type netshFirewall struct {
	run     netshRunner
	profile func(ctx context.Context) (networkProfile, error)
}

func (f *netshFirewall) runner() netshRunner {
	if f.run != nil {
		return f.run
	}
	return func(ctx context.Context, args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "netsh", args...).CombinedOutput()
		return string(out), err
	}
}

func (f *netshFirewall) activeProfile(ctx context.Context) networkProfile {
	probe := f.profile
	if probe == nil {
		probe = func(c context.Context) (networkProfile, error) {
			out, err := f.runner()(c, "advfirewall", "show", "currentprofile")
			if err != nil {
				return profileUnknown, fmt.Errorf("show currentprofile: %w", err)
			}
			return parseCurrentProfile(out), nil
		}
	}
	p, err := probe(ctx)
	if err != nil {
		return profileUnknown
	}
	return p
}

// parseCurrentProfile reads the active profile off
// `netsh advfirewall show currentprofile`, whose first line names it
// ("Public Profile Settings:", ...). Anything unrecognized is unknown —
// the plan then keeps subnet-scoped reachability rather than guessing.
func parseCurrentProfile(out string) networkProfile {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "domain profile"):
			return profileDomain
		case strings.Contains(lower, "private profile"):
			return profilePrivate
		case strings.Contains(lower, "public profile"):
			return profilePublic
		}
		return profileUnknown
	}
	return profileUnknown
}

// sshdFirewallPlan decides rule coverage from the active profile: the base
// rule always covers Private+Domain; the public rule (restricted to the
// local subnet) is ensured while the host sits on Public (or an unknown
// profile) and withdrawn otherwise.
func sshdFirewallPlan(active networkProfile) (publicRestricted bool) {
	return active == profilePublic || active == profileUnknown
}

// sshdRuleArgs builds the add/set argument tail for one allow rule.
func sshdRuleArgs(name, program string, port int, profiles, remoteIP string) []string {
	return []string{
		"name=\"" + name + "\"",
		"dir=in",
		"action=allow",
		"program=\"" + program + "\"",
		"protocol=TCP",
		fmt.Sprintf("localport=%d", port),
		"profile=" + profiles,
		"remoteip=" + remoteIP,
		"enable=yes",
	}
}

func (f *netshFirewall) Ensure(port int, program string) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid port %d", port)
	}
	if strings.TrimSpace(program) == "" {
		return fmt.Errorf("program path is required (the rule scopes to the outpost binary)")
	}
	ctx := context.Background()
	run := f.runner()
	public := sshdFirewallPlan(f.activeProfile(ctx))

	if err := f.ensureRule(ctx, run, sshdFirewallRuleName,
		sshdRuleArgs(sshdFirewallRuleName, program, port, "private,domain", "any")); err != nil {
		return err
	}
	if public {
		if err := f.ensureRule(ctx, run, sshdFirewallPublicRuleName,
			sshdRuleArgs(sshdFirewallPublicRuleName, program, port, "public", "localsubnet")); err != nil {
			return err
		}
	} else if err := f.removeRule(ctx, run, sshdFirewallPublicRuleName); err != nil {
		return err
	}
	return nil
}

// ensureRule converges one rule: present → set-rule-new (in place, no
// gap); absent → add. A show failure that is NOT "no rules match" means
// netsh itself is broken (or not elevated for reads) — return that exact
// reason instead of blindly adding a duplicate.
func (f *netshFirewall) ensureRule(ctx context.Context, run netshRunner, name string, tail []string) error {
	out, err := run(ctx, "advfirewall", "firewall", "show", "rule", "name=\""+name+"\"")
	switch {
	case err == nil || strings.Contains(out, "Rule Name:"):
		setArgs := append([]string{"advfirewall", "firewall", "set", "rule", "name=\"" + name + "\"", "new"}, tail...)
		if out, serr := run(ctx, setArgs...); serr != nil {
			return fmt.Errorf("set rule %q: %w: %s", name, serr, strings.TrimSpace(out))
		}
		return nil
	case strings.Contains(out, "No rules match"):
		addArgs := append([]string{"advfirewall", "firewall", "add", "rule"}, tail...)
		if out, aerr := run(ctx, addArgs...); aerr != nil {
			return fmt.Errorf("add rule %q: %w: %s", name, aerr, strings.TrimSpace(out))
		}
		return nil
	default:
		return fmt.Errorf("show rule %q: %w: %s", name, err, strings.TrimSpace(out))
	}
}

// removeRule deletes one rule; an absent rule is success. A broken show
// is an error (the rule may linger — the caller warns).
func (f *netshFirewall) removeRule(ctx context.Context, run netshRunner, name string) error {
	out, err := run(ctx, "advfirewall", "firewall", "show", "rule", "name=\""+name+"\"")
	switch {
	case strings.Contains(out, "Rule Name:"):
		if out, derr := run(ctx, "advfirewall", "firewall", "delete", "rule", "name=\""+name+"\""); derr != nil {
			return fmt.Errorf("delete rule %q: %w: %s", name, derr, strings.TrimSpace(out))
		}
		return nil
	case strings.Contains(out, "No rules match"):
		return nil
	default:
		return fmt.Errorf("show rule %q: %w: %s", name, err, strings.TrimSpace(out))
	}
}

func (f *netshFirewall) Remove() error {
	ctx := context.Background()
	run := f.runner()
	if err := f.removeRule(ctx, run, sshdFirewallRuleName); err != nil {
		return err
	}
	return f.removeRule(ctx, run, sshdFirewallPublicRuleName)
}
