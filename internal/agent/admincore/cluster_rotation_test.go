package admincore

import (
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Rotation state, redaction, and the worker recovery flow — the P1 contract:
// a control-plane token rotate must hand back an explicit, secret-safe way
// for an already-joined worker to recover, never just a warning.

// A rotate must carry the recovery hint, and the hint text itself must never
// embed a literal secret — it is safe to display next to the freshly revealed
// token (same result struct) without doubling the leak surface.
func TestRotateControlPlaneToken_ReturnsWorkerRejoinHint(t *testing.T) {
	s := newTestServer(t)
	on := true
	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RotateControlPlaneToken()
	if err != nil {
		t.Fatalf("RotateControlPlaneToken: %v", err)
	}
	if got.WorkerRejoinHint == "" {
		t.Fatal("rotate did not return a worker recovery hint")
	}
	if !strings.Contains(got.WorkerRejoinHint, "outpost cluster join") {
		t.Errorf("hint does not name the recovery command: %q", got.WorkerRejoinHint)
	}
	if !strings.Contains(got.WorkerRejoinHint, "--token-stdin") {
		t.Errorf("hint does not steer the operator to the stdin route: %q", got.WorkerRejoinHint)
	}
	if strings.Contains(got.WorkerRejoinHint, got.TunnelToken) {
		t.Errorf("hint embeds the literal token value: %q", got.WorkerRejoinHint)
	}
	if strings.Contains(got.WorkerRejoinHint, got.STCPSecret) && got.STCPSecret != "" {
		t.Errorf("hint embeds the literal stcp secret value: %q", got.WorkerRejoinHint)
	}
}

// The hint is scoped to the rotate operation. A plain status read or an
// enable-mint must not carry it — surfacing it everywhere would blur "this
// just changed and needs a worker fix" into routine status noise.
func TestControlPlaneResult_WorkerRejoinHintOnlyFromRotate(t *testing.T) {
	s := newTestServer(t)
	on := true

	enabled, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if enabled.WorkerRejoinHint != "" {
		t.Errorf("SetControlPlane populated a rejoin hint: %q", enabled.WorkerRejoinHint)
	}

	viewed, err := s.ControlPlaneView(true)
	if err != nil {
		t.Fatal(err)
	}
	if viewed.WorkerRejoinHint != "" {
		t.Errorf("ControlPlaneView populated a rejoin hint: %q", viewed.WorkerRejoinHint)
	}
}

// End-to-end contract: rotating the token on the hosting side must not
// require a worker to re-supply the STCP secret or node token it already
// has — that is what makes recovery "explicit and cheap" instead of a full
// three-credential rejoin. Modeled with two independent admincore.Servers
// standing in for the two machines.
func TestControlPlaneTokenRotation_WorkerRecoversWithTokenOnly(t *testing.T) {
	host := newTestServer(t)
	worker := newTestServer(t)

	on := true
	if _, err := host.SetControlPlane(ControlPlaneParams{Enabled: &on}); err != nil {
		t.Fatalf("host SetControlPlane: %v", err)
	}
	initial, err := host.ControlPlaneView(true)
	if err != nil {
		t.Fatal(err)
	}

	// The worker's original join — all three credentials, as a real peer join
	// would supply them.
	if _, err := worker.JoinPeerPlane(PeerPlaneParams{
		Endpoint:   strp("10.0.0.5:7000"),
		Token:      strp(initial.TunnelToken),
		STCPSecret: strp("stcp-secret-original"),
		NodeToken:  strp("k3s-node-token-original"),
	}); err != nil {
		t.Fatalf("worker initial join: %v", err)
	}

	// Leaked token: the operator rotates it on the host.
	rotated, err := host.RotateControlPlaneToken()
	if err != nil {
		t.Fatalf("RotateControlPlaneToken: %v", err)
	}
	if rotated.TunnelToken == initial.TunnelToken {
		t.Fatal("rotate produced the same token")
	}

	// Recovery: the worker re-supplies ONLY the token, exactly as the hint
	// describes (`outpost cluster join --token-stdin`, no endpoint given).
	recovered, err := worker.JoinPeerPlane(PeerPlaneParams{Token: strp(rotated.TunnelToken)})
	if err != nil {
		t.Fatalf("worker recovery join: %v", err)
	}
	if !recovered.Joined || !recovered.HasSTCPSecret || !recovered.HasNodeToken {
		t.Errorf("worker lost credentials across recovery: %+v", recovered)
	}
	if recovered.Endpoint != "10.0.0.5:7000" {
		t.Errorf("worker endpoint changed during token-only recovery: %q", recovered.Endpoint)
	}

	workerCfg, err := conf.LoadFile(worker.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if workerCfg.Cluster.JoinToken != rotated.TunnelToken {
		t.Errorf("worker did not persist the rotated token: got %q", workerCfg.Cluster.JoinToken)
	}
	if workerCfg.Cluster.STCPSecret != "stcp-secret-original" {
		t.Error("worker's STCP secret changed on a token-only recovery")
	}
	if workerCfg.Cluster.NodeToken != "k3s-node-token-original" {
		t.Error("worker's node token changed on a token-only recovery")
	}
}

// The rotated result is exactly what RotateControlPlaneToken must never leak
// through a status read afterward — pinning the same redaction discipline
// TestPeerPlaneRedaction pins for the join side.
func TestRotateControlPlaneToken_NotLeakedByStatusRead(t *testing.T) {
	s := newTestServer(t)
	on := true
	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	rotated, err := s.RotateControlPlaneToken()
	if err != nil {
		t.Fatal(err)
	}
	if rotated.TunnelToken == "" {
		t.Fatal("rotate returned no token to pin redaction against")
	}

	unrevealed, err := s.ControlPlaneView(false)
	if err != nil {
		t.Fatal(err)
	}
	if unrevealed.TunnelToken != "" {
		t.Errorf("unrevealed status read leaked the token: %q", unrevealed.TunnelToken)
	}
	if !unrevealed.HasToken {
		t.Error("unrevealed status read should still report presence")
	}

	view, err := s.SafeView()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustJSON(t, view), rotated.TunnelToken) {
		t.Error("SafeView leaked the rotated tunnel token")
	}
}
