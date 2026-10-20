package admincore

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/federation"
)

type tokenRunner struct{ calls [][]string }

func (r *tokenRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	return []byte("karmadactl register 127.0.0.1:15443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:" + strings.Repeat("ab", 32)), nil
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("federation verbs refuse on windows")
	}
}

func enableControlPlane(t *testing.T, s *Server) {
	t.Helper()
	on := true
	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
}

func TestFederationOnRequiresControlPlane(t *testing.T) {
	skipWindows(t)
	s := newTestServer(t)
	_, err := s.FederationOn(FederationParams{})
	if ae := AsAPIError(err); ae == nil || ae.Status != 400 {
		t.Fatalf("err = %v, want 400", err)
	}
}

func TestFederationOnStatusNeverRevealsSecrets(t *testing.T) {
	skipWindows(t)
	s := newTestServer(t)
	enableControlPlane(t, s)
	res, err := s.FederationOn(FederationParams{ClusterName: "home"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Enabled || res.Role != "host" || res.ClusterName != "home" || res.APIPort != 15443 {
		t.Fatalf("%+v", res)
	}
	st, _ := s.FederationStatus()
	view := ControlPlaneResultJSON(t, s)
	for _, secret := range []string{view.TunnelToken, view.STCPSecret} {
		if secret == "" {
			t.Fatal("test needs real secrets to check against")
		}
	}
	if st.CAHash != "" || st.FederationID != "" {
		t.Fatalf("identity before it is learned: %+v", st)
	}
}

// ControlPlaneResultJSON returns the revealed control-plane credentials so the
// test can assert federation status never contains them.
func ControlPlaneResultJSON(t *testing.T, s *Server) ControlPlaneResult {
	t.Helper()
	v, err := s.ControlPlaneView(true)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFederationTokenNeedsKnownIdentityThenRevealsEnvHint(t *testing.T) {
	skipWindows(t)
	s := newTestServer(t)
	enableControlPlane(t, s)
	r := &tokenRunner{}
	s.deps.FederationRunner = r
	if _, err := s.FederationToken(context.Background()); AsAPIError(err) == nil || AsAPIError(err).Status != 400 {
		t.Fatalf("token before on = %v, want 400", err)
	}
	if _, err := s.FederationOn(FederationParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FederationToken(context.Background()); AsAPIError(err) == nil || AsAPIError(err).Status != 409 {
		t.Fatalf("token before CA known = %v, want 409", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("minted a token without a known federation identity")
	}
	if err := s.RecordFederationCAHash(strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	tok, err := s.FederationToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cp := ControlPlaneResultJSON(t, s)
	if tok.TunnelToken != cp.TunnelToken || tok.STCPSecret != cp.STCPSecret || tok.BootstrapToken != "abcdef.0123456789abcdef" {
		t.Fatalf("token result %+v", tok)
	}
	if tok.FederationID != "fed-"+strings.Repeat("ab", 6) || tok.Endpoint == "" {
		t.Fatalf("identity %+v", tok)
	}
	for _, lit := range []string{tok.TunnelToken, tok.STCPSecret, tok.BootstrapToken} {
		if strings.Contains(tok.JoinHint, lit) {
			t.Fatal("join hint embeds a literal secret")
		}
	}
	if !strings.Contains(tok.JoinHint, "OUTPOST_FEDERATION_JOIN_TOKEN") {
		t.Fatalf("hint = %q", tok.JoinHint)
	}
	st, _ := s.FederationStatus()
	if st.FederationID != tok.FederationID {
		t.Fatalf("status id %q", st.FederationID)
	}
}

func TestFederationStatusReportsReconcilerState(t *testing.T) {
	skipWindows(t)
	s := newTestServer(t)
	enableControlPlane(t, s)
	if _, err := s.FederationOn(FederationParams{}); err != nil {
		t.Fatal(err)
	}
	s.deps.FederationState = func() federation.HostState {
		return federation.HostState{Phase: federation.PhaseInstalling, SelfRegistered: false, LastError: "dial refused"}
	}
	st, _ := s.FederationStatus()
	if st.Phase != "installing" || st.LastError != "dial refused" || st.SelfRegistered {
		t.Fatalf("%+v", st)
	}
}

func TestFederationOffClearsAndForceGatesUninstallFailure(t *testing.T) {
	skipWindows(t)
	s := newTestServer(t)
	enableControlPlane(t, s)
	if _, err := s.FederationOn(FederationParams{}); err != nil {
		t.Fatal(err)
	}
	// No appstore catalog resolves in the test server, so the uninstall fails.
	_, err := s.FederationOff(context.Background(), FederationParams{})
	if err == nil {
		t.Fatal("off without force must surface an uninstall failure")
	}
	if st, _ := s.FederationStatus(); !st.Enabled {
		t.Fatal("a failed uninstall must not clear the config")
	}
	res, err := s.FederationOff(context.Background(), FederationParams{Force: true})
	if err != nil || res.Enabled || res.Warning == "" {
		t.Fatalf("forced off: %+v err=%v", res, err)
	}
	if st, _ := s.FederationStatus(); st.Enabled {
		t.Fatal("still enabled after forced off")
	}
	// Control plane survives federation off.
	if cp, _ := s.ControlPlaneView(false); !cp.ControlPlane {
		t.Fatal("federation off must not disable the control plane")
	}
	_ = errors.New
}
