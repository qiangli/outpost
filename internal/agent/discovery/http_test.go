package discovery

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestSessionStore covers TTL expiry + max-size eviction so the
// HTTP probe path can rely on its bounded behavior.
func TestSessionStore(t *testing.T) {
	store := NewSessionStore()
	store.ttl = 100 * time.Millisecond
	store.maxSize = 3

	s1, err := store.New("SHA256:a")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, ok := store.Get(s1.ID); !ok {
		t.Fatal("session not retrievable")
	}
	// MarkVerified flips the flag.
	store.MarkVerified(s1.ID)
	got, _ := store.Get(s1.ID)
	if !got.Verified {
		t.Error("MarkVerified did not flip Verified flag")
	}

	// TTL expiry.
	time.Sleep(150 * time.Millisecond)
	if _, ok := store.Get(s1.ID); ok {
		t.Error("session should have expired")
	}

	// Max-size eviction.
	store.ttl = 5 * time.Second
	_, _ = store.New("SHA256:b")
	_, _ = store.New("SHA256:c")
	_, _ = store.New("SHA256:d")
	if l := store.Len(); l != 3 {
		t.Fatalf("Len = %d, want 3", l)
	}
	// One more should evict the oldest.
	_, _ = store.New("SHA256:e")
	if l := store.Len(); l != 3 {
		t.Errorf("after eviction Len = %d, want 3", l)
	}
}

