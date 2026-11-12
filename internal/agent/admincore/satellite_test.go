package admincore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// satFake is an outpost-local httptest of the satellite-attach contract
// (yoke docs/cloudbox-members-attach-wire.md §4, mirrored by yoke
// pkg/fleet/cloudboxfake): a master serving the host-token satellite list
// and attach/detach, plus satellites redeeming attach tokens. Like the
// cloudbox satellite, the redeem reply carries `registration` (the
// /api/register/exchange tunnel fields).
type satFake struct {
	master   *httptest.Server
	sats     map[string]*httptest.Server // id -> satellite
	names    map[string]string           // id -> name
	mu       sync.Mutex
	attached map[string]bool   // satellite id -> attached
	pending  map[string]string // attach token -> satellite id
	satTok   map[string]string // satellite host token -> satellite id
	calls    []string
}

const fakeHostToken = "htok-studio"

func newSatFake(t *testing.T, sats ...string) *satFake {
	t.Helper()
	f := &satFake{sats: map[string]*httptest.Server{}, names: map[string]string{}, attached: map[string]bool{},
		pending: map[string]string{}, satTok: map[string]string{}}
	f.master = httptest.NewServer(http.HandlerFunc(f.serveMaster))
	t.Cleanup(f.master.Close)
	for i, name := range sats {
		id := "sat_" + string(rune('1'+i))
		f.names[id] = name
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serveSat(id, w, r) }))
		t.Cleanup(srv.Close)
		f.sats[id] = srv
	}
	return f
}

func fail(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": code})
}

func (f *satFake) serveMaster(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "master "+r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+fakeHostToken {
		fail(w, 401, "unauthorized")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/api/v1/satellites" && r.Method == http.MethodGet:
		rows := []map[string]any{}
		for id, srv := range f.sats {
			rows = append(rows, map[string]any{"id": id, "name": f.names[id], "endpoint": srv.URL, "state": "running", "attached": f.attached[id]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"satellites": rows})
	case strings.HasPrefix(p, "/api/v1/satellites/") && strings.HasSuffix(p, "/attach"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/satellites/"), "/attach")
		srv, ok := f.sats[id]
		if !ok {
			fail(w, 404, "not_found")
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.attached, id)
			w.WriteHeader(204)
			return
		}
		tok := "atok-" + id
		f.pending[tok] = id
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"attach_token": tok, "expires_at": "2026-11-10T00:10:00Z",
			"satellite": map[string]any{"id": id, "name": f.names[id], "endpoint": srv.URL}, "host": "studio"})
	default:
		http.NotFound(w, r)
	}
}

func (f *satFake) serveSat(id string, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id+" "+r.Method+" "+r.URL.Path)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.URL.Path == "/api/v1/hosts/attach" && r.Method == http.MethodPost:
		if f.pending[tok] != id {
			fail(w, 401, "invalid_attach_token")
			return
		}
		var body struct{ Host, Hostname, OS, Arch string }
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			fail(w, 400, "bad_request")
			return
		}
		if body.Host != "studio" {
			fail(w, 422, "host_mismatch")
			return
		}
		delete(f.pending, tok)
		st := "stok-" + id
		f.satTok[st] = id
		f.attached[id] = true
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"host": map[string]any{"host": "studio", "attached_at": "2026-11-10T00:00:00Z"}, "token": st,
			"scopes": []string{"satellite-host"}, "satellite_id": id,
			"registration": map[string]any{"agent_name": "studio", "server_addr": id + ".example.com", "server_port": 443,
				"protocol": "wss", "token": "matrix-" + id, "remote_port": 7001, "access_token": st},
		})
	case r.URL.Path == "/api/v1/hosts/self" && r.Method == http.MethodDelete:
		if f.satTok[tok] != id {
			fail(w, 401, "unauthorized")
			return
		}
		delete(f.satTok, tok)
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

