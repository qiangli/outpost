package admincore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// fakeMaster is an outpost-local httptest of the cloudbox master's pair-once
// contract (yoke docs/cloudbox-members-attach-wire.md §3, mirrored by yoke
// pkg/fleet/cloudboxfake): POST /api/v1/hosts/pair redeems a code into an
// identity record + host token with tunnel:false, DELETE /api/v1/hosts/self
// revokes it. It serves no /api/register/exchange — the master can't.
type fakeMaster struct {
	*httptest.Server
	mu      sync.Mutex
	codes   map[string]string // code -> host it was redeemed for ("" = fresh)
	tokens  map[string]string // host token -> host
	deletes int
}

func newFakeMaster(t *testing.T, codes ...string) *fakeMaster {
	t.Helper()
	f := &fakeMaster{codes: map[string]string{}, tokens: map[string]string{}}
	for _, c := range codes {
		f.codes[c] = ""
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMaster) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": code})
	}
	switch {
	case r.URL.Path == "/api/v1/hosts/pair" && r.Method == http.MethodPost:
		var body struct{ Code, Host, Hostname, OS, Arch string }
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || body.Code == "" || body.Host == "" {
			fail(400, "bad_request")
			return
		}
		used, ok := f.codes[body.Code]
		if !ok {
			fail(401, "invalid_code")
			return
		}
		if used != "" {
			fail(409, "code_consumed")
			return
		}
		f.codes[body.Code] = body.Host
		tok := "htok-" + body.Host
		f.tokens[tok] = body.Host
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"host": map[string]any{"host": body.Host, "hostname": body.Hostname, "os": body.OS, "arch": body.Arch,
				"owner_email": "owner@example.com", "paired_at": time.Date(2026, 11, 9, 18, 10, 0, 0, time.UTC)},
			"token": tok, "scopes": []string{"host"}, "tunnel": false,
		})
	case r.URL.Path == "/api/v1/hosts/self" && r.Method == http.MethodDelete:
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, ok := f.tokens[tok]; !ok {
			fail(401, "unauthorized")
			return
		}
		delete(f.tokens, tok)
		f.deletes++
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

