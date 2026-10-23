package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The rule builder must emit the exact netsh shape: scoped to the program,
// TCP, the declared port, the right profiles. No real netsh runs here.
func TestSSHDFirewallRuleArgs(t *testing.T) {
	got := sshdRuleArgs("outpost sshd", `C:\Program Files\outpost\outpost.exe`, 2222, "private,domain", "any")
	joined := strings.Join(got, " ")
	for _, want := range []string{
		`name="outpost sshd"`,
		`dir=in`,
		`action=allow`,
		`program="C:\Program Files\outpost\outpost.exe"`,
		`protocol=TCP`,
		`localport=2222`,
		`profile=private,domain`,
		`remoteip=any`,
		`enable=yes`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("rule args %q missing %q", joined, want)
		}
	}
	got = sshdRuleArgs("outpost sshd (public)", `C:\outpost.exe`, 2222, "public", "localsubnet")
	joined = strings.Join(got, " ")
	for _, want := range []string{`profile=public`, `remoteip=localsubnet`, `localport=2222`} {
		if !strings.Contains(joined, want) {
			t.Errorf("public rule args %q missing %q", joined, want)
		}
	}
}

// The Public/LocalSubnet decision: the public rule (subnet-scoped) is
// ensured while on Public or an unknown profile, and withdrawn on
// Private/Domain.
func TestSSHDFirewallPlan(t *testing.T) {
	cases := []struct {
		active networkProfile
		public bool
	}{
		{profilePublic, true},
		{profileUnknown, true},
		{profilePrivate, false},
		{profileDomain, false},
	}
	for _, c := range cases {
		if got := sshdFirewallPlan(c.active); got != c.public {
			t.Errorf("plan(%d) = %v, want %v", c.active, got, c.public)
		}
	}
}

// The currentprofile probe parses the profile off the first line and maps
// anything unrecognized to unknown (which keeps subnet-scoped coverage).
func TestParseCurrentProfile(t *testing.T) {
	cases := []struct {
		out  string
		want networkProfile
	}{
		{"Public Profile Settings:\n----------------------------------------------------------------------\nState ON", profilePublic},
		{"Private Profile Settings:\nState ON", profilePrivate},
		{"Domain Profile Settings:\nState ON", profileDomain},
		{"something unexpected", profileUnknown},
		{"", profileUnknown},
	}
	for _, c := range cases {
		if got := parseCurrentProfile(c.out); got != c.want {
			t.Errorf("parse(%q) = %d, want %d", c.out, got, c.want)
		}
	}
}

// fakeNetsh records invocations and answers from a script. No real netsh.
type fakeNetsh struct {
	calls [][]string
	show  map[string]string // rule name → show output; missing = "No rules match"
	fail  map[string]error  // verb prefix → error
}

const netshRuleDump = `Rule Name:                            outpost sshd
----------------------------------------------------------------------
Enabled:                              Yes
Direction:                            In
Profiles:                             Private,Domain
Action:                               Allow
`

func (f *fakeNetsh) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for prefix, err := range f.fail {
		if strings.HasPrefix(joined, prefix) && err != nil {
			return "Access is denied.", err
		}
	}
	if strings.Contains(joined, " show rule ") {
		name := netshRuleNameOf(joined)
		if dump, ok := f.show[name]; ok {
			return dump, nil
		}
		return "No rules match the specified criteria.", errors.New("exit status 1")
	}
	return "Ok.", nil
}

func netshRuleNameOf(joined string) string {
	for _, name := range []string{sshdFirewallPublicRuleName, sshdFirewallRuleName} {
		if strings.Contains(joined, name) {
			return name
		}
	}
	return ""
}

func (f *fakeNetsh) verbs() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		for i, a := range c {
			if a == "show" || a == "add" || a == "set" || a == "delete" {
				rest := ""
				if i+1 < len(c) {
					rest = " " + c[i+1]
				}
				out = append(out, a+rest)
				break
			}
		}
	}
	return out
}

