package main

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/peerstatus"
	"github.com/qiangli/outpost/internal/agent/upgrade"
)

func TestPeerRepairCommandAndOwnership(t *testing.T) {
	env := upgrade.Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64)}
	calls := 0
	run := func(_ context.Context, host, command string) (string, error) {
		calls++
		if host != "peer-placeholder" {
			t.Fatal(host)
		}
		if calls == 1 {
			if !strings.Contains(command, "upgrade --from '"+env.URL+"' --sha256 '"+env.SHA256+"'") {
				t.Fatal(command)
			}
			return "", nil
		}
		return `{"status":{"build":{"version":"v1.1.0"}}}`, nil
	}
	if err := applyPeerRepair(context.Background(), peerstatus.Peer{Host: "peer-placeholder"}, env, run); err == nil || calls != 0 {
		t.Fatal("non-owned peer executed")
	}
	if err := applyPeerRepair(context.Background(), peerstatus.Peer{Host: "peer-placeholder", Owned: true}, env, run); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestPeerUpgradeTargetBindsOwnedHost(t *testing.T) {
	targets := []admincore.SSHTargetView{{Name: "peer-placeholder", Host: "other-placeholder"}, {Name: "alias-placeholder", Host: "peer-placeholder"}}
	name, err := peerUpgradeTarget("peer-placeholder", targets)
	if err != nil || name != "alias-placeholder" {
		t.Fatalf("name=%s err=%v", name, err)
	}
	if _, err := peerUpgradeTarget("peer-placeholder", targets[:1]); err == nil {
		t.Fatal("alias collision bypassed host binding")
	}
	targets[1].Via = "hop-placeholder"
	if _, err := peerUpgradeTarget("peer-placeholder", targets); err == nil {
		t.Fatal("hop address treated as owned identity")
	}
}
