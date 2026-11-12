package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestHostGrantSourcesOnePerAttachment(t *testing.T) {
	fc := &conf.FileConfig{
		Cloudbox: &conf.CloudboxPairing{OwnerEmail: "o@x"},
		Attachments: []conf.SatelliteAttachment{
			{SatelliteID: "one", Endpoint: "https://one.example", Token: "t1"},
			{SatelliteID: "two", Endpoint: "https://two.example", Token: "t2"},
			{SatelliteID: "notoken", Endpoint: "https://three.example"},
		},
	}
	srcs := hostGrantSources(fc)
	if len(srcs) != 2 || srcs[0].Endpoint != "https://one.example" || srcs[1].Token != "t2" {
		t.Fatalf("sources = %+v", srcs)
	}
	if got := hostGrantOwner(fc); got != "o@x" {
		t.Fatalf("owner = %q", got)
	}
	if hostGrantOwner(&conf.FileConfig{}) != "" || hostGrantSources(nil) != nil {
		t.Fatal("empty config should yield no owner/sources")
	}
}

// A legacy-paired outpost (legacy exchange to a standalone cloudbox or a
// satellite: no pair-once identity, no attachment) pulls the copy from the
// paired server with its outpost-<host> access token, and the copy's owner
// is admitted once it arrives.
func TestHostGrantLegacyPairingSource(t *testing.T) {
	cp := &agent.GrantCopy{Host: "h", Owner: "o@x", Revision: 1,
		Members: []agent.GrantMember{{Email: "m@x", Circle: "team"}, {Email: "g@x", Circle: "guest"}},
		Apps:    map[string][]agent.GrantRule{"klatch": {{Circles: []string{"team"}}}}}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/hosts/self/grants" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		if gotAuth != "Bearer outpost-h" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(cp)
	}))
	defer srv.Close()

	fc := &conf.FileConfig{AgentName: "h", PortalURL: srv.URL, AccessToken: "outpost-h", Token: "frp-secret"}
	srcs := hostGrantSources(fc)
	if len(srcs) != 1 || srcs[0].Endpoint != srv.URL || srcs[0].Token != "outpost-h" {
		t.Fatalf("sources = %+v", srcs)
	}
	store := agent.NewHostGrants(hostGrantOwner(fc))
	if store.Allow("klatch", "o@x") || store.Allow("klatch", "m@x") {
		t.Fatal("before the first pull nobody but a configured owner passes")
	}
	(&agent.GrantPuller{Sources: srcs, Store: store}).PullOnce(context.Background())
	if gotAuth != "Bearer outpost-h" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if !store.Allow("klatch", "o@x") {
		t.Fatal("owner from the pulled copy refused")
	}
	if !store.Allow("klatch", "m@x") {
		t.Fatal("member admitted by rule refused")
	}
	if store.Allow("klatch", "g@x") || store.Allow("klatch", "stranger@x") {
		t.Fatal("caller outside the rule admitted")
	}
	if store.Allow("other", "m@x") {
		t.Fatal("app with no rules must be owner only")
	}
}

// Without a persisted portal URL the legacy source falls back to the
// paired server address; the pair-once owner stays the pre-copy fallback.
func TestHostGrantLegacySourceFallbacks(t *testing.T) {
	fc := &conf.FileConfig{AgentName: "h", ServerAddr: "cb.example", ServerPort: 443, Protocol: "wss", AccessToken: "outpost-h",
		Cloudbox: &conf.CloudboxPairing{OwnerEmail: "o@x"}}
	srcs := hostGrantSources(fc)
	if len(srcs) != 1 || srcs[0].Endpoint != "https://cb.example" {
		t.Fatalf("sources = %+v", srcs)
	}
	if !agent.NewHostGrants(hostGrantOwner(fc)).Allow("klatch", "o@x") {
		t.Fatal("configured owner refused before the first pull")
	}
	if len(hostGrantSources(&conf.FileConfig{AgentName: "h", AccessToken: "outpost-h"})) != 0 {
		t.Fatal("legacy pairing with no server must not yield a source")
	}
}

// Top-level pairing fields overlaid from an attachment are not a legacy
// pairing: only the attachment itself is a source.
func TestHostGrantOverlayIsNotLegacy(t *testing.T) {
	fc := &conf.FileConfig{Attachments: []conf.SatelliteAttachment{{
		SatelliteID: "one", Endpoint: "https://one.example", Token: "t1",
		Tunnel: &conf.AttachmentTunnel{AgentName: "h", ServerAddr: "one.example", AccessToken: "t1"},
	}}}
	if !fc.ApplyAttachmentTunnel() {
		t.Fatal("overlay not applied")
	}
	srcs := hostGrantSources(fc)
	if len(srcs) != 1 || srcs[0].Name != "one" {
		t.Fatalf("sources = %+v", srcs)
	}
}

// A legacy pairing and attachments are all sources: the legacy one first.
func TestHostGrantLegacyAlongsideAttachments(t *testing.T) {
	fc := &conf.FileConfig{AgentName: "h", PortalURL: "https://cb.example", AccessToken: "outpost-h",
		Attachments: []conf.SatelliteAttachment{{SatelliteID: "one", Endpoint: "https://one.example", Token: "t1"}}}
	srcs := hostGrantSources(fc)
	if len(srcs) != 2 || srcs[0].Token != "outpost-h" || srcs[1].Name != "one" {
		t.Fatalf("sources = %+v", srcs)
	}
}