// Ensure on a Public host converges both rules: base via add (missing),
// public via add (missing).
func TestSSHDFirewallEnsurePublic(t *testing.T) {
	fake := &fakeNetsh{show: map[string]string{}}
	fw := &netshFirewall{run: fake.run, profile: func(context.Context) (networkProfile, error) {
		return profilePublic, nil
	}}
	if err := fw.Ensure(2222, `C:\outpost.exe`); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	verbs := fake.verbs()
	var adds int
	for _, v := range verbs {
		if strings.HasPrefix(v, "add") {
			adds++
		}
	}
	if adds != 2 {
		t.Errorf("verbs = %v, want 2 adds (base + public)", verbs)
	}
	joined := strings.Join([]string{strings.Join(flattenCalls(fake.calls), " ")}, " ")
	if !strings.Contains(joined, "localsubnet") {
		t.Errorf("no localsubnet-scoped public rule in %v", fake.calls)
	}
}

// Ensure on a Private host converges the base rule and withdraws a stale
// public rule left over from a network change.
func TestSSHDFirewallEnsurePrivateWithdrawsPublic(t *testing.T) {
	fake := &fakeNetsh{show: map[string]string{
		sshdFirewallRuleName:       netshRuleDump,
		sshdFirewallPublicRuleName: netshRuleDump,
	}}
	fw := &netshFirewall{run: fake.run, profile: func(context.Context) (networkProfile, error) {
		return profilePrivate, nil
	}}
	if err := fw.Ensure(2222, `C:\outpost.exe`); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	verbs := strings.Join(fake.verbs(), ",")
	if !strings.Contains(verbs, "set") {
		t.Errorf("base rule not converged in place: %v", fake.verbs())
	}
	if !strings.Contains(verbs, "delete") {
		t.Errorf("stale public rule not withdrawn: %v", fake.verbs())
	}
}

// Present rules converge via set (no delete+add gap); absent rules add.
func TestSSHDFirewallEnsureIdempotent(t *testing.T) {
	fake := &fakeNetsh{show: map[string]string{sshdFirewallRuleName: netshRuleDump}}
	fw := &netshFirewall{run: fake.run, profile: func(context.Context) (networkProfile, error) {
		return profileDomain, nil
	}}
	if err := fw.Ensure(2222, `C:\outpost.exe`); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, v := range fake.verbs() {
		if strings.HasPrefix(v, "add") || strings.HasPrefix(v, "delete rule name=\"outpost sshd\"") {
			t.Errorf("converged rule was added/deleted instead of set: %v", fake.verbs())
		}
	}
}

// A refusing netsh (no admin) returns the exact reason — the unit reports
// it as degraded.
func TestSSHDFirewallEnsureDenied(t *testing.T) {
	denied := errors.New("exit status 1")
	fake := &fakeNetsh{show: map[string]string{}, fail: map[string]error{"advfirewall firewall add": denied}}
	fw := &netshFirewall{run: fake.run, profile: func(context.Context) (networkProfile, error) {
		return profilePublic, nil
	}}
	err := fw.Ensure(2222, `C:\outpost.exe`)
	if err == nil {
		t.Fatal("Ensure with denying netsh should fail")
	}
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Errorf("error = %q, want the netsh reason", err)
	}
}

// Remove deletes both rules; absent rules are success with no delete call.
func TestSSHDFirewallRemove(t *testing.T) {
	fake := &fakeNetsh{show: map[string]string{
		sshdFirewallRuleName:       netshRuleDump,
		sshdFirewallPublicRuleName: netshRuleDump,
	}}
	fw := &netshFirewall{run: fake.run}
	if err := fw.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	var deletes int
	for _, v := range fake.verbs() {
		if strings.HasPrefix(v, "delete") {
			deletes++
		}
	}
	if deletes != 2 {
		t.Errorf("deletes = %d, want 2 (base + public)", deletes)
	}

	empty := &fakeNetsh{show: map[string]string{}}
	fw2 := &netshFirewall{run: empty.run}
	if err := fw2.Remove(); err != nil {
		t.Fatalf("Remove absent: %v", err)
	}
	for _, v := range empty.verbs() {
		if strings.HasPrefix(v, "delete") {
			t.Errorf("absent rule triggered a delete: %v", empty.verbs())
		}
	}
}

// Validation rejects garbage before touching netsh.
func TestSSHDFirewallEnsureValidation(t *testing.T) {
	fw := &netshFirewall{run: (&fakeNetsh{show: map[string]string{}}).run}
	if err := fw.Ensure(0, `C:\outpost.exe`); err == nil {
		t.Error("port 0 should fail")
	}
	if err := fw.Ensure(2222, ""); err == nil {
		t.Error("empty program should fail")
	}
}

func flattenCalls(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c...)
	}
	return out
}