// Pairing with the master stores the identity in the `cloudbox` block, opens
// no tunnel (top-level pairing untouched, no restart) and never overwrites
// attachments.
func TestPairWithMasterIsIdentityOnly(t *testing.T) {
	s := newTestServer(t)
	var restarts atomic.Int32
	s.deps.Restart = func() { restarts.Add(1) }
	seed := &conf.FileConfig{
		AgentName: "studio", ServerAddr: "sat.example.com", ServerPort: 443, Token: "matrix", AccessToken: "legacy-access",
		Attachments: []conf.SatelliteAttachment{{SatelliteID: "sat_1", Endpoint: "https://sat.example.com", Token: "stok"}},
	}
	if err := conf.SaveFile(s.deps.ConfigPath, seed); err != nil {
		t.Fatal(err)
	}
	m := newFakeMaster(t, "K7QX-2MPD")
	res, err := s.Pair(context.Background(), PairParams{Server: m.URL + "/", Code: "K7QX-2MPD", Name: "studio"})
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if !res.IdentityOnly || res.RestartPending || res.OwnerEmail != "owner@example.com" {
		t.Errorf("result = %+v, want identity-only, no restart", res)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	c := fc.Cloudbox
	if c == nil || c.PortalURL != m.URL || c.Host != "studio" || c.Token != "htok-studio" || c.OwnerEmail != "owner@example.com" {
		t.Fatalf("cloudbox identity = %+v", c)
	}
	if fc.ServerAddr != "sat.example.com" || fc.Token != "matrix" || fc.AccessToken != "legacy-access" {
		t.Errorf("identity pairing touched the tunnel pairing: %+v", fc)
	}
	if len(fc.Attachments) != 1 || fc.Attachments[0].Token != "stok" {
		t.Errorf("identity pairing touched attachments: %+v", fc.Attachments)
	}
	time.Sleep(1200 * time.Millisecond) // past the restart debounce
	if restarts.Load() != 0 {
		t.Errorf("identity-only pairing scheduled a restart")
	}
	if v, _ := s.SafeView(); v.Cloudbox == nil || !v.Cloudbox.HasToken || len(v.Attachments) != 1 {
		t.Errorf("SafeView cloudbox=%+v attachments=%+v", v.Cloudbox, v.Attachments)
	}

	// Pair once: a second name against the same master is refused before
	// any code is spent.
	if _, err := s.Pair(context.Background(), PairParams{Server: m.URL, Code: "K7QX-2MPD", Name: "other"}); err == nil || !strings.Contains(err.Error(), "unpair") {
		t.Errorf("second pairing under another name: err=%v, want refusal", err)
	}
}

// A server without /api/v1/hosts/pair (a satellite) pairs over the legacy
// exchange exactly as before.
func TestPairWithSatelliteFallsBackToLegacyExchange(t *testing.T) {
	s := newTestServer(t)
	srv := fakeExchange(t, "sat.example.com")
	res, err := s.Pair(context.Background(), PairParams{Server: srv.URL, Code: "code", Name: "worker"})
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if res.IdentityOnly {
		t.Errorf("legacy pairing reported identity-only")
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if fc.ServerAddr != "sat.example.com" || fc.AccessToken != "access-token" || fc.Cloudbox != nil {
		t.Errorf("legacy pairing not applied: %+v", fc)
	}
}

// A contract error (bad code) surfaces; it does not fall back.
func TestPairWithMasterBadCode(t *testing.T) {
	s := newTestServer(t)
	m := newFakeMaster(t)
	_, err := s.Pair(context.Background(), PairParams{Server: m.URL, Code: "NOPE", Name: "studio"})
	if err == nil || !strings.Contains(err.Error(), "invalid_code") {
		t.Fatalf("err = %v, want invalid_code", err)
	}
}

// Unpair deletes the identity at the master, then forgets identity,
// attachments and tunnel pairing; a second unpair is a no-op.
func TestUnpairDeletesCloudboxIdentity(t *testing.T) {
	s := newTestServer(t)
	m := newFakeMaster(t, "C1")
	if _, err := s.Pair(context.Background(), PairParams{Server: m.URL, Code: "C1", Name: "studio"}); err != nil {
		t.Fatal(err)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	fc.Attachments = []conf.SatelliteAttachment{{SatelliteID: "sat_1", Endpoint: "https://sat.example.com", Token: "stok"}}
	fc.AgentName, fc.AccessToken = "studio", "legacy"
	_ = conf.SaveFile(s.deps.ConfigPath, fc)

	if _, err := s.Unpair(context.Background()); err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	if m.deletes != 1 {
		t.Errorf("DELETE /api/v1/hosts/self calls = %d, want 1", m.deletes)
	}
	fc, _ = conf.LoadFile(s.deps.ConfigPath)
	if fc.Cloudbox != nil || fc.Attachments != nil || fc.AgentName != "" || fc.AccessToken != "" {
		t.Errorf("unpair left pairing state: %+v", fc)
	}
	if _, err := s.Unpair(context.Background()); err != nil {
		t.Errorf("second Unpair: %v", err)
	}
}

// A revoked token (401) counts as done; any other failure keeps the
// identity so the host is never forgotten while the master still lists it.
func TestUnpairMasterFailures(t *testing.T) {
	s := newTestServer(t)
	m := newFakeMaster(t)
	_ = conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{Cloudbox: &conf.CloudboxPairing{PortalURL: m.URL, Host: "studio", Token: "revoked"}})
	if _, err := s.Unpair(context.Background()); err != nil {
		t.Fatalf("Unpair with revoked token: %v", err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer down.Close()
	_ = conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{Cloudbox: &conf.CloudboxPairing{PortalURL: down.URL, Host: "studio", Token: "t"}})
	if _, err := s.Unpair(context.Background()); err == nil {
		t.Fatal("Unpair succeeded against a failing master")
	}
	if fc, _ := conf.LoadFile(s.deps.ConfigPath); !fc.CloudboxPaired() {
		t.Errorf("identity dropped although the master delete failed")
	}
}