// TestHTTPDiscoveryFullRoundtrip exercises hello → probe → peers
// end-to-end against an in-process server. Two ed25519 host keys
// (server's + client's) are generated; the test asserts both sides
// verify the other's signature.
func TestHTTPDiscoveryFullRoundtrip(t *testing.T) {
	serverSigner, serverPub := mustGenSigner(t)
	clientSigner, clientPub := mustGenSigner(t)
	serverID := PeerID(ssh.FingerprintSHA256(serverPub))
	clientID := PeerID(ssh.FingerprintSHA256(clientPub))

	serverSelf := PeerHello{
		PeerID:           serverID,
		AgentName:        "server-outpost",
		AssignedHostname: "server-7a3b",
		OAuth2Email:      "alice@example.com",
		Endpoints: []Endpoint{
			{Kind: EndpointLANSSH, Host: "127.0.0.1", Port: 2222},
		},
		Version: "test",
		Paired:  true,
	}
	cachedPeers := []Peer{
		{ID: "SHA256:zzz", AgentName: "another-peer", AssignedHostname: "another-9c4d"},
	}
	srv := NewServer(ServerOptions{
		Self:    serverSelf,
		Signer:  serverSigner,
		PeersFn: func() []Peer { return cachedPeers },
	})

	mux := http.NewServeMux()
	srv.Mount(mux, "")

	httpSrv := httptest.NewServer(mux)
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := NewClient()
	clientSelf := PeerHello{
		PeerID:           clientID,
		AgentName:        "client-outpost",
		AssignedHostname: "client-1b2c",
		OAuth2Email:      "alice@example.com",
		Version:          "test",
	}
	result, err := client.Probe(ctx, httpSrv.URL, clientSigner, clientSelf)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if result.Peer.ID != serverID {
		t.Errorf("peer.ID = %q, want %q", result.Peer.ID, serverID)
	}
	if result.Peer.AssignedHostname != "server-7a3b" {
		t.Errorf("AssignedHostname = %q", result.Peer.AssignedHostname)
	}
	if !result.ServerVerified {
		t.Error("ServerVerified = false; mutual auth should have succeeded")
	}
	if result.Peer.Trust != TrustTOFU {
		t.Errorf("Trust = %q, want TOFU", result.Peer.Trust)
	}

	// /peers should work on the now-verified session.
	peers, err := client.FetchPeers(ctx, httpSrv.URL, result.SessionID)
	if err != nil {
		t.Fatalf("FetchPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].AgentName != "another-peer" {
		t.Errorf("FetchPeers returned %d peers, want 1 named another-peer: %+v", len(peers), peers)
	}
}

// TestProbeRejectsWrongPubkey: the server must refuse a probe whose
// pubkey doesn't fingerprint to the PeerID claimed in /hello.
func TestProbeRejectsWrongPubkey(t *testing.T) {
	serverSigner, _ := mustGenSigner(t)
	srv := NewServer(ServerOptions{
		Self:    PeerHello{PeerID: "SHA256:server"},
		Signer:  serverSigner,
		PeersFn: nil,
	})
	mux := http.NewServeMux()
	srv.Mount(mux, "")
	httpSrv := httptest.NewServer(mux)
	defer httpSrv.Close()

	// Step 1: legitimate /hello with a real PeerID
	_, realPub := mustGenSigner(t)
	realID := PeerID(ssh.FingerprintSHA256(realPub))

	helloReq := HelloRequest{My: PeerHello{PeerID: realID, AgentName: "x"}}
	helloBody, _ := json.Marshal(helloReq)
	resp, err := http.Post(httpSrv.URL+"/api/v1/discover/hello", "application/json", strings.NewReader(string(helloBody)))
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	defer resp.Body.Close()
	var helloResp HelloResponse
	_ = json.NewDecoder(resp.Body).Decode(&helloResp)
	challenge, _ := base64.StdEncoding.DecodeString(helloResp.Challenge)

	// Step 2: probe with a DIFFERENT ed25519 keypair (attacker)
	attackerPub, attackerPriv, _ := ed25519.GenerateKey(rand.Reader)
	sig := ed25519.Sign(attackerPriv, challenge)
	probeReq := ProbeRequest{
		SessionID:        helloResp.SessionID,
		SignedChallenge:  base64.StdEncoding.EncodeToString(sig),
		YourCallerPubkey: base64.StdEncoding.EncodeToString(attackerPub),
	}
	probeBody, _ := json.Marshal(probeReq)
	resp2, err := http.Post(httpSrv.URL+"/api/v1/discover/probe", "application/json", strings.NewReader(string(probeBody)))
	if err != nil {
		t.Fatalf("probe post: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("probe status = %d, want 401 (pubkey-claim mismatch)", resp2.StatusCode)
	}
}

// TestPeersRequiresVerifiedSession: the /peers endpoint must refuse
// a session that hasn't completed /probe.
func TestPeersRequiresVerifiedSession(t *testing.T) {
	serverSigner, _ := mustGenSigner(t)
	srv := NewServer(ServerOptions{
		Self:    PeerHello{PeerID: "SHA256:server"},
		Signer:  serverSigner,
		PeersFn: func() []Peer { return nil },
	})
	mux := http.NewServeMux()
	srv.Mount(mux, "")
	httpSrv := httptest.NewServer(mux)
	defer httpSrv.Close()

	// Hello to mint a session, but skip /probe.
	_, pub := mustGenSigner(t)
	helloReq := HelloRequest{My: PeerHello{PeerID: PeerID(ssh.FingerprintSHA256(pub))}}
	body, _ := json.Marshal(helloReq)
	resp, err := http.Post(httpSrv.URL+"/api/v1/discover/hello", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("hello post: %v", err)
	}
	defer resp.Body.Close()
	var helloResp HelloResponse
	_ = json.NewDecoder(resp.Body).Decode(&helloResp)

	r, err := http.Get(httpSrv.URL + "/api/v1/discover/peers?session_id=" + helloResp.SessionID)
	if err != nil {
		t.Fatalf("get peers: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("/peers status = %d, want 403 (unverified session)", r.StatusCode)
	}
}

// mustGenSigner generates a fresh ed25519 ssh.Signer for tests.
func mustGenSigner(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 gen: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ssh signer: %v", err)
	}
	return signer, signer.PublicKey()
}
