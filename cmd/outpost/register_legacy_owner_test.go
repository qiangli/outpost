package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// S606: the LEGACY pairing path (a satellite / standalone server —
// POST /api/register/exchange) makes the account that minted the code the
// OWNER of this host. register must say so plainly BEFORE the code is
// redeemed, and get consent: --as-owner-node when non-interactive
// (--code on the CLI / --no-input), a y/N answer when interactive. The
// pair-once portal path (POST /api/v1/hosts/pair) is unaffected.

// fakeSatellite stands in for a satellite: it answers the pair-once
// endpoint 404 (not the master — the code is NOT consumed there) and
// /api/register/exchange with a full legacy tunnel payload, counting
// redemptions so tests can prove the gate runs BEFORE the code is spent.
type fakeSatellite struct {
	srv       *httptest.Server
	exchanges atomic.Int32
	lastCode  atomic.Pointer[string]
}

func newFakeSatellite(t *testing.T) *fakeSatellite {
	t.Helper()
	f := &fakeSatellite{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/hosts/pair" && r.Method == http.MethodPost:
			http.NotFound(w, r) // ErrNoHostPair: fall back to the legacy exchange
		case r.URL.Path == "/api/register/exchange" && r.Method == http.MethodPost:
			f.exchanges.Add(1)
			var body struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.lastCode.Store(&body.Code)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agent_name":   "studio",
				"server_addr":  "sat.example.com",
				"server_port":  443,
				"protocol":     "wss",
				"token":        "mtok",
				"remote_port":  17006,
				"access_token": "atok",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// sandboxLegacyPair keeps doExchange's side effects off the real host:
// the config path under a temp HOME and the sshd-unit write into a temp
// fleet store.
func sandboxLegacyPair(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
}

func TestRegisterLegacyOwnerFlagsExist(t *testing.T) {
	cmd := registerCmd()
	for _, name := range []string{"as-owner-node", "no-input"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s flag not registered on registerCmd", name)
		}
	}
}

func TestRegisterNoInputRequiresCode(t *testing.T) {
	sandboxLegacyPair(t)
	cmd := registerCmd()
	cmd.SetArgs([]string{"--no-input"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--no-input requires --code") {
		t.Fatalf("err = %v, want --no-input requires --code", err)
	}
}

func TestConfirmLegacyOwner(t *testing.T) {
	t.Run("notice printed and as-owner-node proceeds", func(t *testing.T) {
		var buf bytes.Buffer
		if err := confirmLegacyOwner(&buf, nil, true, false); err != nil {
			t.Fatalf("as-owner-node: %v", err)
		}
		if s := buf.String(); !strings.Contains(s, "owned by the account that minted the code") ||
			!strings.Contains(s, "bashy paired-host invite") {
			t.Errorf("notice not printed plainly: %q", s)
		}
	})
	t.Run("non-interactive without flag is refused", func(t *testing.T) {
		var buf bytes.Buffer
		err := confirmLegacyOwner(&buf, nil, false, false)
		if err == nil || !strings.Contains(err.Error(), "--as-owner-node") ||
			!strings.Contains(err.Error(), "owned by the account that minted the code") {
			t.Fatalf("err = %v, want refusal naming --as-owner-node and the owner transfer", err)
		}
		if !strings.Contains(buf.String(), "bashy paired-host invite") {
			t.Errorf("notice missing: %q", buf.String())
		}
	})
	t.Run("interactive y proceeds", func(t *testing.T) {
		var buf bytes.Buffer
		if err := confirmLegacyOwner(&buf, bufio.NewReader(strings.NewReader("y\n")), false, true); err != nil {
			t.Fatalf("y: %v", err)
		}
	})
	t.Run("interactive n declines", func(t *testing.T) {
		var buf bytes.Buffer
		err := confirmLegacyOwner(&buf, bufio.NewReader(strings.NewReader("n\n")), false, true)
		if err != errLegacyOwnerDeclined {
			t.Fatalf("n: err = %v, want errLegacyOwnerDeclined", err)
		}
	})
	t.Run("interactive default (empty answer) declines", func(t *testing.T) {
		var buf bytes.Buffer
		err := confirmLegacyOwner(&buf, bufio.NewReader(strings.NewReader("\n")), false, true)
		if err != errLegacyOwnerDeclined {
			t.Fatalf("empty: err = %v, want errLegacyOwnerDeclined (y/N defaults to N)", err)
		}
	})
}

// RED: the legacy exchange is unreachable without consent — the one-time
// code must NOT be consumed by a refused pairing.
func TestDoPairLegacyRefusedWithoutConsent(t *testing.T) {
	sandboxLegacyPair(t)
	sat := newFakeSatellite(t)
	path := writeConfig(t, &conf.FileConfig{})

	_, err := doPair(context.Background(), sat.srv.URL, "K7QX-2MPD", "studio", "", "", path, false, "",
		func() error { return confirmLegacyOwner(&bytes.Buffer{}, nil, false, false) })
	if err == nil || !strings.Contains(err.Error(), "--as-owner-node") {
		t.Fatalf("err = %v, want non-interactive refusal", err)
	}
	if got := sat.exchanges.Load(); got != 0 {
		t.Fatalf("exchange endpoint hit %d times after refusal — the code was consumed", got)
	}
}

// GREEN: --as-owner-node consents up front; the code is redeemed exactly
// once and the legacy tunnel pairing lands in the config.
func TestDoPairLegacyAsOwnerNodeRedeems(t *testing.T) {
	sandboxLegacyPair(t)
	sat := newFakeSatellite(t)
	path := writeConfig(t, &conf.FileConfig{})

	id, err := doPair(context.Background(), sat.srv.URL, "K7QX-2MPD", "studio", "", "", path, false, "",
		func() error { return confirmLegacyOwner(&bytes.Buffer{}, nil, true, false) })
	if err != nil || id != nil {
		t.Fatalf("doPair: id=%v err=%v (want nil identity, legacy pairing)", id, err)
	}
	if got := sat.exchanges.Load(); got != 1 {
		t.Fatalf("exchange endpoint hit %d times, want 1", got)
	}
	if code := sat.lastCode.Load(); code == nil || *code != "K7QX-2MPD" {
		t.Fatalf("redeemed code = %v, want K7QX-2MPD", code)
	}
	fc, _ := conf.LoadFile(path)
	if fc.AgentName != "studio" || fc.AccessToken != "atok" || fc.ServerAddr != "sat.example.com" {
		t.Errorf("legacy pairing not persisted: %+v", fc)
	}
}

// Interactive consent: y redeems, n leaves the code unconsumed.
func TestDoPairLegacyInteractiveConsent(t *testing.T) {
	sandboxLegacyPair(t)

	t.Run("decline keeps the code", func(t *testing.T) {
		sat := newFakeSatellite(t)
		path := writeConfig(t, &conf.FileConfig{})
		_, err := doPair(context.Background(), sat.srv.URL, "K7QX-2MPD", "studio", "", "", path, false, "",
			func() error {
				return confirmLegacyOwner(&bytes.Buffer{}, bufio.NewReader(strings.NewReader("n\n")), false, true)
			})
		if err != errLegacyOwnerDeclined {
			t.Fatalf("err = %v, want errLegacyOwnerDeclined", err)
		}
		if got := sat.exchanges.Load(); got != 0 {
			t.Fatalf("exchange hit %d times after decline", got)
		}
	})

	t.Run("accept redeems", func(t *testing.T) {
		sat := newFakeSatellite(t)
		path := writeConfig(t, &conf.FileConfig{})
		_, err := doPair(context.Background(), sat.srv.URL, "K7QX-2MPD", "studio", "", "", path, false, "",
			func() error {
				return confirmLegacyOwner(&bytes.Buffer{}, bufio.NewReader(strings.NewReader("y\n")), false, true)
			})
		if err != nil {
			t.Fatalf("doPair: %v", err)
		}
		if got := sat.exchanges.Load(); got != 1 {
			t.Fatalf("exchange hit %d times, want 1", got)
		}
	})
}

// End-to-end through registerCmd's RunE: a scripted (--code) run against
// a satellite refuses without --as-owner-node and consumes nothing; with
// the flag it pairs over the legacy exchange. The pair-once portal path
// stays prompt-free (covered by TestDoPairWithMasterStoresIdentityOnly).
func TestRegisterScriptedLegacyGate(t *testing.T) {
	run := func(t *testing.T, sat *fakeSatellite, extraArgs ...string) error {
		t.Helper()
		sandboxLegacyPair(t)
		original := pairingServiceManager
		fake := &fakeUserServiceManager{}
		pairingServiceManager = fake
		t.Cleanup(func() { pairingServiceManager = original })
		cmd := registerCmd()
		args := append([]string{"--server", sat.srv.URL, "--code", "K7QX-2MPD", "--name", "studio"}, extraArgs...)
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		return cmd.Execute()
	}

	t.Run("scripted without consent refuses", func(t *testing.T) {
		sat := newFakeSatellite(t)
		err := run(t, sat)
		if err == nil || !strings.Contains(err.Error(), "--as-owner-node") {
			t.Fatalf("err = %v, want refusal naming --as-owner-node", err)
		}
		if got := sat.exchanges.Load(); got != 0 {
			t.Fatalf("exchange hit %d times — code consumed by a refused pairing", got)
		}
	})

	t.Run("scripted with --no-input also refuses without the flag", func(t *testing.T) {
		sat := newFakeSatellite(t)
		err := run(t, sat, "--no-input")
		if err == nil || !strings.Contains(err.Error(), "--as-owner-node") {
			t.Fatalf("err = %v, want refusal naming --as-owner-node", err)
		}
		if got := sat.exchanges.Load(); got != 0 {
			t.Fatalf("exchange hit %d times — code consumed by a refused pairing", got)
		}
	})

	t.Run("scripted with --as-owner-node pairs", func(t *testing.T) {
		sat := newFakeSatellite(t)
		if err := run(t, sat, "--as-owner-node"); err != nil {
			t.Fatalf("register --as-owner-node: %v", err)
		}
		if got := sat.exchanges.Load(); got != 1 {
			t.Fatalf("exchange hit %d times, want 1", got)
		}
	})
}

// The dry-run plan states the legacy gate so operators see it before any
// request is sent.
func TestRegisterDryRunNotesLegacyOwnerGate(t *testing.T) {
	plan := registerDryRunPlan("https://sat.example.com", "studio")
	reqs, _ := plan["requests"].([]string)
	if len(reqs) != 2 || !strings.Contains(reqs[1], "/api/register/exchange") {
		t.Fatalf("plan requests = %v, want the legacy exchange listed second", reqs)
	}
	if !strings.Contains(reqs[1], "owner") || !strings.Contains(reqs[1], "--as-owner-node") {
		t.Errorf("legacy request does not state the owner gate: %q", reqs[1])
	}
}
