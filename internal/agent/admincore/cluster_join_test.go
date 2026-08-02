package admincore

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

// A fresh host joins the cloudbox-hosted plane. Reporting anything else would
// make the default look like a configuration someone chose.
func TestPeerPlaneView_DefaultIsCloudbox(t *testing.T) {
	s := newTestServer(t)
	got, err := s.PeerPlaneView()
	if err != nil {
		t.Fatalf("PeerPlaneView: %v", err)
	}
	if got.Joined || got.Endpoint != "" {
		t.Errorf("fresh host reports a peer plane: %+v", got)
	}
	if got.HasToken || got.HasSTCPSecret || got.HasNodeToken {
		t.Errorf("fresh host reports credentials: %+v", got)
	}
}

// Endpoint parsing is where an operator's copy-paste lands, so each accepted
// and rejected shape is pinned.
func TestJoinPeerPlane_EndpointValidation(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string // "" means the call must fail
	}{
		{"host and port", "10.0.0.5:7000", "10.0.0.5:7000"},
		{"bare host takes the default port", "10.0.0.5", "10.0.0.5:7000"},
		{"bare name takes the default port", "peer.local", "peer.local:7000"},
		{"surrounding space is trimmed", "  10.0.0.5:7100  ", "10.0.0.5:7100"},
		{"ipv6 literal keeps its brackets", "[fd00::1]:7000", "[fd00::1]:7000"},
		{"bare ipv6 literal takes the default port", "fd00::1", "[fd00::1]:7000"},
		{"a URL is rejected, not coerced", "https://10.0.0.5:7000", ""},
		{"a path is rejected", "10.0.0.5:7000/join", ""},
		{"empty is rejected", "   ", ""},
		{"port 0 is rejected", "10.0.0.5:0", ""},
		{"port above range is rejected", "10.0.0.5:70000", ""},
		{"non-numeric port is rejected", "10.0.0.5:frps", ""},
		{"missing host is rejected", ":7000", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			got, err := s.JoinPeerPlane(PeerPlaneParams{
				Endpoint: strp(tt.endpoint),
				Token:    strp("tunnel-token"),
			})
			if tt.want == "" {
				if err == nil {
					t.Fatalf("accepted %q -> %+v", tt.endpoint, got)
				}
				if ae := AsAPIError(err); ae == nil || ae.Status != 400 {
					t.Errorf("want 400 APIError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected %q: %v", tt.endpoint, err)
			}
			if got.Endpoint != tt.want {
				t.Errorf("endpoint = %q, want %q", got.Endpoint, tt.want)
			}
		})
	}
}

// A join without the token would persist an endpoint and produce a node that
// silently fails its frpc login, so it is refused at the surface.
func TestJoinPeerPlane_RequiresBothEndpointAndToken(t *testing.T) {
	tests := []struct {
		name   string
		params PeerPlaneParams
	}{
		{"token without endpoint", PeerPlaneParams{Token: strp("t")}},
		{"endpoint without token", PeerPlaneParams{Endpoint: strp("10.0.0.5:7000")}},
		{"neither", PeerPlaneParams{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			if _, err := s.JoinPeerPlane(tt.params); err == nil {
				t.Fatal("accepted an incomplete join")
			}
		})
	}
}

func TestJoinPeerPlane_APIPortRange(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		s := newTestServer(t)
		_, err := s.JoinPeerPlane(PeerPlaneParams{
			Endpoint: strp("10.0.0.5:7000"), Token: strp("t"), APIPort: intp(port),
		})
		if err == nil {
			t.Errorf("accepted api_port %d", port)
		}
	}
}

