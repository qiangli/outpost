package admincore

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/vkcred"
)

// --- worker side: the join provisions the vk credential fail-closed ---------

// THE END-TO-END CONTRACT OF A FRESH JOIN: passing the hosting machine's vk
// bundle persists everything the peer vk path in startClusterRunner reads —
// peer CA, apiserver bearer token, fail-closed namespace policy — with no
// agent.json hand edit anywhere.
func TestJoinPeerPlane_VKBundleProvisionsCredential(t *testing.T) {
	s := newTestServer(t)
	bundle, err := vkcred.Bundle{
		CA:         []byte("the-peer-ca"),
		Token:      "the-sa-token",
		Namespaces: []string{"default", "workloads"},
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint: strp("10.0.0.5:7000"),
		Token:    strp("tunnel-token"),
		VKBundle: &bundle,
		Virtual:  []string{"vk-podman"},
	})
	if err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	if !res.HasVKCredential || res.VKCredentialKind != "token" {
		t.Errorf("result vk credential = %v/%q, want token", res.HasVKCredential, res.VKCredentialKind)
	}
	if !reflect.DeepEqual(res.AllowedNamespaces, []string{"default", "workloads"}) {
		t.Errorf("result namespaces = %v", res.AllowedNamespaces)
	}

	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cc := fc.Cluster
	if string(cc.CA) != "the-peer-ca" || cc.Token != "the-sa-token" {
		t.Errorf("persisted credential = ca %q token %q", cc.CA, cc.Token)
	}
	if !reflect.DeepEqual(cc.AllowedNamespaces, []string{"default", "workloads"}) {
		t.Errorf("persisted namespaces = %v", cc.AllowedNamespaces)
	}
}

// The bundle is authoritative: applying it clears a stale client-cert pair,
// which would otherwise WIN over the fresh token (vknode's precedence) and
// keep authenticating as the old identity.
func TestJoinPeerPlane_VKBundleReplacesClientCert(t *testing.T) {
	s := newTestServer(t)
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			ClientCert: []byte("old-cert"),
			ClientKey:  []byte("old-key"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	bundle, _ := vkcred.Bundle{
		CA: []byte("ca"), Token: "tok", Namespaces: []string{"default"},
	}.Encode()
	if _, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint: strp("10.0.0.5:7000"), Token: strp("t"), VKBundle: &bundle,
	}); err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if len(fc.Cluster.ClientCert) != 0 || len(fc.Cluster.ClientKey) != 0 {
		t.Error("bundle apply left the stale client-cert pair in place")
	}
}

// FAIL-CLOSED PROVISIONING: a join whose RESULT selects virtual runtimes
// without a usable vk credential set is refused — at join time, where the
// operator still holds the hosting machine — instead of booting into an
// endless "no usable credentials" retry loop (or, with a credential but no
// policy, a node that denies every pod with no hint why).
func TestJoinPeerPlane_VirtualRequiresVKProvision(t *testing.T) {
	tests := []struct {
		name    string
		cluster *conf.ClusterConfig // pre-seeded state, nil for fresh
		wantErr string
	}{
		{
			name:    "no credential at all",
			wantErr: "vk apiserver credential",
		},
		{
			name:    "token without the peer CA cannot verify the self-signed apiserver",
			cluster: &conf.ClusterConfig{Token: "sa-token"},
			wantErr: "peer CA",
		},
		{
			name:    "credential without a namespace policy denies every pod",
			cluster: &conf.ClusterConfig{Token: "sa-token", CA: []byte("ca")},
			wantErr: "namespace policy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			if tt.cluster != nil {
				if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{Cluster: tt.cluster}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.JoinPeerPlane(PeerPlaneParams{
				Endpoint: strp("10.0.0.5:7000"),
				Token:    strp("tunnel-token"),
				Virtual:  []string{"vk-podman"},
			})
			if err == nil {
				t.Fatal("a virtual join without a vk provision was accepted")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not name the gap %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "vk-credential") {
				t.Errorf("error %q does not name the mint command", err)
			}
		})
	}
}

// A hand-provisioned k3s client-certificate pair satisfies the same gate —
// the bundle is the paved road, not the only road.
func TestJoinPeerPlane_ClientCertSatisfiesVKProvision(t *testing.T) {
	s := newTestServer(t)
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			ClientCert:        []byte("cert"),
			ClientKey:         []byte("key"),
			CA:                []byte("ca"),
			AllowedNamespaces: []string{"default"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint: strp("10.0.0.5:7000"), Token: strp("t"), Virtual: []string{"vk-native"},
	})
	if err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	if !res.HasVKCredential || res.VKCredentialKind != "client-cert" {
		t.Errorf("vk credential = %v/%q, want client-cert", res.HasVKCredential, res.VKCredentialKind)
	}
}

// A mispasted bundle (the k3s node token is the likeliest) fails loudly and
// persists nothing.
func TestJoinPeerPlane_BadVKBundleRejected(t *testing.T) {
	s := newTestServer(t)
	bad := "K10abc::node:secret"
	_, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint: strp("10.0.0.5:7000"), Token: strp("t"), VKBundle: &bad,
	})
	if err == nil {
		t.Fatal("a non-bundle vk_bundle value was accepted")
	}
	fc, lerr := conf.LoadFile(s.deps.ConfigPath)
	if lerr == nil && fc != nil && fc.Cluster != nil && fc.Cluster.JoinEndpoint != "" {
		t.Error("a refused join persisted state")
	}
}

