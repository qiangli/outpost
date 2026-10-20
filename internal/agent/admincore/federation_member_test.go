package admincore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/federation"
)

const (
	testBootstrap = "abcdef.0123456789abcdef"
	testSTCP      = "member-stcp-secret"
	testTunnel    = "member-tunnel-token"
)

func joinParams() FederationJoinParams {
	return FederationJoinParams{
		Endpoint: "192.0.2.10:7000", Token: testTunnel, STCPSecret: testSTCP,
		BootstrapToken: testBootstrap, CAHash: "sha256:" + strings.Repeat("cd", 32), ClusterName: "member-b",
	}
}

func memberServer(t *testing.T) (*Server, string) {
	t.Helper()
	skipWindows(t)
	s := newTestServer(t)
	enableControlPlane(t, s)
	pending := filepath.Join(t.TempDir(), "outpost", "federation-join.json")
	s.deps.FederationPendingPath = pending
	return s, pending
}

// Everything but the bootstrap token lands in agent.json; the token lands only
// in the 0600 pending-join file. Status never echoes a credential.
func TestFederationJoinPersistsAllButBootstrapToken(t *testing.T) {
	s, pending := memberServer(t)
	res, err := s.FederationJoin(joinParams())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Enabled || res.Role != "member" || res.Endpoint != "192.0.2.10:7000" || res.ClusterName != "member-b" || !res.JoinPending {
		t.Fatalf("%+v", res)
	}
	if res.FederationID != "fed-"+strings.Repeat("cd", 6) {
		t.Fatalf("federation id = %q", res.FederationID)
	}

	cfg, err := os.ReadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), testBootstrap) {
		t.Fatal("bootstrap token written to agent.json")
	}
	for _, want := range []string{testSTCP, testTunnel, "192.0.2.10:7000"} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("agent.json missing %q", want)
		}
	}
	info, err := os.Stat(pending)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pending join: %v %v, want mode 0600", info, err)
	}
	pj, ok, err := federation.ReadPendingJoin(pending)
	if err != nil || !ok || pj.BootstrapToken != testBootstrap {
		t.Fatalf("pending join = %+v %v %v", pj, ok, err)
	}

	st, _ := s.FederationStatus()
	b, _ := json.Marshal(st)
	for _, secret := range []string{testBootstrap, testSTCP, testTunnel} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("status leaks %q: %s", secret, b)
		}
	}
}

func TestFederationJoinValidates(t *testing.T) {
	s, pending := memberServer(t)
	for name, mut := range map[string]func(*FederationJoinParams){
		"no endpoint":       func(p *FederationJoinParams) { p.Endpoint = "" },
		"wildcard endpoint": func(p *FederationJoinParams) { p.Endpoint = "0.0.0.0:7000" },
		"no stcp secret":    func(p *FederationJoinParams) { p.STCPSecret = "" },
		"malformed token":   func(p *FederationJoinParams) { p.BootstrapToken = "nope" },
		"malformed ca hash": func(p *FederationJoinParams) { p.CAHash = "sha256:xyz" },
	} {
		p := joinParams()
		mut(&p)
		if _, err := s.FederationJoin(p); AsAPIError(err) == nil || AsAPIError(err).Status != 400 {
			t.Errorf("%s: err = %v, want 400", name, err)
		}
	}
	if _, ok, _ := federation.ReadPendingJoin(pending); ok {
		t.Fatal("a rejected join left a pending token behind")
	}
	p := joinParams()
	p.Endpoint = "192.0.2.10"
	res, err := s.FederationJoin(p)
	if err != nil || res.Endpoint != "192.0.2.10:7000" {
		t.Fatalf("port default: %+v %v", res, err)
	}
}

