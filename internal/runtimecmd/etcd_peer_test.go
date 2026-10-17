package runtimecmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReconcileEtcdPeer(t *testing.T) {
	const peer = "https://192.0.2.2:2380"
	for _, tc := range []struct {
		name         string
		members      []etcdMember
		update, fail bool
	}{
		{"changed", []etcdMember{{ID: "18446744073709551615", Name: "local", PeerURLs: []string{"https://192.0.2.1:2380"}}}, true, false},
		{"unchanged", []etcdMember{{ID: "1", Name: "local", PeerURLs: []string{peer}}}, false, false},
		{"empty", nil, false, true},
		{"other member", []etcdMember{{ID: "1", Name: "other"}}, false, true},
		{"learner", []etcdMember{{ID: "1", Name: "local", IsLearner: true}}, false, true},
		{"multiple", []etcdMember{{ID: "1", Name: "local"}, {ID: "2", Name: "other"}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updates := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Errorf("method %s", r.Method)
				}
				members := tc.members
				switch r.URL.Path {
				case "/v3/cluster/member/list":
				case "/v3/cluster/member/update":
					updates++
					var body etcdMember
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.ID != tc.members[0].ID || len(body.PeerURLs) != 1 || body.PeerURLs[0] != peer {
						t.Errorf("update %+v", body)
					}
					members = []etcdMember{body}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				json.NewEncoder(w).Encode(map[string]any{"members": members})
			}))
			defer srv.Close()
			err := reconcileEtcdPeer(context.Background(), srv.Client(), srv.URL, "local", peer)
			if (err != nil) != tc.fail {
				t.Fatalf("error %v, want failure %v", err, tc.fail)
			}
			if (updates == 1) != tc.update {
				t.Errorf("updates %d", updates)
			}
		})
	}
}

func TestReconcileEtcdPeerFailsClosed(t *testing.T) {
	for _, response := range []string{`{"error":"unavailable"}`, `not json`} {
		t.Run(response, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(response)) }))
			defer srv.Close()
			if err := reconcileEtcdPeer(context.Background(), srv.Client(), srv.URL, "local", "https://192.0.2.2:2380"); err == nil {
				t.Fatal("accepted failed membership response")
			}
		})
	}
}
