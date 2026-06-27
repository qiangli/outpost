package peerplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_AnnounceConnectInbox(t *testing.T) {
	var gotAnnounce map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/peer/announce":
			_ = json.NewDecoder(r.Body).Decode(&gotAnnounce)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/v1/peer/connect":
			_, _ = w.Write([]byte(`{"peer":{"host":"beta","candidates":["10.0.0.2:9000","169.254.1.2:9000"],"external_ip":"1.2.3.4"},"same_lan":true}`))
		case "/api/v1/peer/inbox":
			_, _ = w.Write([]byte(`{"rendezvous":[{"from_host":"gamma","from_candidates":["192.168.1.5:9000"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "tok"}
	ctx := context.Background()

	if err := c.Announce(ctx, "alpha", "", []string{"10.0.0.1:9000", "169.254.1.1:9000"}, nil); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if gotAnnounce["candidates"] != "10.0.0.1:9000,169.254.1.1:9000" {
		t.Errorf("announced candidates=%v", gotAnnounce["candidates"])
	}

	tgt, err := c.Connect(ctx, "alpha", "beta")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if tgt.Peer.Host != "beta" || len(tgt.Peer.Candidates) != 2 || !tgt.SameLAN {
		t.Errorf("target=%+v", tgt)
	}

	box, err := c.Inbox(ctx, "alpha")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(box) != 1 || box[0].FromHost != "gamma" || len(box[0].FromCandidates) != 1 {
		t.Errorf("inbox=%+v", box)
	}
}

func TestClient_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "x"}
	if _, err := c.Connect(context.Background(), "a", "b"); err == nil {
		t.Fatalf("expected error on 403")
	}
}