// The whole point of the surface: after one call the config is complete AND
// the node is actually a node — cluster mode on with a runtime selected.
func TestJoinPeerPlane_PersistsAndEnables(t *testing.T) {
	s := newTestServer(t)
	got, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint:   strp("10.0.0.5"),
		Token:      strp("tunnel-token"),
		STCPSecret: strp("stcp-secret"),
		NodeToken:  strp("K10abc::node:xyz"),
		APIPort:    intp(6443),
	})
	if err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	if !got.Joined || !got.ClusterEnabled {
		t.Errorf("join did not take effect: %+v", got)
	}
	if !got.HasToken || !got.HasSTCPSecret || !got.HasNodeToken {
		t.Errorf("credentials not persisted: %+v", got)
	}

	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Cluster.JoinEndpoint != "10.0.0.5:7000" || fc.Cluster.JoinToken != "tunnel-token" {
		t.Errorf("persisted join = %q / %q", fc.Cluster.JoinEndpoint, fc.Cluster.JoinToken)
	}
	if fc.Cluster.STCPSecret != "stcp-secret" || fc.Cluster.NodeToken != "K10abc::node:xyz" {
		t.Errorf("persisted credentials wrong: %+v", fc.Cluster)
	}
	if !fc.ClusterOn() || !fc.Cluster.Runtimes.Agent {
		t.Errorf("cluster not enabled with a runtime: %+v", fc.Cluster)
	}
	if !fc.Cluster.JoinsPeerPlane() {
		t.Error("JoinsPeerPlane false after a join")
	}
}

// B6 at the switch itself: joining a peer plane drops the cloud plane's
// overlay trio. The runtime joins an overlay purely on overlay_login_server
// being non-empty, so a leftover trio would attach this node to the CLOUD
// overlay while its k3s agent joins the peer plane. The boot reattach also
// clears them, but that needs cloudbox reachable — the join must not depend
// on the next boot being online.
func TestJoinPeerPlane_DropsCloudOverlayCreds(t *testing.T) {
	s := newTestServer(t)
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			OverlayLoginServer: "https://ai.example.io/overlay",
			OverlayAuthKey:     "ts-authkey-cloud",
			OverlayPodCIDR:     "10.42.7.0/24",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint: strp("10.0.0.5:7000"), Token: strp("t"),
	}); err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cc := fc.Cluster
	if cc.OverlayLoginServer != "" || cc.OverlayAuthKey != "" || cc.OverlayPodCIDR != "" {
		t.Errorf("cloud overlay trio survived the join: %+v", cc)
	}
}

// Joining a peer plane overwrites STCPSecret with the PEER's credential —
// and when the outgoing value was CLOUDBOX's (a plain cloudbox-plane member
// until this call), that value must be preserved in cloud_stcp_secret
// first: it authorizes the overlay-control relay the peer-flannel runtime
// needs to register on cloudbox's tailnet, and this join may be the last
// moment it exists locally (the boot reattach also captures it, but that
// needs cloudbox reachable).
func TestJoinPeerPlane_PreservesCloudboxSTCPSecretForTheRelay(t *testing.T) {
	s := newTestServer(t)
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			STCPSecret: "cloudbox-secret", // pre-join: the cloudbox plane's
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint:   strp("10.0.0.5:7000"),
		Token:      strp("t"),
		STCPSecret: strp("peer-secret"),
	}); err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Cluster.STCPSecret != "peer-secret" {
		t.Errorf("stcp_secret = %q, want the peer's", fc.Cluster.STCPSecret)
	}
	if fc.Cluster.CloudSTCPSecret != "cloudbox-secret" {
		t.Errorf("cloud_stcp_secret = %q, want cloudbox's preserved", fc.Cluster.CloudSTCPSecret)
	}

	// A REJOIN (already on a peer plane) must not capture the PEER's secret
	// as if it were cloudbox's — the pre-join value is no longer cloudbox's.
	if _, err := s.JoinPeerPlane(PeerPlaneParams{STCPSecret: strp("rotated-peer-secret")}); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	fc, _ = conf.LoadFile(s.deps.ConfigPath)
	if fc.Cluster.CloudSTCPSecret != "cloudbox-secret" {
		t.Errorf("rejoin overwrote cloud_stcp_secret with %q — the peer's secret is not cloudbox's",
			fc.Cluster.CloudSTCPSecret)
	}
}

// An operator who selected virtual runtimes must not have a k3s agent started
// for them as a side effect of naming a control plane.
func TestJoinPeerPlane_KeepsAnExistingRuntimeSelection(t *testing.T) {
	s := newTestServer(t)
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			Runtimes: conf.ClusterRuntimes{Virtual: []string{conf.ClusterRuntimeVKPodman}},
			// A virtual selection needs the vk credential set; seed the fields a
			// prior bundle-carrying join would have persisted, so this join is
			// purely a selection-preservation exercise.
			Token:             "sa-token",
			CA:                []byte("peer-ca"),
			AllowedNamespaces: []string{"default"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint: strp("10.0.0.5:7000"), Token: strp("t"),
	}); err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if fc.Cluster.Runtimes.Agent {
		t.Error("join turned on the agent runtime over an explicit virtual selection")
	}
}