func pairedServer(t *testing.T, f *satFake, legacy bool) *Server {
	t.Helper()
	s := newTestServer(t)
	fc := &conf.FileConfig{Cloudbox: &conf.CloudboxPairing{PortalURL: f.master.URL, Host: "studio", Token: fakeHostToken}}
	if legacy {
		fc.AgentName, fc.ServerAddr, fc.AccessToken, fc.Token = "studio", "old.example.com", "legacy", "m"
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSatelliteAttachListDetach(t *testing.T) {
	f := newSatFake(t, "home", "work")
	s := pairedServer(t, f, false)
	ctx := context.Background()

	l, err := s.SatelliteList(ctx)
	if err != nil || len(l.Satellites) != 2 {
		t.Fatalf("list = %+v, %v", l, err)
	}

	// Dry run sends nothing.
	n := len(f.calls)
	dr, err := s.SatelliteAttach(ctx, SatelliteAttachParams{Satellite: "sat_1", DryRun: true})
	if err != nil || !dr.DryRun || len(dr.Requests) != 2 || len(f.calls) != n {
		t.Fatalf("dry run = %+v, %v (calls %d -> %d)", dr, err, n, len(f.calls))
	}

	// Attach by name: no pairing code, tunnel registration stored.
	res, err := s.SatelliteAttach(ctx, SatelliteAttachParams{Satellite: "home"})
	if err != nil || res.SatelliteID != "sat_1" || !res.Tunnel || !res.RestartPending {
		t.Fatalf("attach = %+v, %v", res, err)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if len(fc.Attachments) != 1 || fc.Attachments[0].Token != "stok-sat_1" || fc.Attachments[0].Tunnel == nil ||
		fc.Attachments[0].Tunnel.ServerAddr != "sat_1.example.com" {
		t.Fatalf("attachments = %+v", fc.Attachments)
	}
	if fc.LegacyPaired() || fc.Cloudbox.Token != fakeHostToken {
		t.Errorf("attach touched the identity or the top-level pairing: %+v", fc)
	}
	l, _ = s.SatelliteList(ctx)
	for _, row := range l.Satellites {
		if (row.ID == "sat_1") != (row.Attached && row.Local) {
			t.Errorf("row %+v", row)
		}
	}

	// Re-attach refreshes its entry; a second satellite is added.
	if _, err := s.SatelliteAttach(ctx, SatelliteAttachParams{Satellite: "sat_1"}); err != nil {
		t.Errorf("re-attach: %v", err)
	}
	if _, err := s.SatelliteAttach(ctx, SatelliteAttachParams{Satellite: "work"}); err != nil {
		t.Fatalf("second attach: %v", err)
	}
	fc, _ = conf.LoadFile(s.deps.ConfigPath)
	if len(fc.Attachments) != 2 || fc.Attachments[0].SatelliteID != "sat_1" || fc.Attachments[1].SatelliteID != "sat_2" {
		t.Fatalf("attachments after restart load = %+v", fc.Attachments)
	}
	l, _ = s.SatelliteList(ctx)
	for _, row := range l.Satellites {
		if !row.Attached || !row.Local {
			t.Errorf("row %+v", row)
		}
	}
	if _, err := s.SatelliteDetach(ctx, SatelliteDetachParams{DryRun: true}); err == nil {
		t.Fatal("ambiguous detach should require a satellite")
	}

	// Detach one: dry run, then both DELETEs, other attachment retained.
	dd, err := s.SatelliteDetach(ctx, SatelliteDetachParams{Satellite: "home", DryRun: true})
	if err != nil || len(dd.Requests) != 2 {
		t.Fatalf("detach dry run = %+v, %v", dd, err)
	}
	n = len(f.calls)
	d, err := s.SatelliteDetach(ctx, SatelliteDetachParams{Satellite: "sat_1"})
	if err != nil || !d.RestartPending || len(d.Warnings) != 0 {
		t.Fatalf("detach = %+v, %v", d, err)
	}
	got := strings.Join(f.calls[n:], ",")
	if !strings.Contains(got, "sat_1 DELETE /api/v1/hosts/self") || !strings.Contains(got, "master DELETE /api/v1/satellites/sat_1/attach") {
		t.Errorf("detach calls = %s", got)
	}
	fc, _ = conf.LoadFile(s.deps.ConfigPath)
	if len(fc.Attachments) != 1 || fc.Attachments[0].SatelliteID != "sat_2" || !fc.CloudboxPaired() {
		t.Errorf("after detach: %+v", fc)
	}
	if _, err := s.SatelliteDetach(ctx, SatelliteDetachParams{}); err != nil {
		t.Errorf("detach remaining satellite: %v", err)
	}
}

// A legacy satellite-paired host is refused unless it migrates; migrating
// replaces the top-level pairing with the attachment in one step.
func TestSatelliteAttachLegacyMigrate(t *testing.T) {
	f := newSatFake(t, "home")
	s := pairedServer(t, f, true)
	ctx := context.Background()
	if _, err := s.SatelliteAttach(ctx, SatelliteAttachParams{Satellite: "sat_1"}); err == nil || !strings.Contains(err.Error(), "--migrate") {
		t.Fatalf("legacy attach err = %v", err)
	}
	res, err := s.SatelliteAttach(ctx, SatelliteAttachParams{Satellite: "sat_1", Migrate: true})
	if err != nil || !res.Migrated {
		t.Fatalf("migrate = %+v, %v", res, err)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if fc.LegacyPaired() || len(fc.Attachments) != 1 {
		t.Fatalf("after migrate: %+v", fc)
	}
}

func TestSatelliteNeedsCloudboxPairing(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.SatelliteList(context.Background()); err == nil || !strings.Contains(err.Error(), "register") {
		t.Fatalf("err = %v", err)
	}
}