func TestFederationJoinRequiresControlPlaneAndNotHost(t *testing.T) {
	skipWindows(t)
	s := newTestServer(t)
	s.deps.FederationPendingPath = filepath.Join(t.TempDir(), "j.json")
	if _, err := s.FederationJoin(joinParams()); AsAPIError(err) == nil || AsAPIError(err).Status != 400 {
		t.Fatalf("join without control plane: %v, want 400", err)
	}
	enableControlPlane(t, s)
	if _, err := s.FederationOn(FederationParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FederationJoin(joinParams()); AsAPIError(err) == nil || !strings.Contains(err.Error(), "federation host") {
		t.Fatalf("join on a host: %v", err)
	}
}

func TestFederationJoinRefusesOnWindows(t *testing.T) {
	s, _ := memberServer(t)
	defer func(g string) { federationGOOS = g }(federationGOOS)
	federationGOOS = "windows"
	_, err := s.FederationJoin(joinParams())
	if ae := AsAPIError(err); ae == nil || ae.Status != 400 || !strings.Contains(err.Error(), "not supported on windows") {
		t.Fatalf("err = %v, want the windows refusal", err)
	}
}

func TestFederationLeaveUnregistersAndClears(t *testing.T) {
	s, pending := memberServer(t)
	if _, err := s.FederationJoin(joinParams()); err != nil {
		t.Fatal(err)
	}
	// Registration completed: the reconciler deleted the pending token.
	if err := federation.RemovePendingJoin(pending); err != nil {
		t.Fatal(err)
	}
	var gotName string
	var gotKarmadaConfig string
	var gotPending bool
	fail := errors.New("unregister: dial 127.0.0.1:15443 refused")
	s.deps.FederationLeave = func(_ context.Context, name, _, karmadaConfig string, pending bool) error {
		gotName, gotKarmadaConfig, gotPending = name, karmadaConfig, pending
		return fail
	}
	if _, err := s.FederationLeave(context.Background(), FederationParams{}); err == nil || !strings.Contains(err.Error(), "karmada-config") {
		t.Fatalf("registered member left without host admin kubeconfig: %v", err)
	}
	if gotName != "" {
		t.Fatal("unregister ran before the host admin kubeconfig was supplied")
	}
	if _, err := s.FederationLeave(context.Background(), FederationParams{KarmadaConfig: "/host-karmada.yaml"}); err == nil {
		t.Fatal("leave swallowed an unregister failure")
	}
	if st, _ := s.FederationStatus(); !st.Enabled {
		t.Fatal("a failed leave cleared the config")
	}
	if gotName != "member-b" || gotKarmadaConfig != "/host-karmada.yaml" || gotPending {
		t.Fatalf("leave called with name=%q karmadaConfig=%q pending=%v", gotName, gotKarmadaConfig, gotPending)
	}

	s.deps.FederationLeave = func(context.Context, string, string, string, bool) error { return nil }
	// `off` on a member is leave.
	res, err := s.FederationOff(context.Background(), FederationParams{KarmadaConfig: "/host-karmada.yaml"})
	if err != nil || res.Enabled || res.Warning != "" {
		t.Fatalf("leave: %+v %v", res, err)
	}
	if st, _ := s.FederationStatus(); st.Enabled {
		t.Fatal("still a member after leave")
	}
}

func TestFederationLeaveForceAndPending(t *testing.T) {
	s, pending := memberServer(t)
	if _, err := s.FederationJoin(joinParams()); err != nil {
		t.Fatal(err)
	}
	var sawPending bool
	s.deps.FederationLeave = func(_ context.Context, _, _, _ string, pending bool) error {
		sawPending = pending
		return errors.New("plane unreachable")
	}
	res, err := s.FederationLeave(context.Background(), FederationParams{Force: true})
	if err != nil || res.Enabled || !strings.Contains(res.Warning, "plane unreachable") {
		t.Fatalf("forced leave: %+v %v", res, err)
	}
	if !sawPending {
		t.Fatal("leave was not told the join never registered")
	}
	if _, ok, _ := federation.ReadPendingJoin(pending); ok {
		t.Fatal("leave left the bootstrap token behind")
	}
	if _, err := s.FederationLeave(context.Background(), FederationParams{}); AsAPIError(err) == nil {
		t.Fatal("leave on a non-member must be refused")
	}
}

func TestFederationLeaveForceWithoutHostConfigWarns(t *testing.T) {
	s, pending := memberServer(t)
	if _, err := s.FederationJoin(joinParams()); err != nil {
		t.Fatal(err)
	}
	if err := federation.RemovePendingJoin(pending); err != nil {
		t.Fatal(err)
	}
	s.deps.FederationLeave = func(context.Context, string, string, string, bool) error { return nil }
	res, err := s.FederationLeave(context.Background(), FederationParams{Force: true})
	if err != nil || !strings.Contains(res.Warning, "RBAC may remain") {
		t.Fatalf("force without control-plane cleanup warning: %+v %v", res, err)
	}
}