// A partial update must not clear the credentials it does not mention —
// rotating the tunnel token is a one-field operation.
func TestJoinPeerPlane_PartialUpdateKeepsOtherCredentials(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint:   strp("10.0.0.5:7000"),
		Token:      strp("old"),
		STCPSecret: strp("stcp"),
		NodeToken:  strp("node"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.JoinPeerPlane(PeerPlaneParams{Token: strp("new")})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !got.HasSTCPSecret || !got.HasNodeToken || got.Endpoint != "10.0.0.5:7000" {
		t.Errorf("partial update dropped state: %+v", got)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if fc.Cluster.JoinToken != "new" {
		t.Errorf("token = %q, want new", fc.Cluster.JoinToken)
	}
}

// Leaving must clear the PEER-issued credentials too. Half-clearing is what
// makes the next boot present a foreign node token to cloudbox's apiserver.
func TestLeavePeerPlane_ClearsPeerCredentialsOnly(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint:   strp("10.0.0.5:7000"),
		Token:      strp("t"),
		STCPSecret: strp("s"),
		NodeToken:  strp("n"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LeavePeerPlane()
	if err != nil {
		t.Fatalf("LeavePeerPlane: %v", err)
	}
	if got.Joined || got.HasToken || got.HasSTCPSecret || got.HasNodeToken {
		t.Errorf("leave did not clear: %+v", got)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if fc.Cluster.JoinEndpoint != "" || fc.Cluster.JoinToken != "" ||
		fc.Cluster.STCPSecret != "" || fc.Cluster.NodeToken != "" {
		t.Errorf("config not cleared: %+v", fc.Cluster)
	}
	// Cluster mode stays ON: "stop joining that plane" is not "stop being a
	// node" — the cloudbox-hosted plane is the default it falls back to.
	if !fc.ClusterOn() {
		t.Error("leaving a peer plane disabled cluster mode")
	}
	// Idempotent, and a no-op leave doesn't ask for a restart.
	again, err := s.LeavePeerPlane()
	if err != nil {
		t.Fatalf("second LeavePeerPlane: %v", err)
	}
	if again.RestartPending {
		t.Error("a no-op leave requested a restart")
	}
}

// REDACTION. The join token must never appear in a status read — same
// contract cluster.token and mcp_bearer_token already hold.
func TestPeerPlaneRedaction(t *testing.T) {
	const (
		joinToken = "JOIN-TOKEN-a1b2c3d4e5f6"
		stcp      = "STCP-SECRET-9z8y7x"
		nodeTok   = "K10NODETOKEN::node:secretvalue"
	)
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(cfgPath, &conf.FileConfig{AgentName: "host1"}); err != nil {
		t.Fatal(err)
	}
	core, err := New(Deps{ConfigPath: cfgPath, Apps: agent.NewAppRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.JoinPeerPlane(PeerPlaneParams{
		Endpoint:   strp("10.0.0.5:7000"),
		Token:      strp(joinToken),
		STCPSecret: strp(stcp),
		NodeToken:  strp(nodeTok),
	}); err != nil {
		t.Fatal(err)
	}

	secrets := []string{joinToken, stcp, nodeTok}

	// 1. The mutation's own result.
	joined, err := core.PeerPlaneView()
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "PeerPlaneView", mustJSON(t, joined), secrets)

	// 2. SafeView — what GET /api/config and the MCP config resource render.
	view, err := core.SafeView()
	if err != nil {
		t.Fatalf("SafeView: %v", err)
	}
	body := mustJSON(t, view)
	assertNoSecrets(t, "SafeView", body, secrets)
	if !strings.Contains(body, `"has_join_token":true`) {
		t.Errorf("SafeView does not report join-token presence: %s", body)
	}
	if !strings.Contains(body, `"join_endpoint":"10.0.0.5:7000"`) {
		t.Errorf("SafeView does not report the join endpoint: %s", body)
	}

	// 3. Status — the other read surface (`outpost status`).
	st, err := core.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	assertNoSecrets(t, "Status", mustJSON(t, st), secrets)
}

func assertNoSecrets(t *testing.T, surface, body string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(body, s) {
			t.Errorf("%s leaked a credential (%q) in: %s", surface, s, body)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
