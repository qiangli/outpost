package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent"
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
	if _, err := peerUpgradeTarget("peer-placeholder", targets); err != nil {
		t.Fatal(err)
	}
}

func TestPeerRepairRestartVerification(t *testing.T) {
	for _, tt := range []struct {
		name, version, want string
		unreachable         bool
	}{
		{name: "target after restart", version: "v1.1.0"},
		{name: "old after timeout", version: "v1.0.0", want: "rolled back or not applied"},
		{name: "unreachable", unreachable: true, want: "unreachable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := upgrade.Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64)}
			peer := peerstatus.Peer{Host: "peer-a", Owned: true, Version: "v1.0.0"}
			ledger := upgrade.RepairLedger{Path: filepath.Join(t.TempDir(), "heals.jsonl"), Peer: peer.Host}
			if err := ledger.Observe(agent.BuildInfo{Version: peer.Version}, env, false, "", false); err != nil {
				t.Fatal(err)
			}
			sent, queries := 0, 0
			run := func(ctx context.Context, host, command string) (string, error) {
				if strings.HasPrefix(command, "outpost upgrade") {
					sent++
					return "", peerRepairUncertain{errors.New("session dropped")}
				}
				queries++
				if queries == 1 || tt.unreachable {
					return "", errors.New("connection unavailable")
				}
				return `{"status":{"build":{"version":"` + tt.version + `"}}}`, nil
			}
			err := finishPeerRepair(context.Background(), peer, env, ledger, run, 30*time.Millisecond, time.Millisecond)
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("error=%v", err)
			}
			if sent != 1 || queries < 2 {
				t.Fatalf("sent=%d queries=%d", sent, queries)
			}
			rows, err := agent.ReadHealRecords(ledger.Path)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows=%v", rows)
			}
			row := rows[0]
			evidence, err := json.Marshal(row.Evidence)
			if err != nil {
				t.Fatal(err)
			}
			wantResult := "healed"
			if tt.want != "" {
				wantResult = "failed"
			}
			if row.Result != wantResult || row.Rung != "upgrade" || row.Before != peer.Version || row.After != env.ReleaseID || !strings.Contains(string(evidence), env.SHA256) {
				t.Fatalf("record=%+v", row)
			}
			if tt.want != "" && !strings.Contains(row.Reason, tt.want) {
				t.Fatal(row.Reason)
			}
		})
	}
}

func TestPeerUpgradeMissingAliasGuidance(t *testing.T) {
	_, err := peerUpgradeTarget("peer-a", nil)
	if err == nil || err.Error() != "no SSH alias for peer-a; add one with: outpost ssh add peer-a --host peer-a" {
		t.Fatalf("error=%v", err)
	}
}

func TestPeerRepairExecResult(t *testing.T) {
	for _, tt := range []struct {
		code      int
		stderr    string
		uncertain bool
	}{
		{-1, "", true}, {-1, " ", true}, {-1, "upgrade rejected", false}, {1, "", false}, {0, "", false},
	} {
		_, err := peerRepairExecResult(sshExecResult{ExitCode: tt.code, Stderr: tt.stderr})
		var uncertain peerRepairUncertain
		if errors.As(err, &uncertain) != tt.uncertain || (err == nil) != (tt.code == 0) {
			t.Fatalf("code=%d stderr=%q error=%v", tt.code, tt.stderr, err)
		}
	}
}

func TestPeerRepairDefiniteFailureDoesNotPoll(t *testing.T) {
	env := upgrade.Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64)}
	calls := 0
	cause := errors.New("upgrade rejected")
	err := applyPeerRepair(context.Background(), peerstatus.Peer{Host: "peer-a", Owned: true}, env, func(context.Context, string, string) (string, error) { calls++; return "", cause })
	if !errors.Is(err, cause) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

// Live shape: a release v1.0.0-rc.23 runs as "v1.0.0-rc.23-dev" (S549.R5b
// live run). Identity is the build commit when both sides carry one.
func TestPeerRunsTargetByCommit(t *testing.T) {
	env := upgrade.Envelope{ReleaseID: "v1.1.0", Commit: "4da588d"}
	cases := []struct {
		name, version, commit string
		want                  bool
	}{
		{"dev-suffixed release, same commit", "v1.1.0-dev", "4da588d684ac143f5ca2b9bdd9f231b83daff5b3", true},
		{"same tag, different commit", "v1.1.0", "0123456789abcdef", false},
		{"old build", "v1.0.0-dev", "fedcba9876543210", false},
	}
	for _, c := range cases {
		if got := peerRunsTarget(c.version, c.commit, env); got != c.want {
			t.Errorf("%s: peerRunsTarget(%q,%q) = %v, want %v", c.name, c.version, c.commit, got, c.want)
		}
	}
	noCommit := upgrade.Envelope{ReleaseID: "v1.1.0"}
	if !peerRunsTarget("v1.1.0-dev", "", noCommit) {
		t.Error("without commits a -dev suffixed release must match its tag")
	}
	if peerRunsTarget("v1.1.0-rc.1", "", noCommit) {
		t.Error("a different prerelease must not match")
	}
}
