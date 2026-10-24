package admincore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// S482, hardened by S498.4 — admincore.Pair mirrors cmd/outpost's
// mergePairing: a re-pair onto a DIFFERENT server orphans a plain
// server-plane member (join dropped, cluster mode disabled), a same-server
// re-pair keeps the join, and a peer-plane member keeps its own
// credentials. See cmd/outpost's TestMergePairingServerChange* twins.

// fakeExchange serves POST /api/register/exchange answering with a pairing
// against serverAddr. The exchange URL itself is the httptest listener; the
// server_addr in the response is what the outpost records as its pairing
// server, which is what the policy compares.
func fakeExchange(t *testing.T, serverAddr string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/register/exchange" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent_name":   "worker",
			"server_addr":  serverAddr,
			"server_port":  443,
			"protocol":     "wss",
			"token":        "matrix-token",
			"remote_port":  17006,
			"access_token": "access-token",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func seedJoinedTo(t *testing.T, s *Server, server string, mutate func(*conf.ClusterConfig)) {
	t.Helper()
	on := true
	fc := &conf.FileConfig{
		AgentName:  "worker",
		ServerAddr: server,
		ServerPort: 443,
		Protocol:   "wss",
		Cluster: &conf.ClusterConfig{
			Enabled:   &on,
			Runtimes:  conf.ClusterRuntimes{Agent: true},
			NodeName:  "worker-node",
			APIURL:    "https://" + server + "/api/cluster/agent",
			Token:     "old-bearer",
			CA:        []byte("-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----\n"),
			NodeToken: "K10old::node:old",
		},
	}
	if mutate != nil {
		mutate(fc.Cluster)
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		t.Fatal(err)
	}
}

func pairAgainst(t *testing.T, s *Server, srv *httptest.Server) *conf.FileConfig {
	t.Helper()
	if _, err := s.Pair(context.Background(), PairParams{Server: srv.URL, Code: "code", Name: "worker"}); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil || fc == nil || fc.Cluster == nil {
		t.Fatalf("reload: fc=%v err=%v", fc, err)
	}
	return fc
}

// S498.4: like its cmd/outpost twin, a re-pair onto a different server
// lands a plain server-plane member as an orphan — join dropped AND cluster
// mode disabled, so no boot silently joins the new server's cluster.
func TestPairServerChangeDropsCachedClusterJoin(t *testing.T) {
	s := newTestServer(t)
	seedJoinedTo(t, s, "old.example.com", nil)
	fc := pairAgainst(t, s, fakeExchange(t, "new.example.com"))
	cc := fc.Cluster
	if cc.APIURL != "" || cc.Token != "" || len(cc.CA) != 0 {
		t.Errorf("cached join survived a re-pair to a different server: api_url=%q token=%q ca=%d bytes",
			cc.APIURL, cc.Token, len(cc.CA))
	}
	if fc.ClusterOn() {
		t.Errorf("re-pair to a different server left cluster mode on (auto-join): %+v", cc)
	}
	if !cc.Runtimes.Agent || cc.NodeName != "worker-node" {
		t.Errorf("host-owned cluster fields clobbered: %+v", cc)
	}
	if fc.ServerAddr != "new.example.com" || fc.AccessToken != "access-token" {
		t.Errorf("pairing not applied: %+v", fc)
	}
}

func TestPairSameServerKeepsClusterJoin(t *testing.T) {
	s := newTestServer(t)
	seedJoinedTo(t, s, "same.example.com", nil)
	fc := pairAgainst(t, s, fakeExchange(t, "same.example.com"))
	cc := fc.Cluster
	if cc.APIURL != "https://same.example.com/api/cluster/agent" || cc.Token != "old-bearer" || len(cc.CA) == 0 {
		t.Errorf("same-server re-pair dropped the cached join: %+v", cc)
	}
}

func TestPairPeerPlaneKeepsCreds(t *testing.T) {
	s := newTestServer(t)
	seedJoinedTo(t, s, "old.example.com", func(cc *conf.ClusterConfig) {
		cc.JoinEndpoint = "10.0.0.5:7000"
		cc.JoinToken = "peer-tunnel-token"
		cc.NodeToken = "K10peer::node:peer"
		cc.STCPSecret = "peer-secret"
	})
	fc := pairAgainst(t, s, fakeExchange(t, "new.example.com"))
	cc := fc.Cluster
	if cc.JoinEndpoint != "10.0.0.5:7000" || cc.JoinToken != "peer-tunnel-token" {
		t.Errorf("peer join dropped: %+v", cc)
	}
	if cc.NodeToken != "K10peer::node:peer" || cc.STCPSecret != "peer-secret" {
		t.Errorf("peer credentials clobbered: %+v", cc)
	}
	if cc.APIURL != "https://old.example.com/api/cluster/agent" || cc.Token != "old-bearer" || len(cc.CA) == 0 {
		t.Errorf("peer-plane member lost its join triple on a server change: %+v", cc)
	}
}
