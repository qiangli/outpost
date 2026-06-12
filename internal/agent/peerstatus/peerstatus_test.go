package peerstatus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetch_OK(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"peers":[
			{"host":"box1","online":true,"location":"same_lan","version":"v0.7.2","os":"linux","arch":"amd64","owned":true},
			{"host":"box2","online":false,"location":"remote","shared":true}
		]}`))
	}))
	defer srv.Close()

	peers, err := Fetch(context.Background(), srv.URL, "tok", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("len=%d, want 2", len(peers))
	}
	if p := peers[0]; p.Host != "box1" || !p.Online || p.Location != "same_lan" || p.Version != "v0.7.2" || p.OS != "linux" || p.Arch != "amd64" || !p.Owned {
		t.Errorf("peer0 = %+v", p)
	}
	if p := peers[1]; p.Host != "box2" || p.Online || !p.Shared || p.Location != "remote" {
		t.Errorf("peer1 = %+v", p)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
	}
	if gotPath != "/api/v1/peers" {
		t.Errorf("path = %q, want /api/v1/peers", gotPath)
	}
}

func TestFetch_UnpairedGuards(t *testing.T) {
	if _, err := Fetch(context.Background(), "", "tok", nil); err == nil {
		t.Error("want error on empty base")
	}
	if _, err := Fetch(context.Background(), "https://example.com", "", nil); err == nil {
		t.Error("want error on empty token")
	}
}

func TestFetch_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("insufficient scope"))
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.URL, "tok", nil); err == nil {
		t.Error("want error on 403")
	}
}