// The view must report the policy and credential PRESENCE without ever
// carrying the token the bundle delivered.
func TestPeerPlaneView_VKCredentialRedacted(t *testing.T) {
	s := newTestServer(t)
	const saToken = "SA-TOKEN-3f9c2b1e8d"
	bundle, _ := vkcred.Bundle{
		CA: []byte("ca"), Token: saToken, Namespaces: []string{"workloads"},
	}.Encode()
	if _, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint: strp("10.0.0.5:7000"), Token: strp("t"), VKBundle: &bundle,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PeerPlaneView()
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasVKCredential || got.VKCredentialKind != "token" {
		t.Errorf("view vk credential = %v/%q", got.HasVKCredential, got.VKCredentialKind)
	}
	if !reflect.DeepEqual(got.AllowedNamespaces, []string{"workloads"}) {
		t.Errorf("view namespaces = %v", got.AllowedNamespaces)
	}
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), saToken) {
		t.Errorf("view leaked the vk token: %s", blob)
	}
}

// Leaving the peer plane clears the peer-only vk policy fields, as pinned by
// TestLeavePeerPlane_ClearsPeerCredentialsOnly — this covers the bundle-written
// namespace list specifically.
func TestLeavePeerPlane_ClearsBundleNamespacePolicy(t *testing.T) {
	s := newTestServer(t)
	bundle, _ := vkcred.Bundle{
		CA: []byte("ca"), Token: "tok", Namespaces: []string{"default"},
	}.Encode()
	if _, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint: strp("10.0.0.5:7000"), Token: strp("t"), VKBundle: &bundle,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LeavePeerPlane(); err != nil {
		t.Fatal(err)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if len(fc.Cluster.AllowedNamespaces) != 0 {
		t.Errorf("leave kept the peer namespace policy: %v", fc.Cluster.AllowedNamespaces)
	}
}

// --- hosting side: minting the credential ------------------------------------

func TestControlPlaneVKCredential_RefusesOnANonHostingHost(t *testing.T) {
	s := newTestServer(t)
	_, err := s.ControlPlaneVKCredential(context.Background(), nil)
	if err == nil {
		t.Fatal("minted a vk credential on a host with no control plane")
	}
	if !strings.Contains(err.Error(), "control plane") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
}

func TestControlPlaneVKCredential_NeedsTheHostedKubeconfig(t *testing.T) {
	s := newTestServer(t)
	on := true
	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ControlPlaneVKCredential(context.Background(), nil)
	if err == nil {
		t.Fatal("minted without the hosted plane's kubeconfig recorded")
	}
	if !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("error does not name the missing piece: %v", err)
	}
}

// hostingServer flips the control plane on and records a kubeconfig path, then
// stubs the mint seam so no apiserver is contacted.
func hostingServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	on := true
	if _, err := s.SetControlPlane(ControlPlaneParams{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	fc.Cluster.ControlPlaneKubeconfig = "/tmp/does-not-matter-stubbed.yaml"
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestControlPlaneVKCredential_MintsTheEncodedBundle(t *testing.T) {
	s := hostingServer(t)

	var gotOpts vkcred.MintOptions
	orig := mintVKCredential
	mintVKCredential = func(_ context.Context, opts vkcred.MintOptions) (vkcred.Bundle, error) {
		gotOpts = opts
		return vkcred.Bundle{CA: []byte("minted-ca"), Token: "minted-token", Namespaces: opts.Namespaces}, nil
	}
	defer func() { mintVKCredential = orig }()

	res, err := s.ControlPlaneVKCredential(context.Background(), []string{" workloads ", ""})
	if err != nil {
		t.Fatalf("ControlPlaneVKCredential: %v", err)
	}
	if gotOpts.KubeconfigPath == "" {
		t.Error("mint was not handed the hosted plane's kubeconfig path")
	}
	if !reflect.DeepEqual(gotOpts.Namespaces, []string{"workloads"}) {
		t.Errorf("mint namespaces = %v", gotOpts.Namespaces)
	}
	if !reflect.DeepEqual(res.Namespaces, []string{"workloads"}) {
		t.Errorf("result namespaces = %v", res.Namespaces)
	}
	if res.Endpoint == "" {
		t.Error("result has no endpoint — the caller cannot render the join line")
	}

	// The bundle must round-trip into exactly what the worker will persist.
	back, err := vkcred.Decode(res.Bundle)
	if err != nil {
		t.Fatalf("returned bundle does not decode: %v", err)
	}
	if string(back.CA) != "minted-ca" || back.Token != "minted-token" {
		t.Errorf("bundle round-trip = %+v", back)
	}
}

// Naming no namespace falls back to ["default"] rather than minting a policy
// that denies everything — the zero-flag path must yield a usable node.
func TestControlPlaneVKCredential_DefaultNamespace(t *testing.T) {
	s := hostingServer(t)
	orig := mintVKCredential
	mintVKCredential = func(_ context.Context, opts vkcred.MintOptions) (vkcred.Bundle, error) {
		return vkcred.Bundle{CA: []byte("ca"), Token: "tok", Namespaces: opts.Namespaces}, nil
	}
	defer func() { mintVKCredential = orig }()

	res, err := s.ControlPlaneVKCredential(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Namespaces, []string{DefaultVKNamespace}) {
		t.Errorf("default namespaces = %v, want [%s]", res.Namespaces, DefaultVKNamespace)
	}
}
